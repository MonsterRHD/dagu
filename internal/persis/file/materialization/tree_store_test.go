// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package materialization

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dagucloud/dagu/v2/internal/build"
	"github.com/dagucloud/dagu/v2/internal/cmn/fileutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommitPublishesDirectoryAndVersionTwoManifest(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory commit test uses POSIX directory exchange")
	}

	dir := t.TempDir()
	store := New(filepath.Join(dir, "store"))
	finalPath := filepath.Join(dir, "site")
	stagingPath := filepath.Join(dir, ".site.tmp")
	require.NoError(t, os.MkdirAll(filepath.Join(stagingPath, "empty"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(stagingPath, "index.html"), []byte("new"), 0o600))
	manifest := testTreeManifest(t, "materialization", "commit", stagingPath, finalPath)
	lock, err := store.AcquirePaths(context.Background(), []build.PathLockRequest{{
		Key: build.ComparisonKey(finalPath), Mode: build.PathLockExclusive,
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })

	require.NoError(t, store.Commit(context.Background(), lock, build.MaterializationCommit{
		StagingPath: stagingPath,
		FinalPath:   finalPath,
		Manifest:    manifest,
	}))
	require.NoDirExists(t, stagingPath)
	require.FileExists(t, filepath.Join(finalPath, "index.html"))
	assert.DirExists(t, filepath.Join(finalPath, "empty"))
	stored, err := store.Get(context.Background(), manifest.MaterializationKey)
	require.NoError(t, err)
	require.Equal(t, build.MaterializationSchemaVersion, stored.SchemaVersion)
	assert.Equal(t, build.SnapshotTypeDirectory, stored.Output.Type)
	assert.NotEmpty(t, stored.Output.Entries)
}

func TestCommitPublishesDirectoryWithFinalAbsoluteSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory commit test uses POSIX symlinks")
	}

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	store := New(filepath.Join(dir, "store"))
	finalPath := filepath.Join(dir, "site")
	stagingPath := filepath.Join(dir, ".site.tmp")
	require.NoError(t, os.MkdirAll(stagingPath, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(stagingPath, "index.html"), []byte("new"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(finalPath, "index.html"), filepath.Join(stagingPath, "link.html")))
	output, err := build.SnapshotForPublication("site", stagingPath, finalPath)
	require.NoError(t, err)
	output.Path = finalPath
	manifest := build.Materialization{
		SchemaVersion:      build.MaterializationSchemaVersion,
		MaterializationKey: "materialization",
		CommitID:           "commit",
		Output:             output,
	}
	lock, err := store.AcquirePaths(context.Background(), []build.PathLockRequest{{
		Key: build.ComparisonKey(finalPath), Mode: build.PathLockExclusive,
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })
	require.NoError(t, store.Commit(context.Background(), lock, build.MaterializationCommit{
		StagingPath: stagingPath,
		FinalPath:   finalPath,
		Manifest:    manifest,
	}))
	resolved, err := filepath.EvalSymlinks(filepath.Join(finalPath, "link.html"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(finalPath, "index.html"), resolved)
}

func TestCommitRollbackPreservesOldTreeWithAbsoluteSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory rollback test uses POSIX symlinks")
	}

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	store := New(filepath.Join(dir, "store"))
	finalPath := filepath.Join(dir, "site")
	oldStaging := filepath.Join(dir, ".old.tmp")
	newStaging := filepath.Join(dir, ".new.tmp")
	require.NoError(t, os.MkdirAll(oldStaging, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(oldStaging, "keep.html"), []byte("old"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(finalPath, "keep.html"), filepath.Join(oldStaging, "link.html")))
	require.NoError(t, os.Rename(oldStaging, finalPath))
	require.NoError(t, os.MkdirAll(newStaging, 0o750))
	require.NoError(t, os.Symlink(filepath.Join(finalPath, "missing.html"), filepath.Join(newStaging, "link.html")))
	proposed, err := build.SnapshotForPublication("site", newStaging, finalPath)
	require.NoError(t, err)
	proposed.Path = finalPath
	manifest := build.Materialization{
		SchemaVersion:      build.MaterializationSchemaVersion,
		MaterializationKey: "materialization",
		CommitID:           "broken",
		Output:             proposed,
	}
	lock, err := store.AcquirePaths(context.Background(), []build.PathLockRequest{{Key: finalPath, Mode: build.PathLockExclusive}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })
	err = store.Commit(context.Background(), lock, build.MaterializationCommit{
		StagingPath: newStaging,
		FinalPath:   finalPath,
		Manifest:    manifest,
	})
	require.Error(t, err)
	t.Logf("rollback error: %v", err)
	require.FileExists(t, filepath.Join(finalPath, "keep.html"))
	resolved, err := filepath.EvalSymlinks(filepath.Join(finalPath, "link.html"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(finalPath, "keep.html"), resolved)
	require.NoDirExists(t, newStaging)
	require.NoFileExists(t, store.journalPath(build.ComparisonKey(finalPath)))
}

func TestFailedFirstDirectoryCommitCanBeRetriedWithBrokenSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory rollback test uses POSIX symlinks")
	}

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	store := New(filepath.Join(dir, "store"))
	finalPath := filepath.Join(dir, "site")
	badStaging := filepath.Join(dir, ".bad.tmp")
	require.NoError(t, os.MkdirAll(badStaging, 0o750))
	require.NoError(t, os.Symlink(filepath.Join(finalPath, "missing.html"), filepath.Join(badStaging, "link.html")))
	badOutput, err := build.SnapshotForPublication("site", badStaging, finalPath)
	require.NoError(t, err)
	badOutput.Path = finalPath
	lock, err := store.AcquirePaths(context.Background(), []build.PathLockRequest{{Key: finalPath, Mode: build.PathLockExclusive}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })
	err = store.Commit(context.Background(), lock, build.MaterializationCommit{
		StagingPath: badStaging,
		FinalPath:   finalPath,
		Manifest:    build.Materialization{CommitID: "bad", Output: badOutput},
	})
	require.Error(t, err)
	t.Logf("first rollback error: %v", err)
	require.NoDirExists(t, finalPath)
	require.NoDirExists(t, badStaging)

	goodStaging := filepath.Join(dir, ".good.tmp")
	require.NoError(t, os.MkdirAll(goodStaging, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(goodStaging, "index.html"), []byte("good"), 0o600))
	goodOutput, err := build.SnapshotForPublication("site", goodStaging, finalPath)
	require.NoError(t, err)
	goodOutput.Path = finalPath
	require.NoError(t, store.Commit(context.Background(), lock, build.MaterializationCommit{
		StagingPath: goodStaging,
		FinalPath:   finalPath,
		Manifest: build.Materialization{
			SchemaVersion:      build.MaterializationSchemaVersion,
			MaterializationKey: "materialization",
			CommitID:           "good",
			Output:             goodOutput,
		},
	}))
	require.FileExists(t, filepath.Join(finalPath, "index.html"))
}

func TestRecoverDirectoryExchangeCommit(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory recovery test uses POSIX directory exchange")
	}

	tests := []struct {
		name         string
		manifest     string
		final        string
		staging      string
		backup       string
		mode         string
		wantFinal    string
		wantStaging  bool
		wantManifest string
	}{
		{name: "completes exchanged tree", manifest: "proposed", final: "proposed", staging: "previous", mode: commitModeExchange, wantFinal: "proposed", wantManifest: "proposed"},
		{name: "rolls exchanged tree back", manifest: "previous", final: "proposed", staging: "previous", mode: commitModeExchange, wantFinal: "previous", wantManifest: "previous"},
		{name: "removes staged tree before exchange", manifest: "previous", final: "previous", staging: "proposed", mode: commitModeExchange, wantFinal: "previous", wantManifest: "previous"},
		{name: "removes first tree before manifest", manifest: "missing", final: "proposed", mode: commitModeExchange, wantFinal: "missing", wantManifest: "missing"},
		{name: "restores backup tree", manifest: "previous", final: "proposed", backup: "previous", mode: commitModeBackup, wantFinal: "previous", wantManifest: "previous"},
		{name: "restores backup tree while final is absent", manifest: "previous", final: "missing", staging: "proposed", backup: "previous", mode: commitModeBackup, wantFinal: "previous", wantManifest: "previous"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store := New(filepath.Join(dir, "store"))
			require.NoError(t, store.ensureDirs())
			finalPath := filepath.Join(dir, "site")
			stagingPath := filepath.Join(dir, ".site.tmp")
			backupPath := filepath.Join(dir, ".site.backup")
			manifestPath := store.manifestPath("materialization")

			previousSource := filepath.Join(dir, "previous-source")
			proposedSource := filepath.Join(dir, "proposed-source")
			requireWriteTree(t, previousSource, "known-good")
			requireWriteTree(t, proposedSource, "proposed")
			previous := testTreeSnapshot(t, previousSource, finalPath)
			proposed := testTreeSnapshot(t, proposedSource, finalPath)

			moveTree(t, previousSource, tt.final == "previous", finalPath)
			moveTree(t, proposedSource, tt.final == "proposed", finalPath)
			moveTree(t, previousSource, tt.staging == "previous" && tt.final != "previous", stagingPath)
			moveTree(t, proposedSource, tt.staging == "proposed" && tt.final != "proposed", stagingPath)
			moveTree(t, previousSource, tt.backup == "previous", backupPath)

			previousManifest := json.RawMessage(`{"commitId":"previous"}`)
			switch tt.manifest {
			case "previous":
				require.NoError(t, fileutil.WriteFileAtomic(manifestPath, previousManifest, fileMode))
			case "proposed":
				proposedManifest := build.Materialization{SchemaVersion: build.MaterializationSchemaVersion, CommitID: "proposed", Output: proposed}
				require.NoError(t, fileutil.WriteJSONAtomic(manifestPath, proposedManifest, fileMode))
			}

			journal := commitJournal{
				FinalPath:        finalPath,
				StagingPath:      stagingPath,
				BackupPath:       backupPath,
				ManifestPath:     manifestPath,
				Mode:             tt.mode,
				PreviousFinal:    &previous,
				Proposed:         build.Materialization{CommitID: "proposed", Output: proposed},
				PreviousManifest: previousManifest,
			}
			if tt.name == "removes first tree before manifest" {
				journal.PreviousFinal = nil
				journal.PreviousManifest = nil
			}
			require.NoError(t, fileutil.WriteJSONAtomic(store.journalPath(build.ComparisonKey(finalPath)), journal, fileMode))
			require.NoError(t, store.recover(build.ComparisonKey(finalPath)))

			switch tt.wantFinal {
			case "proposed":
				require.FileExists(t, filepath.Join(finalPath, "index.html"))
			case "previous":
				require.FileExists(t, filepath.Join(finalPath, "old.html"))
			default:
				require.NoFileExists(t, finalPath)
			}
			require.NoFileExists(t, stagingPath)
			require.NoFileExists(t, backupPath)
			require.NoFileExists(t, store.journalPath(build.ComparisonKey(finalPath)))
			switch tt.wantManifest {
			case "proposed":
				stored, err := store.Get(context.Background(), "materialization")
				require.NoError(t, err)
				assert.Equal(t, "proposed", stored.CommitID)
			case "previous":
				data, err := os.ReadFile(manifestPath)
				require.NoError(t, err)
				require.JSONEq(t, string(previousManifest), string(data))
			default:
				require.NoFileExists(t, manifestPath)
			}
		})
	}
}

func TestAcquirePathsRecoversCommittedDirectoryJournal(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory recovery test uses POSIX directory exchange")
	}

	dir := t.TempDir()
	store := New(filepath.Join(dir, "store"))
	require.NoError(t, store.ensureDirs())
	finalPath := filepath.Join(dir, "site")
	stagingPath := filepath.Join(dir, ".site.tmp")
	manifestPath := store.manifestPath("materialization")
	previousSource := filepath.Join(dir, "previous-source")
	proposedSource := filepath.Join(dir, "proposed-source")
	requireWriteTree(t, previousSource, "known-good")
	requireWriteTree(t, proposedSource, "proposed")
	previous := testTreeSnapshot(t, previousSource, finalPath)
	proposed := testTreeSnapshot(t, proposedSource, finalPath)
	require.NoError(t, os.Rename(proposedSource, finalPath))
	require.NoError(t, os.Rename(previousSource, stagingPath))
	manifest := build.Materialization{SchemaVersion: build.MaterializationSchemaVersion, CommitID: "proposed", Output: proposed}
	require.NoError(t, fileutil.WriteJSONAtomic(manifestPath, manifest, fileMode))
	journal := commitJournal{
		FinalPath: finalPath,
		StagingPath: stagingPath,
		ManifestPath: manifestPath,
		Mode: commitModeExchange,
		PreviousFinal: &previous,
		Proposed: manifest,
	}
	require.NoError(t, fileutil.WriteJSONAtomic(store.journalPath(build.ComparisonKey(finalPath)), journal, fileMode))

	lock, err := store.AcquirePaths(context.Background(), []build.PathLockRequest{{Key: finalPath, Mode: build.PathLockExclusive}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })
	require.FileExists(t, filepath.Join(finalPath, "index.html"))
	require.NoDirExists(t, stagingPath)
	require.NoFileExists(t, store.journalPath(build.ComparisonKey(finalPath)))
}

func TestAcquirePathsExpandsDirectoryLocksAndCleansStaging(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX locks and directory modes are required")
	}

	dir := t.TempDir()
	store := New(filepath.Join(dir, "store"))
	root := filepath.Join(dir, "site")
	child := filepath.Join(root, "index.html")
	require.NoError(t, os.MkdirAll(root, 0o750))
	require.NoError(t, os.WriteFile(child, []byte("current"), 0o600))

	expanded, err := expandRequests(normalizeRequests([]build.PathLockRequest{{Key: root, Mode: build.PathLockShared}}))
	require.NoError(t, err)
	require.Contains(t, expanded, build.PathLockRequest{Key: build.ComparisonKey(child), Mode: build.PathLockShared})
	require.NotEqual(t, store.lockPath(build.ComparisonKey(root)), store.lockPath(build.ComparisonKey(child)))
	var reader *heldLock
	readerLock, err := store.AcquirePaths(context.Background(), []build.PathLockRequest{{Key: root, Mode: build.PathLockShared}})
	require.NoError(t, err)
	reader = readerLock.(*heldLock)
	require.Len(t, reader.locks, len(expanded))
	blocked, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err = store.AcquirePaths(blocked, []build.PathLockRequest{{Key: child, Mode: build.PathLockExclusive}})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, reader.Release())

	staleFile := filepath.Join(dir, ".site.dagu-old-0-abc.tmp")
	staleDir := filepath.Join(dir, ".site.dagu-old-1-def.tmp")
	require.NoError(t, os.WriteFile(staleFile, []byte("stale"), 0o600))
	require.NoError(t, os.Mkdir(staleDir, 0o700))
	unrelated := filepath.Join(dir, ".other.dagu-keep.tmp")
	require.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0o600))
	writer, err := store.AcquirePaths(context.Background(), []build.PathLockRequest{{Key: root, Mode: build.PathLockExclusive}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Release()) })
	require.NoFileExists(t, staleFile)
	require.NoDirExists(t, staleDir)
	require.FileExists(t, unrelated)
}

func TestGetAcceptsVersionOneFileManifest(t *testing.T) {
	t.Parallel()

	store := New(t.TempDir())
	require.NoError(t, store.ensureDirs())
	manifest := build.Materialization{SchemaVersion: 1, MaterializationKey: "materialization", Output: build.FileSnapshot{
		Path: "output.txt", Size: 3, Digest: "sha256:abc",
	}}
	require.NoError(t, fileutil.WriteJSONAtomic(store.manifestPath(manifest.MaterializationKey), manifest, fileMode))
	stored, err := store.Get(context.Background(), manifest.MaterializationKey)
	require.NoError(t, err)
	assert.Equal(t, build.SnapshotTypeFile, stored.Output.Type)
}

func requireWriteTree(t *testing.T, root, marker string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o750))
	name := "index.html"
	if marker == "known-good" {
		name = "old.html"
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(marker), 0o600))
}

func moveTree(t *testing.T, source string, shouldMove bool, target string) {
	t.Helper()
	if !shouldMove {
		return
	}
	require.NoError(t, os.Rename(source, target))
}

func testTreeSnapshot(t *testing.T, source, finalPath string) build.FileSnapshot {
	t.Helper()
	snapshot, err := build.Snapshot("site", source)
	require.NoError(t, err)
	snapshot.Path = finalPath
	return snapshot
}

func testTreeManifest(t *testing.T, key, commitID, sourcePath, finalPath string) build.Materialization {
	t.Helper()
	return build.Materialization{
		SchemaVersion:      build.MaterializationSchemaVersion,
		MaterializationKey: key,
		CommitID:           commitID,
		Output:             testTreeSnapshot(t, sourcePath, finalPath),
	}
}
