// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package dag

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/dagucloud/dagu/v2/internal/cmn/fileutil"
	"github.com/dagucloud/dagu/v2/internal/persis"
)

// renamePlan describes a validated definition rename that has not switched
// files yet.
type renamePlan struct {
	oldPath string
	newPath string
}

// resolveRename validates the source and target of a definition rename without
// modifying the filesystem. The source must not be an external symlink and the
// target name must not already exist.
func (store *Store) resolveRename(ctx context.Context, oldID, newID string) (renamePlan, error) {
	resolved, err := store.locateDAG(ctx, oldID)
	if err != nil {
		return renamePlan{}, fmt.Errorf("failed to locate DAG %s: %w", oldID, err)
	}
	if resolved.ExternalSymlink {
		return renamePlan{}, fmt.Errorf("%w: external DAG file symlink", persis.ErrDAGReadOnly)
	}
	oldFilePath := resolved.EntryPath
	newFilePath := store.generateFilePath(newID)
	exceptPath := ""
	if store.recursive {
		if relPath, ok := relativeToBase(store.baseDir, oldFilePath); ok {
			newFilePath = fileutil.EnsureYAMLExtension(filepath.Join(filepath.Dir(oldFilePath), filepath.Base(newID)))
			exceptPath = relPath
		}
	}
	if fileExists(newFilePath) || store.recursive && store.stemExists(newID, exceptPath) {
		return renamePlan{}, persis.ErrDAGAlreadyExists
	}
	return renamePlan{oldPath: oldFilePath, newPath: newFilePath}, nil
}

// PrepareRename validates a rename without changing the filesystem.
func (store *Store) PrepareRename(ctx context.Context, oldID, newID string) error {
	_, err := store.resolveRename(ctx, oldID, newID)
	return err
}

// CommitRename durably switches a validated rename. It re-validates the
// preconditions so a plan cannot overwrite a target that appeared while a
// crash-safe rename was being prepared.
func (store *Store) CommitRename(ctx context.Context, oldID, newID string) error {
	plan, err := store.resolveRename(ctx, oldID, newID)
	if err != nil {
		return err
	}
	if err := fileutil.Rename(plan.oldPath, plan.newPath); err != nil {
		return err
	}
	if err := fileutil.SyncDir(filepath.Dir(plan.newPath)); err != nil {
		return fmt.Errorf("failed to sync DAG directory after rename: %w", err)
	}
	store.invalidateIndex()
	return nil
}

// Rename validates and immediately applies a definition rename.
func (store *Store) Rename(ctx context.Context, oldID, newID string) error {
	if err := store.PrepareRename(ctx, oldID, newID); err != nil {
		return err
	}
	return store.CommitRename(ctx, oldID, newID)
}
