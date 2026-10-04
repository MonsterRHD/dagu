// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	SnapshotTypeFile      = "file"
	SnapshotTypeDirectory = "directory"
	SnapshotTypeSymlink   = "symlink"
)

var errPathChanged = errors.New("path changed while hashing")

// PathEntry is one deterministic member of a directory snapshot.
type PathEntry struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       int64  `json:"size,omitempty"`
	Digest     string `json:"digest,omitempty"`
	LinkTarget string `json:"target,omitempty"`
}

func snapshotDirectory(name, root, finalRoot string) (FileSnapshot, error) {
	first, size, err := directoryEntries(root, finalRoot)
	if err != nil {
		return FileSnapshot{}, err
	}
	second, _, err := directoryEntries(root, finalRoot)
	if err != nil {
		return FileSnapshot{}, err
	}
	if !entriesEqual(first, second) {
		return FileSnapshot{}, fmt.Errorf("%w: %s", errPathChanged, root)
	}
	data, err := json.Marshal(first)
	if err != nil {
		return FileSnapshot{}, err
	}
	rootDigest := digest(data)
	return FileSnapshot{
		Name:       name,
		Path:       root,
		Type:       SnapshotTypeDirectory,
		Size:       size,
		Digest:     rootDigest,
		RootDigest: rootDigest,
		Entries:    first,
	}, nil
}

func directoryEntries(root, finalRoot string) ([]PathEntry, int64, error) {
	entries := []PathEntry{{Path: ".", Type: SnapshotTypeDirectory}}
	var totalSize int64
	var collect func(string) error
	collect = func(dir string) error {
		items, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		slices.SortFunc(items, func(left, right os.DirEntry) int {
			return strings.Compare(left.Name(), right.Name())
		})
		for _, item := range items {
			fullPath := filepath.Join(dir, item.Name())
			relative, err := filepath.Rel(root, fullPath)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			info, err := item.Info()
			if err != nil {
				return err
			}
			entry := PathEntry{Path: relative}
			switch {
			case info.Mode().IsRegular():
				fileDigest, fileSize, err := hashRegularFileAt(fullPath, info)
				if err != nil {
					return err
				}
				entry.Type = SnapshotTypeFile
				entry.Size = fileSize
				entry.Digest = fileDigest
				totalSize += fileSize
			case info.IsDir():
				entry.Type = SnapshotTypeDirectory
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := validatedSymlinkTarget(root, finalRoot, fullPath)
				if err != nil {
					return err
				}
				entry.Type = SnapshotTypeSymlink
				entry.Size = info.Size()
				entry.LinkTarget = target
			default:
				return fmt.Errorf("%s is not a regular file, directory, or tree-local symlink", fullPath)
			}
			entries = append(entries, entry)
			if info.IsDir() {
				if err := collect(fullPath); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := collect(root); err != nil {
		return nil, 0, err
	}
	return entries, totalSize, nil
}

func hashRegularFileAt(path string, before fs.FileInfo) (string, int64, error) {
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		_ = file.Close()
		return "", 0, err
	}
	if err := file.Close(); err != nil {
		return "", 0, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", 0, fmt.Errorf("%w: %s", errPathChanged, path)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), after.Size(), nil
}

func validatedSymlinkTarget(root, finalRoot, linkPath string) (string, error) {
	target, err := os.Readlink(linkPath)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	candidate := target
	finalCandidate := target
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(filepath.Dir(linkPath), candidate)
		relativeLink, err := filepath.Rel(root, linkPath)
		if err != nil {
			return "", err
		}
		finalLinkPath := filepath.Join(finalRoot, filepath.Dir(filepath.FromSlash(relativeLink)), filepath.Base(linkPath))
		finalCandidate = filepath.Join(filepath.Dir(finalLinkPath), target)
	}
	candidate = filepath.Clean(candidate)
	finalCandidate = filepath.Clean(finalCandidate)
	finalRoot = filepath.Clean(finalRoot)
	withinCurrentRoot := pathWithinRoot(root, candidate) || pathWithinRoot(resolvedRoot, candidate)
	withinFinalRoot := pathWithinRoot(finalRoot, finalCandidate)
	if (root != finalRoot && (!withinFinalRoot || (!filepath.IsAbs(target) && !withinCurrentRoot))) ||
		(root == finalRoot && !withinCurrentRoot) {
		return "", fmt.Errorf("symlink escapes materialization root: %s", linkPath)
	}
	resolved, resolveErr := filepath.EvalSymlinks(candidate)
	if resolveErr != nil {
		if root == finalRoot || !filepath.IsAbs(target) || !withinFinalRoot {
			return "", fmt.Errorf("resolve symlink target for %s: %w", linkPath, resolveErr)
		}
	} else {
		withinResolvedFinalRoot := pathWithinRoot(finalRoot, resolved)
		if root != finalRoot && !filepath.IsAbs(target) {
			withinResolvedFinalRoot = pathWithinRoot(finalRoot, finalCandidate)
		}
		if root == finalRoot {
			withinResolvedFinalRoot = pathWithinRoot(resolvedRoot, resolved)
		}
		if !withinResolvedFinalRoot {
			return "", fmt.Errorf("symlink escapes materialization root: %s", linkPath)
		}
	}
	return filepath.ToSlash(target), nil
}

func pathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func entriesEqual(left, right []PathEntry) bool {
	return slices.EqualFunc(left, right, func(a, b PathEntry) bool {
		return a == b
	})
}

func NormalizeSnapshot(snapshot *FileSnapshot) {
	if snapshot.Type == "" {
		snapshot.Type = SnapshotTypeFile
	}
	for idx := range snapshot.Entries {
		if snapshot.Entries[idx].Type == "" {
			snapshot.Entries[idx].Type = SnapshotTypeFile
		}
	}
}
