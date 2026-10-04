// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

// Package dagrename turns a DAG rename into a recoverable single publication:
// the definition switch and the run-configuration move are staged in a durable
// journal and either completed or reconciled after a crash, so the old name
// stays usable until the new name is durably committed and exactly one
// publication (change notification and success audit) is emitted.
package dagrename

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dagucloud/dagu/v2/internal/cmn/fileutil"
	"github.com/dagucloud/dagu/v2/internal/dagsettings"
)

const (
	journalVersion = 1

	opsSubdir     = "ops"
	locksSubdir   = "locks"
	stateFileName = "state.json"
	settingsFile  = "settings.json"

	stagePrepared           = "prepared"
	stageDefinitionSwitched = "definition_switched"
	stageSettingsSwitched   = "settings_switched"
	stagePublished          = "published"
	stageCompleted          = "completed"
)

var errNoState = errors.New("rename journal state not found")

// errUnknownRename marks the absence of any journal for a rename pair; it is
// an internal control signal and never reaches callers.
var errUnknownRename = errors.New("rename not journaled")

// ErrPendingRecovery reports that a rename for the same DAG is recorded in the
// journal but cannot be reconciled to a single name without intervention.
var ErrPendingRecovery = errors.New("DAG rename is pending recovery")

// state is the durable rename journal record.
type state struct {
	Version       int        `json:"version"`
	OldName       string     `json:"old_name"`
	NewName       string     `json:"new_name"`
	SourceSHA256  string     `json:"source_sha256"`
	HasSettings   bool       `json:"has_settings"`
	Stage         string     `json:"stage"`
	ActorID       string     `json:"actor_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

// stagedSettings is the run configuration prepared under the new name before
// the rename switches.
type stagedSettings struct {
	DAGName   string    `json:"dag_name"`
	Profile   string    `json:"profile,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func stagedFromSettings(s *dagsettings.Settings) stagedSettings {
	return stagedSettings{
		DAGName:   s.DAGName,
		Profile:   s.Profile,
		CreatedBy: s.CreatedBy,
		CreatedAt: s.CreatedAt,
		UpdatedBy: s.UpdatedBy,
		UpdatedAt: s.UpdatedAt,
	}
}

func (s stagedSettings) toSettings() *dagsettings.Settings {
	return &dagsettings.Settings{
		DAGName:   s.DAGName,
		Profile:   s.Profile,
		CreatedBy: s.CreatedBy,
		CreatedAt: s.CreatedAt,
		UpdatedBy: s.UpdatedBy,
		UpdatedAt: s.UpdatedAt,
	}
}

// op is the on-disk location of one rename journal.
type op struct {
	key          string
	dir          string
	statePath    string
	settingsPath string
	oldName      string
	newName      string
}

func operationKey(oldName, newName string) string {
	sum := sha256.Sum256([]byte(oldName + "\x00" + newName))
	return hex.EncodeToString(sum[:])
}

func nameLockKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:16])
}

func (c *Coordinator) opFor(oldName, newName string) op {
	dir := filepath.Join(c.opsDir(), operationKey(oldName, newName))
	return op{
		key:          operationKey(oldName, newName),
		dir:          dir,
		statePath:    filepath.Join(dir, stateFileName),
		settingsPath: filepath.Join(dir, settingsFile),
		oldName:      oldName,
		newName:      newName,
	}
}

func (c *Coordinator) opsDir() string {
	return filepath.Join(c.stateDir, opsSubdir)
}

// readState loads the journal record. It returns errNoState when the operation
// has never been durably prepared.
func (o *op) readState() (*state, error) {
	data, err := os.ReadFile(o.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoState
		}
		return nil, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("decode %s: %w", o.statePath, err)
	}
	if st.Version != journalVersion || st.OldName != o.oldName || st.NewName != o.newName {
		return nil, fmt.Errorf("rename journal %s does not describe %s -> %s", o.key, o.oldName, o.newName)
	}
	return &st, nil
}

func (o *op) writeState(st *state, now time.Time) error {
	if err := os.MkdirAll(o.dir, 0o750); err != nil {
		return fmt.Errorf("create rename journal directory: %w", err)
	}
	st.Version = journalVersion
	st.UpdatedAt = now
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(o.statePath, data, 0o600)
}

func (o *op) writeSettings(s stagedSettings) error {
	if err := os.MkdirAll(o.dir, 0o750); err != nil {
		return fmt.Errorf("create rename journal directory: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(o.settingsPath, data, 0o600)
}

func (o *op) stagedSettings() (*dagsettings.Settings, error) {
	data, err := os.ReadFile(o.settingsPath)
	if err != nil {
		return nil, fmt.Errorf("read staged DAG settings: %w", err)
	}
	var staged stagedSettings
	if err := json.Unmarshal(data, &staged); err != nil {
		return nil, fmt.Errorf("decode staged DAG settings: %w", err)
	}
	return staged.toSettings(), nil
}

// remove discards the operation journal. The live definition and settings
// stores are never touched here.
func (o *op) remove() error {
	if err := fileutil.RemoveAll(o.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func hashSpec(spec string) string {
	sum := sha256.Sum256([]byte(spec))
	return hex.EncodeToString(sum[:])
}
