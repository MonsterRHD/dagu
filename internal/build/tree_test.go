// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package build_test

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/dagucloud/dagu/v2/internal/build"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotDirectoryIsStableAndComplete(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("tree test uses POSIX symlinks")
	}

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "nested"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "a.txt"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "b.txt"), []byte("b"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "target.txt"), []byte("target"), 0o600))
	require.NoError(t, os.Symlink("target.txt", filepath.Join(root, "link.txt")))

	first, err := build.Snapshot("site", root)
	require.NoError(t, err)
	second, err := build.Snapshot("site", root)
	require.NoError(t, err)
	assert.Equal(t, build.SnapshotTypeDirectory, first.Type)
	assert.Equal(t, first, second)
	assert.Equal(t, first.Digest, first.RootDigest)

	paths := make([]string, 0, len(first.Entries))
	types := make(map[string]string, len(first.Entries))
	for _, entry := range first.Entries {
		paths = append(paths, entry.Path)
		types[entry.Path] = entry.Type
	}
	assert.Equal(t, []string{".", "empty", "link.txt", "nested", "nested/a.txt", "nested/b.txt", "target.txt"}, paths)
	assert.Equal(t, build.SnapshotTypeSymlink, types["link.txt"])
}

func TestSnapshotForPublicationValidatesFinalSymlinkRoot(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink validation uses POSIX links")
	}

	finalRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	finalRoot = filepath.Join(finalRoot, "site")
	stagingRoot := filepath.Join(filepath.Dir(finalRoot), ".site.tmp")
	require.NoError(t, os.Mkdir(stagingRoot, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(stagingRoot, "index.html"), []byte("index"), 0o600))

	require.NoError(t, os.Symlink(filepath.Join(stagingRoot, "index.html"), filepath.Join(stagingRoot, "staging-link.html")))
	_, err = build.SnapshotForPublication("site", stagingRoot, finalRoot)
	require.ErrorContains(t, err, "escapes materialization root")
	require.NoError(t, os.Remove(filepath.Join(stagingRoot, "staging-link.html")))

	require.NoError(t, os.Symlink(filepath.Join(finalRoot, "index.html"), filepath.Join(stagingRoot, "final-link.html")))
	snapshot, err := build.SnapshotForPublication("site", stagingRoot, finalRoot)
	require.NoError(t, err)
	assert.Equal(t, build.SnapshotTypeDirectory, snapshot.Type)
}

func TestSnapshotDirectoryDetectsTreeChanges(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("tree test uses POSIX symlinks")
	}

	for _, tt := range []struct {
		name  string
		apply func(string) error
	}{
		{"content", func(root string) error {
			return os.WriteFile(filepath.Join(root, "a.txt"), []byte("changed"), 0o600)
		}},
		{"relative path", func(root string) error {
			return os.Rename(filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt"))
		}},
		{"empty directory removed", func(root string) error {
			return os.Remove(filepath.Join(root, "empty"))
		}},
		{"type changed", func(root string) error {
			if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(root, "a.txt"), 0o750)
		}},
		{"symlink target changed", func(root string) error {
			if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("other"), 0o600); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(root, "link.txt")); err != nil {
				return err
			}
			return os.Symlink("other.txt", filepath.Join(root, "link.txt"))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o600))
			if tt.name == "symlink target changed" {
				require.NoError(t, os.Symlink("a.txt", filepath.Join(root, "link.txt")))
			}
			before, err := build.Snapshot("site", root)
			require.NoError(t, err)
			require.NoError(t, tt.apply(root))
			after, err := build.Snapshot("site", root)
			require.NoError(t, err)
			assert.NotEqual(t, before.Digest, after.Digest)
		})
	}
}

func TestSnapshotDirectoryRejectsUnsafeSymlinksAndSpecialFiles(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("tree test uses POSIX symlinks and Unix sockets")
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))

	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{"absolute escape", func(t *testing.T, root string) {
			require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
		}},
		{"relative escape", func(t *testing.T, root string) {
			require.NoError(t, os.Symlink("../../../outside.txt", filepath.Join(root, "escape")))
		}},
		{"chained escape", func(t *testing.T, root string) {
			require.NoError(t, os.Mkdir(filepath.Join(root, "sub"), 0o750))
			require.NoError(t, os.Symlink(outside, filepath.Join(root, "outside-link")))
			require.NoError(t, os.Symlink("../outside-link", filepath.Join(root, "sub", "escape")))
		}},
		{"symlink cycle", func(t *testing.T, root string) {
			require.NoError(t, os.Symlink("b", filepath.Join(root, "a")))
			require.NoError(t, os.Symlink("a", filepath.Join(root, "b")))
		}},
		{"named pipe", func(t *testing.T, root string) {
			require.NoError(t, syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "root")
			require.NoError(t, os.Mkdir(root, 0o750))
			tt.setup(t, root)
			_, err := build.Snapshot("site", root)
			require.Error(t, err)
		})
	}
}
