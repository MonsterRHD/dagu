// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package build

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Snapshot hashes a stable file or directory snapshot.
func Snapshot(name, path string) (FileSnapshot, error) {
	for range 3 {
		info, err := os.Lstat(path)
		if err != nil {
			return FileSnapshot{}, err
		}
		switch {
		case info.Mode().IsRegular():
			digestValue, size, err := hashRegularFileAt(path, info)
			if err != nil {
				return FileSnapshot{}, err
			}
			return FileSnapshot{
				Name: name, Path: path, Type: SnapshotTypeFile, Size: size, Digest: digestValue,
			}, nil
		case info.IsDir():
			snapshot, err := snapshotDirectory(name, path, path)
			if err == nil {
				return snapshot, nil
			}
			if !errors.Is(err, errPathChanged) {
				return FileSnapshot{}, err
			}
		default:
			return FileSnapshot{}, fmt.Errorf("%s is not a regular file or non-symlink directory", path)
		}
	}
	return FileSnapshot{}, fmt.Errorf("%w: %s", errPathChanged, path)
}

// SnapshotForPublication validates staging content while enforcing the final root boundary.
func SnapshotForPublication(name, stagingPath, finalPath string) (FileSnapshot, error) {
	info, err := os.Lstat(stagingPath)
	if err != nil {
		return FileSnapshot{}, err
	}
	if info.IsDir() {
		return snapshotDirectory(name, stagingPath, finalPath)
	}
	if info.Mode().IsRegular() {
		return Snapshot(name, stagingPath)
	}
	return FileSnapshot{}, fmt.Errorf("%s is not a regular file or directory", stagingPath)
}

func snapshotsEqual(left, right []FileSnapshot) bool {
	if len(left) != len(right) {
		return false
	}
	for idx := range left {
		if !snapshotEqual(left[idx], right[idx]) {
			return false
		}
	}
	return true
}

func SnapshotsEqual(left, right FileSnapshot) bool {
	return snapshotEqual(left, right)
}

func snapshotEqual(left, right FileSnapshot) bool {
	NormalizeSnapshot(&left)
	NormalizeSnapshot(&right)
	return left.Name == right.Name &&
		left.Path == right.Path &&
		left.Type == right.Type &&
		left.Size == right.Size &&
		left.Digest == right.Digest &&
		left.RootDigest == right.RootDigest &&
		entriesEqual(left.Entries, right.Entries)
}

// ResolvePath returns an absolute path with existing ancestors resolved.
func ResolvePath(raw, base string, output bool) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("materialization path is empty")
	}
	path := raw
	if !filepath.IsAbs(path) {
		if base == "" {
			return "", fmt.Errorf("relative materialization path %q has no stable working directory", raw)
		}
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	if output {
		parent, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return "", fmt.Errorf("resolve output parent for %s: %w", path, err)
		}
		path = filepath.Join(parent, filepath.Base(path))
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("output path must not be a symlink: %s", path)
			}
			if !info.Mode().IsRegular() && !info.IsDir() {
				return "", fmt.Errorf("output path must be a regular file or directory: %s", path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return path, nil
	}
	return resolveExistingAncestor(path)
}

func resolveExistingAncestor(path string) (string, error) {
	suffix := make([]string, 0)
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for _, s := range slices.Backward(suffix) {
				resolved = filepath.Join(resolved, s)
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// IdentityKey returns the canonical path used to identify one materialization.
func IdentityKey(path string) string {
	path = filepath.Clean(path)
	if resolved, err := resolveExistingAncestor(path); err == nil {
		path = resolved
	}
	if runtime.GOOS == "windows" {
		path = filepath.ToSlash(path)
	}
	return path
}

// ComparisonKey returns the canonical path used for equality and locking.
func ComparisonKey(path string) string {
	return NewPathKeyResolver().ComparisonKey(path)
}

// PathKeyResolver caches filesystem comparison behavior for one planning or
// evaluation pass.
type PathKeyResolver struct {
	caseInsensitive map[string]bool
}

// NewPathKeyResolver creates a path comparison key resolver.
func NewPathKeyResolver() *PathKeyResolver {
	return &PathKeyResolver{caseInsensitive: make(map[string]bool)}
}

// ComparisonKey returns the canonical path used for equality and locking.
func (r *PathKeyResolver) ComparisonKey(path string) string {
	path = IdentityKey(path)
	dir := filepath.Dir(path)
	caseInsensitive, ok := r.caseInsensitive[dir]
	if !ok {
		caseInsensitive = filesystemIsCaseInsensitive(path)
		r.caseInsensitive[dir] = caseInsensitive
	}
	if caseInsensitive {
		path = strings.ToLower(path)
	}
	return path
}

func filesystemIsCaseInsensitive(path string) bool {
	dir := filepath.Dir(path)
	for {
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
			continue
		}
		if err == nil {
			name := filepath.Base(dir)
			if alternate, ok := alternateASCIICase(name); ok {
				alternateInfo, alternateErr := os.Lstat(filepath.Join(filepath.Dir(dir), alternate))
				switch {
				case alternateErr == nil:
					return os.SameFile(info, alternateInfo)
				case errors.Is(alternateErr, os.ErrNotExist):
					return false
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

func alternateASCIICase(value string) (string, bool) {
	bytes := []byte(value)
	for idx, ch := range bytes {
		switch {
		case ch >= 'a' && ch <= 'z':
			bytes[idx] = ch - ('a' - 'A')
			return string(bytes), true
		case ch >= 'A' && ch <= 'Z':
			bytes[idx] = ch + ('a' - 'A')
			return string(bytes), true
		}
	}
	return value, false
}
