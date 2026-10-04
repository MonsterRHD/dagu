// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package baseconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dagucloud/dagu/v2/internal/cmn/fileutil"
)

const (
	stateVersion      = 1
	stateFileName     = "generation.json"
	candidatesDirName = "candidates"
	stateFilePerm     = 0o600
	candidateFilePerm = 0o600
	recordsPrefixGlob = "*.json"
)

// generationState is the only durable record of which generation is confirmed.
// Live base config files are the source of truth; this record exists to fence
// concurrent promotions and to reconcile interrupted switches.
type generationState struct {
	Version          int               `json:"version"`
	Generation       int64             `json:"generation"`
	GlobalDigest     string            `json:"global_digest"`
	WorkspaceDigests map[string]string `json:"workspace_digests,omitempty"`
	ConfirmedAt      time.Time         `json:"confirmed_at"`
}

// candidateRecord is persisted before validation begins and removed only after
// promotion (or rejection) completes. A record whose content matches the live
// file after restart proves the rename happened but the generation record did
// not.
type candidateRecord struct {
	// Generation is the number the candidate would occupy once confirmed.
	Generation int64 `json:"generation"`
	// Workspace is empty for a global candidate.
	Workspace string `json:"workspace"`
	// Content is the candidate base config YAML.
	Content []byte `json:"content"`
	// BaseDigest is the digest of the target live file when the record began.
	BaseDigest string    `json:"base_digest"`
	CreatedAt  time.Time `json:"created_at"`
}

func (r candidateRecord) scope() Scope {
	return Scope{Workspace: r.Workspace}
}

func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func (m *Manager) statePath() string {
	return filepath.Join(m.stateDir, stateFileName)
}

func (m *Manager) candidatesDir() string {
	return filepath.Join(m.stateDir, candidatesDirName)
}

func (m *Manager) candidatePath(rec candidateRecord) string {
	return filepath.Join(m.candidatesDir(), fmt.Sprintf("%020d-%s.json", rec.Generation, rec.scope().fileStem()))
}

// loadState reads the generation record. A missing or unreadable record yields
// the zero state, because live files are the source of truth and recovery
// reconciles the record from them.
func (m *Manager) loadState() generationState {
	state := generationState{Version: stateVersion, WorkspaceDigests: map[string]string{}}
	data, err := fileutil.ReadFile(m.statePath())
	if err != nil {
		return state
	}
	if err := json.Unmarshal(data, &state); err != nil || state.Version != stateVersion {
		return generationState{Version: stateVersion, WorkspaceDigests: map[string]string{}}
	}
	if state.WorkspaceDigests == nil {
		state.WorkspaceDigests = map[string]string{}
	}
	return state
}

func (m *Manager) saveState(state generationState) error {
	state.Version = stateVersion
	return fileutil.WriteJSONAtomic(m.statePath(), state, stateFilePerm)
}

func (m *Manager) writeCandidateRecord(rec candidateRecord) error {
	if err := os.MkdirAll(m.candidatesDir(), 0o750); err != nil {
		return fmt.Errorf("create candidates directory: %w", err)
	}
	return fileutil.WriteJSONAtomic(m.candidatePath(rec), rec, candidateFilePerm)
}

func (m *Manager) removeCandidateRecord(rec candidateRecord) error {
	err := os.Remove(m.candidatePath(rec))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *generationState) digestFor(scope Scope) string {
	if scope.IsGlobal() {
		return s.GlobalDigest
	}
	return s.WorkspaceDigests[scope.Workspace]
}

func (s *generationState) setDigest(scope Scope, digest string) {
	if scope.IsGlobal() {
		s.GlobalDigest = digest
		return
	}
	s.WorkspaceDigests[scope.Workspace] = digest
}

func (m *Manager) liveStoreFor(scope Scope) (LiveStore, error) {
	if scope.IsGlobal() {
		return m.live.Global(), nil
	}
	return m.live.Workspace(scope.Workspace)
}

func (m *Manager) liveDigest(ctx context.Context, scope Scope) (string, error) {
	store, err := m.liveStoreFor(scope)
	if err != nil {
		return "", err
	}
	content, err := store.Read(ctx)
	if err != nil {
		return "", err
	}
	return digestOf(content), nil
}

// recover completes or discards interrupted switches and reconciles the
// generation record with the live files. It is safe to run on every startup.
func (m *Manager) recover(ctx context.Context) error {
	if err := os.MkdirAll(m.candidatesDir(), 0o750); err != nil {
		return fmt.Errorf("create base config state directory: %w", err)
	}

	state := m.loadState()
	stateChanged := false

	entries, err := os.ReadDir(m.candidatesDir())
	if err != nil {
		return fmt.Errorf("read candidate directory: %w", err)
	}
	records := make([]candidateRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(m.candidatesDir(), entry.Name())
		data, readErr := fileutil.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read candidate %s: %w", entry.Name(), readErr)
		}
		var rec candidateRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			// An unreadable candidate can never be promoted; discard it.
			_ = os.Remove(path)
			stateChanged = true
			continue
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Generation != records[j].Generation {
			return records[i].Generation < records[j].Generation
		}
		return records[i].Workspace < records[j].Workspace
	})

	for _, rec := range records {
		scope := rec.scope()
		targetDigest, digestErr := m.liveDigest(ctx, scope)
		if digestErr != nil {
			return fmt.Errorf("read live base config while recovering: %w", digestErr)
		}
		if digestOf(rec.Content) == targetDigest {
			// The live rename completed before the interruption; finish the
			// promotion so the confirmed record catches up.
			if rec.Generation > state.Generation {
				state.Generation = rec.Generation
				state.ConfirmedAt = m.now().UTC()
			}
			state.setDigest(scope, targetDigest)
			stateChanged = true
		}
		if err := m.removeCandidateRecord(rec); err != nil {
			return fmt.Errorf("remove processed candidate: %w", err)
		}
	}

	// Reconcile every recorded scope against the live content.
	reconciled, changed, err := m.reconcileDigests(ctx, state)
	if err != nil {
		return err
	}
	state = reconciled
	if state.ConfirmedAt.IsZero() {
		state.ConfirmedAt = m.now().UTC()
		changed = true
	}
	if changed || stateChanged || !fileutil.FileExists(m.statePath()) {
		if err := m.saveState(state); err != nil {
			return fmt.Errorf("persist reconciled base config generation: %w", err)
		}
	}
	return nil
}

// reconcileDigests rewrites recorded digests to match the live files. Live
// content is the source of truth, so an external hand-edit is adopted instead
// of blocking updates. The generation number itself is never lowered here.
func (m *Manager) reconcileDigests(ctx context.Context, state generationState) (generationState, bool, error) {
	changed := false
	liveGlobal, err := m.liveDigest(ctx, GlobalScope())
	if err != nil {
		return state, false, fmt.Errorf("read live global base config: %w", err)
	}
	if state.GlobalDigest != liveGlobal {
		state.GlobalDigest = liveGlobal
		changed = true
	}
	for workspaceName := range state.WorkspaceDigests {
		digest, digestErr := m.liveDigest(ctx, Scope{Workspace: workspaceName})
		if digestErr != nil {
			return state, false, fmt.Errorf("read live workspace base config: %w", digestErr)
		}
		if state.WorkspaceDigests[workspaceName] != digest {
			state.WorkspaceDigests[workspaceName] = digest
			changed = true
		}
	}
	return state, changed, nil
}

// adoptLiveDrift re-snapshots the generation record when live content changed
// outside a promotion (for example a hand-edited base config). Callers must
// hold the promotion lock; the pre-publish fence still blocks concurrent
// mutations during validation.
func (m *Manager) adoptLiveDrift(ctx context.Context, state generationState) (generationState, error) {
	reconciled, changed, err := m.reconcileDigests(ctx, state)
	if err != nil {
		return state, err
	}
	if changed {
		if err := m.saveState(reconciled); err != nil {
			return state, fmt.Errorf("persist reconciled base config generation: %w", err)
		}
	}
	return reconciled, nil
}
