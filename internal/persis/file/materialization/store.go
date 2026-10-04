// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package materialization

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dagucloud/dagu/v2/internal/build"
	"github.com/dagucloud/dagu/v2/internal/cmn/fileutil"
	"github.com/gofrs/flock"
)

const (
	fileMode           = 0o600
	commitModeExchange = "exchange"
	commitModeBackup   = "backup"
)

// Store is a local file-backed materialization store.
type Store struct {
	root string
}

// New creates a materialization store rooted at dir.
func New(dir string) *Store {
	return &Store{root: dir}
}

type heldLock struct {
	store    *Store
	locks    []*flock.Flock
	requests []build.PathLockRequest
	released bool
}

func (l *heldLock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	var result error
	for _, v := range slices.Backward(l.locks) {
		result = errors.Join(result, v.Unlock())
	}
	return result
}

// Get returns one committed manifest.
func (s *Store) Get(_ context.Context, key string) (*build.Materialization, error) {
	data, err := os.ReadFile(s.manifestPath(key)) //nolint:gosec // key is generated internally
	if errors.Is(err, os.ErrNotExist) {
		return nil, build.ErrMaterializationNotFound
	}
	if err != nil {
		return nil, err
	}
	var manifest build.Materialization
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode materialization manifest: %w", err)
	}
	if manifest.SchemaVersion != build.MaterializationSchemaVersion && manifest.SchemaVersion != 1 {
		return nil, fmt.Errorf("%w: unsupported materialization schema version %d", build.ErrMaterializationNotFound, manifest.SchemaVersion)
	}
	build.NormalizeSnapshot(&manifest.Output)
	for idx := range manifest.Inputs {
		build.NormalizeSnapshot(&manifest.Inputs[idx])
	}
	return &manifest, nil
}

// AcquirePaths acquires path locks in stable order and recovers incomplete commits.
func (s *Store) AcquirePaths(ctx context.Context, requests []build.PathLockRequest) (build.MaterializationLock, error) {
	if s == nil || s.root == "" {
		return nil, fmt.Errorf("materialization store is unavailable")
	}
	if err := s.ensureDirs(); err != nil {
		return nil, err
	}
	baseRequests := normalizeRequests(requests)
	requests, err := expandRequests(baseRequests)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		held := &heldLock{store: s, requests: requests}
		restart := false
		for _, request := range requests {
			lock := flock.New(s.lockPath(request.Key))
			locked, err := acquire(ctx, lock, request.Mode)
			if err != nil || !locked {
				_ = held.Release()
				if err != nil {
					return nil, fmt.Errorf("acquire materialization path lock: %w", err)
				}
				return nil, fmt.Errorf("materialization path lock was not acquired")
			}
			held.locks = append(held.locks, lock)

			if request.Mode != build.PathLockShared {
				continue
			}
			if _, err := os.Stat(s.journalPath(request.Key)); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				_ = held.Release()
				return nil, err
			}

			_ = held.Release()
			recoveryLock, recoveryErr := s.AcquirePaths(ctx, []build.PathLockRequest{{
				Key: request.Key, Mode: build.PathLockExclusive,
			}})
			if recoveryErr != nil {
				return nil, fmt.Errorf("%w: %w", build.ErrMaterializationRecovery, recoveryErr)
			}
			if err := recoveryLock.Release(); err != nil {
				return nil, err
			}
			restart = true
			break
		}
		if !restart {
			current, err := expandRequests(baseRequests)
			if err != nil {
				_ = held.Release()
				return nil, err
			}
			if !sameRequests(current, requests) {
				_ = held.Release()
				requests = current
				continue
			}
			recovered := false
			for _, request := range baseRequests {
				if request.Mode != build.PathLockExclusive {
					continue
				}
				if _, err := os.Stat(s.journalPath(request.Key)); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					_ = held.Release()
					return nil, err
				}
				if err := s.recover(request.Key); err != nil {
					_ = held.Release()
					return nil, fmt.Errorf("%w: %w", build.ErrMaterializationRecovery, err)
				}
				recovered = true
			}
			if recovered {
				_ = held.Release()
				requests, err = expandRequests(baseRequests)
				if err != nil {
					return nil, err
				}
				continue
			}
			for _, request := range baseRequests {
				if request.Mode != build.PathLockExclusive {
					continue
				}
				if err := s.cleanStagingSiblings(request.Key); err != nil {
					_ = held.Release()
					return nil, err
				}
			}
			return held, nil
		}
	}
}

func acquire(ctx context.Context, lock *flock.Flock, mode build.PathLockMode) (bool, error) {
	if mode == build.PathLockShared {
		return lock.TryRLockContext(ctx, 25*time.Millisecond)
	}
	return lock.TryLockContext(ctx, 25*time.Millisecond)
}

// Commit publishes the staged file and manifest with rollback on failure.
func (s *Store) Commit(_ context.Context, lock build.MaterializationLock, req build.MaterializationCommit) error {
	held, ok := lock.(*heldLock)
	if !ok || held.store != s || held.released {
		return fmt.Errorf("invalid materialization lock")
	}
	outputKey := build.ComparisonKey(req.FinalPath)
	hasOutputLock := false
	for _, request := range held.requests {
		if request.Mode == build.PathLockExclusive && request.Key == outputKey {
			hasOutputLock = true
			break
		}
	}
	if !hasOutputLock {
		return fmt.Errorf("materialization commit requires an exclusive lock for the final output")
	}
	finalPath, err := build.ResolvePath(req.FinalPath, "", true)
	if err != nil {
		return err
	}
	stagingPath, err := build.ResolvePath(req.StagingPath, "", false)
	if err != nil {
		return err
	}
	if filepath.Dir(stagingPath) != filepath.Dir(finalPath) {
		return fmt.Errorf("staging and final output must be on the same filesystem directory")
	}
	req.FinalPath = finalPath
	req.StagingPath = stagingPath
	if err := verifyStagingOutput(req); err != nil {
		return err
	}

	manifestPath := s.manifestPath(req.Manifest.MaterializationKey)
	previousManifest, previousManifestErr := os.ReadFile(manifestPath) //nolint:gosec
	if previousManifestErr != nil && !errors.Is(previousManifestErr, os.ErrNotExist) {
		return previousManifestErr
	}
	previousFinal, err := snapshotExisting(req.FinalPath)
	if err != nil {
		return err
	}

	journalPath := s.journalPath(outputKey)
	backupPath := filepath.Join(filepath.Dir(req.FinalPath), ".dagu-backup-"+digestName(req.FinalPath+"\x00"+req.Manifest.CommitID))
	treeCommit := req.Manifest.Output.Type == build.SnapshotTypeDirectory ||
		(previousFinal != nil && previousFinal.Type == build.SnapshotTypeDirectory)
	journal := commitJournal{
		FinalPath:        req.FinalPath,
		StagingPath:      req.StagingPath,
		BackupPath:       backupPath,
		ManifestPath:     manifestPath,
		PreviousFinal:    previousFinal,
		PreviousManifest: previousManifest,
		Proposed:         req.Manifest,
		PreserveManifest: req.PreserveManifest,
		Mode:             commitModeBackup,
	}
	if treeCommit {
		journal.Mode = commitModeExchange
	}
	if err := fileutil.WriteJSONAtomic(journalPath, journal, fileMode); err != nil {
		return fmt.Errorf("write materialization journal: %w", err)
	}

	rollback := func(cause error) error {
		if restoreErr := restorePrevious(journal, true); restoreErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback materialization: %w", restoreErr))
		}
		_ = fileutil.RemoveFileDurable(journalPath)
		return cause
	}
	if treeCommit {
		if err := publishTree(&journalPath, &journal); err != nil {
			return rollback(fmt.Errorf("replace materialized output tree: %w", err))
		}
	} else {
		if previousFinal != nil {
			if err := preserveFile(req.FinalPath, backupPath); err != nil {
				_ = fileutil.RemoveFileDurable(journalPath)
				return fmt.Errorf("preserve previous materialization: %w", err)
			}
		}
		if err := fileutil.ReplaceFileDurable(req.StagingPath, req.FinalPath); err != nil {
			return rollback(fmt.Errorf("replace materialized output: %w", err))
		}
	}
	if err := verifyFile(req.FinalPath, req.Manifest.Output); err != nil {
		return rollback(fmt.Errorf("verify published materialized output: %w", err))
	}
	if !req.PreserveManifest {
		if err := fileutil.WriteJSONAtomic(manifestPath, req.Manifest, fileMode); err != nil {
			return rollback(fmt.Errorf("write materialization manifest: %w", err))
		}
	}
	cleanupJournalArtifacts(journal)
	_ = fileutil.RemoveFileDurable(journalPath)
	return nil
}

type commitJournal struct {
	FinalPath        string                `json:"finalPath"`
	StagingPath      string                `json:"stagingPath,omitempty"`
	BackupPath       string                `json:"backupPath"`
	ManifestPath     string                `json:"manifestPath"`
	Mode             string                `json:"mode,omitempty"`
	PreviousFinal    *build.FileSnapshot   `json:"previousFinal,omitempty"`
	PreviousManifest json.RawMessage       `json:"previousManifest,omitempty"`
	Proposed         build.Materialization `json:"proposed"`
	PreserveManifest bool                  `json:"preserveManifest,omitempty"`
}

func (s *Store) recover(pathKey string) error {
	journalPath := s.journalPath(pathKey)
	data, err := os.ReadFile(journalPath) //nolint:gosec
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal commitJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return fmt.Errorf("recover materialization: invalid journal: %w", err)
	}
	manifestData, manifestErr := os.ReadFile(journal.ManifestPath) //nolint:gosec
	manifestCommitted := journal.PreserveManifest && preservedManifestMatches(manifestData, manifestErr, journal.PreviousManifest)
	if !journal.PreserveManifest {
		var current build.Materialization
		manifestCommitted = manifestErr == nil && json.Unmarshal(manifestData, &current) == nil && current.CommitID == journal.Proposed.CommitID
	}
	if manifestCommitted && verifyFile(journal.FinalPath, journal.Proposed.Output) == nil {
		cleanupJournalArtifacts(journal)
		return fileutil.RemoveFileDurable(journalPath)
	}
	if err := restorePrevious(journal, false); err != nil {
		recoveryErr := fmt.Errorf("recover materialization: %w", err)
		if removeErr := fileutil.RemoveFileDurable(journalPath); removeErr != nil {
			return errors.Join(recoveryErr, fmt.Errorf("remove unrecoverable materialization journal: %w", removeErr))
		}
		return recoveryErr
	}
	_ = fileutil.RemoveFileDurable(journalPath)
	return nil
}

func preservedManifestMatches(current []byte, currentErr error, previous json.RawMessage) bool {
	if len(previous) == 0 {
		return errors.Is(currentErr, os.ErrNotExist)
	}
	if currentErr != nil {
		return false
	}
	var currentCompact, previousCompact bytes.Buffer
	if json.Compact(&currentCompact, current) != nil || json.Compact(&previousCompact, previous) != nil {
		return false
	}
	return bytes.Equal(currentCompact.Bytes(), previousCompact.Bytes())
}

func expandRequests(base []build.PathLockRequest) ([]build.PathLockRequest, error) {
	byKey := make(map[string]build.PathLockMode, len(base))
	add := func(key string, mode build.PathLockMode) {
		if key == "" {
			return
		}
		if byKey[key] != build.PathLockExclusive {
			byKey[key] = mode
		}
	}
	for _, request := range base {
		add(request.Key, request.Mode)
		info, err := os.Lstat(request.Key)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			continue
		}
		err = filepath.WalkDir(request.Key, func(path string, _ os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path != request.Key {
				add(build.ComparisonKey(path), request.Mode)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	result := make([]build.PathLockRequest, 0, len(byKey))
	for key, mode := range byKey {
		result = append(result, build.PathLockRequest{Key: key, Mode: mode})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}

func sameRequests(left, right []build.PathLockRequest) bool {
	if len(left) != len(right) {
		return false
	}
	for idx := range left {
		if left[idx] != right[idx] {
			return false
		}
	}
	return true
}

func (s *Store) cleanStagingSiblings(finalPath string) error {
	if finalPath == "" || filepath.Dir(finalPath) == finalPath {
		return nil
	}
	dir := filepath.Dir(finalPath)
	base := filepath.Base(finalPath)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "."+base+".dagu-") && strings.HasSuffix(name, ".tmp") {
			if err := removeAllDurable(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func normalizeRequests(requests []build.PathLockRequest) []build.PathLockRequest {
	byKey := make(map[string]build.PathLockMode, len(requests))
	for _, request := range requests {
		key := build.ComparisonKey(request.Key)
		if key == "" {
			continue
		}
		mode := byKey[key]
		if mode == "" || request.Mode == build.PathLockExclusive {
			byKey[key] = request.Mode
		}
	}
	result := make([]build.PathLockRequest, 0, len(byKey))
	for key, mode := range byKey {
		result = append(result, build.PathLockRequest{Key: key, Mode: mode})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

func (s *Store) ensureDirs() error {
	for _, dir := range []string{filepath.Join(s.root, "manifests"), filepath.Join(s.root, "locks"), filepath.Join(s.root, "journals")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) manifestPath(key string) string {
	return filepath.Join(s.root, "manifests", key+".json")
}
func (s *Store) lockPath(key string) string {
	return filepath.Join(s.root, "locks", digestName(key)+".lock")
}
func (s *Store) journalPath(key string) string {
	return filepath.Join(s.root, "journals", digestName(key)+".json")
}

func digestName(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func snapshotExisting(path string) (*build.FileSnapshot, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snapshot, err := snapshotFile(path)
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func snapshotFile(path string) (build.FileSnapshot, error) {
	return build.Snapshot("", path)
}

func verifyStagingOutput(req build.MaterializationCommit) error {
	got, err := build.SnapshotForPublication(req.Manifest.Output.Name, req.StagingPath, req.FinalPath)
	if err != nil {
		return err
	}
	got.Path = req.Manifest.Output.Path
	got.Name = req.Manifest.Output.Name
	if !build.SnapshotsEqual(got, req.Manifest.Output) {
		return fmt.Errorf("staged materialized content changed for %s", req.StagingPath)
	}
	return nil
}

func verifyFile(path string, expected build.FileSnapshot) error {
	got, err := snapshotFile(path)
	if err != nil {
		return err
	}
	got.Path = expected.Path
	got.Name = expected.Name
	if !build.SnapshotsEqual(got, expected) {
		return fmt.Errorf("materialized content changed for %s", path)
	}
	return nil
}

func preserveFile(source, target string) error {
	if err := os.Link(source, target); err == nil {
		return fileutil.SyncDir(filepath.Dir(target))
	}
	in, err := os.Open(source) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode) //nolint:gosec
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = fileutil.RemoveFileDurable(target)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = fileutil.RemoveFileDurable(target)
		return err
	}
	if err := out.Close(); err != nil {
		_ = fileutil.RemoveFileDurable(target)
		return err
	}
	return fileutil.SyncDir(filepath.Dir(target))
}
