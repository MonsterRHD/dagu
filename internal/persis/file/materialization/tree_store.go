// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package materialization

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dagucloud/dagu/v2/internal/build"
	"github.com/dagucloud/dagu/v2/internal/cmn/fileutil"
)

func publishTree(journalPath *string, journal *commitJournal) error {
	if journal.PreviousFinal == nil {
		return renameDurable(journal.StagingPath, journal.FinalPath)
	}
	if err := exchangePaths(journal.StagingPath, journal.FinalPath); err == nil {
		return fileutil.SyncDir(filepath.Dir(journal.FinalPath))
	}

	journal.Mode = commitModeBackup
	if err := fileutil.WriteJSONAtomic(*journalPath, journal, fileMode); err != nil {
		return err
	}
	if err := renameDurable(journal.FinalPath, journal.BackupPath); err != nil {
		return err
	}
	return renameDurable(journal.StagingPath, journal.FinalPath)
}

func renameDurable(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return err
	}
	return fileutil.SyncDir(filepath.Dir(target))
}

func removeAllDurable(path string) error {
	if path == "" {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return fileutil.SyncDir(filepath.Dir(path))
}

func cleanupJournalArtifacts(journal commitJournal) {
	if journal.Mode == commitModeExchange && journal.StagingPath != "" {
		_ = removeAllDurable(journal.StagingPath)
	}
	if journal.BackupPath != "" {
		_ = removeAllDurable(journal.BackupPath)
	}
}

func restorePrevious(journal commitJournal, force bool) error {
	if journal.PreviousFinal != nil {
		if err := restoreWithPrevious(journal, force); err != nil {
			return err
		}
	} else {
		if err := removeProposedIfPresent(journal, force); err != nil {
			return err
		}
	}
	if journal.StagingPath != journal.FinalPath && journal.StagingPath != journal.BackupPath {
		if err := removeAllDurable(journal.StagingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if len(journal.PreviousManifest) > 0 {
		return fileutil.WriteFileAtomic(journal.ManifestPath, journal.PreviousManifest, fileMode)
	}
	if err := fileutil.RemoveFileDurable(journal.ManifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func restoreWithPrevious(journal commitJournal, force bool) error {
	previous := *journal.PreviousFinal
	if _, err := os.Lstat(journal.FinalPath); errors.Is(err, os.ErrNotExist) {
		if journal.Mode == commitModeBackup {
			if err := requirePathIdentifiesAt(journal.BackupPath, previous, journal.FinalPath); err != nil {
				return fmt.Errorf("previous output backup is unavailable: %w", err)
			}
			return renameDurable(journal.BackupPath, journal.FinalPath)
		}
		if err := requirePathIdentifiesAt(journal.StagingPath, previous, journal.FinalPath); err != nil {
			return fmt.Errorf("previous output is unavailable at staging path: %w", err)
		}
		return renameDurable(journal.StagingPath, journal.FinalPath)
	} else if err != nil {
		return err
	}

	finalMatches, err := pathIdentifies(journal.FinalPath, previous)
	if err != nil && !force {
		return err
	}
	if finalMatches {
		return nil
	}
	proposed, proposalErr := pathIdentifies(journal.FinalPath, journal.Proposed.Output)
	if proposalErr != nil && !errors.Is(proposalErr, os.ErrNotExist) && !force {
		return proposalErr
	}
	if force || (proposalErr == nil && proposed) {
		if journal.Mode == commitModeExchange {
			if err := requirePathIdentifiesAt(journal.StagingPath, previous, journal.FinalPath); err != nil {
				return fmt.Errorf("previous output is unavailable at staging path: %w", err)
			}
			if err := exchangePaths(journal.FinalPath, journal.StagingPath); err != nil {
				return err
			}
			if err := fileutil.SyncDir(filepath.Dir(journal.FinalPath)); err != nil {
				return err
			}
			return removeAllDurable(journal.StagingPath)
		}
		if err := requirePathIdentifiesAt(journal.BackupPath, previous, journal.FinalPath); err != nil {
			return fmt.Errorf("previous output backup is unavailable: %w", err)
		}
		if err := removeAllDurable(journal.FinalPath); err != nil {
			return err
		}
		return renameDurable(journal.BackupPath, journal.FinalPath)
	}
	return fmt.Errorf("current output cannot be identified as the previous or proposed materialization")
}

func removeProposedIfPresent(journal commitJournal, force bool) error {
	if _, err := os.Lstat(journal.FinalPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	matches, err := pathIdentifies(journal.FinalPath, journal.Proposed.Output)
	if err != nil {
		if !force {
			return err
		}
	} else if !matches {
		if !force {
			return fmt.Errorf("current output cannot be identified as the proposed materialization")
		}
	}
	return removeAllDurable(journal.FinalPath)
}

func pathIdentifies(path string, expected build.FileSnapshot) (bool, error) {
	return pathIdentifiesAt(path, expected, path)
}

func pathIdentifiesAt(path string, expected build.FileSnapshot, finalPath string) (bool, error) {
	got, err := build.SnapshotForPublication(expected.Name, path, finalPath)
	if err != nil {
		return false, err
	}
	got.Path = expected.Path
	got.Name = expected.Name
	return build.SnapshotsEqual(got, expected), nil
}

func requirePathIdentifiesAt(path string, expected build.FileSnapshot, finalPath string) error {
	matches, err := pathIdentifiesAt(path, expected, finalPath)
	if err != nil {
		return err
	}
	if !matches {
		return fmt.Errorf("content at %s is not the expected materialization", path)
	}
	return nil
}
