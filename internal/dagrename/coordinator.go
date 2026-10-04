// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package dagrename

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/dagucloud/dagu/v2/internal/cmn/dirlock"
	"github.com/dagucloud/dagu/v2/internal/cmn/fileutil"
	"github.com/dagucloud/dagu/v2/internal/cmn/logger"
	"github.com/dagucloud/dagu/v2/internal/cmn/logger/tag"
	"github.com/dagucloud/dagu/v2/internal/dagsettings"
	"github.com/dagucloud/dagu/v2/internal/persis"
)

// completedMarkerRetention bounds how long terminal journals are retained for
// duplicate-request recognition.
const completedMarkerRetention = 90 * 24 * time.Hour

// Definitions is the definition-store surface required by the coordinator.
// *persis.DAGRepository satisfies it.
type Definitions interface {
	// GetSpec returns the raw definition source for id.
	GetSpec(ctx context.Context, id string) (string, error)
	// PrepareRename validates read-only source and target conflicts without
	// mutating the store.
	PrepareRename(ctx context.Context, oldID, newID string) error
	// CommitRename durably switches oldID to newID.
	CommitRename(ctx context.Context, oldID, newID string) error
}

// Event is published once after a rename converges.
type Event struct {
	OldName string
	NewName string
	// ActorID is the requesting actor captured before the switch so a crash
	// recovery can still attribute the publication.
	ActorID string
}

// PublishFunc receives the single publication for a completed rename.
type PublishFunc func(ctx context.Context, event Event)

// Config configures a rename Coordinator.
type Config struct {
	// StateDir is a writable, persistent directory for the rename journals.
	StateDir    string
	Definitions Definitions
	// Settings is the per-DAG run-configuration store. It may be nil when the
	// server does not persist DAG settings.
	Settings dagsettings.Store
	// Publish fires the change notification and success audit. It must be
	// idempotent under a rare crash-recovery replay.
	Publish PublishFunc
	// Now defaults to time.Now.
	Now func() time.Time
}

// Coordinator stages and converges crash-safe DAG renames.
type Coordinator struct {
	stateDir string
	defs     Definitions
	settings dagsettings.Store
	publish  PublishFunc
	now      func() time.Time

	mu      sync.Mutex
	opLocks map[string]*sync.Mutex
}

// New creates a rename coordinator backed by stateDir.
func New(cfg Config) *Coordinator {
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Coordinator{
		stateDir: cfg.StateDir,
		defs:     cfg.Definitions,
		settings: cfg.Settings,
		publish:  cfg.Publish,
		now:      now,
		opLocks:  make(map[string]*sync.Mutex),
	}
}

// Outcome reports how a rename request completed.
type Outcome struct {
	OldName string
	NewName string
	// AlreadyCompleted is true when the same rename had converged before this
	// request; no new publication was emitted.
	AlreadyCompleted bool
}

// Rename validates, stages, and converges the rename of oldName to newName.
// Callers keep the existing permission, read-only source, target-conflict and
// running-state checks; the coordinator additionally re-validates the
// store-level preconditions under the rename locks.
func (c *Coordinator) Rename(ctx context.Context, oldName, newName, actorID string) (Outcome, error) {
	var outcome Outcome
	err := c.withOpLock(ctx, oldName, newName, func(o op) error {
		st, err := o.readState()
		if err != nil {
			if errors.Is(err, errNoState) {
				outcome, err = c.startNew(ctx, o, actorID)
				return err
			}
			return fmt.Errorf("read DAG rename journal: %w", err)
		}
		outcome, err = c.converge(ctx, o, st, actorID, false)
		return err
	})
	if err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

// LookupOrRecover handles a request whose source name no longer resolves to a
// definition. When a journal for the pair exists it is converged: a completed
// rename is acknowledged idempotently and an interrupted one is recovered.
// found is false when no rename is journaled for the pair, so callers can keep
// their ordinary not-found behavior.
func (c *Coordinator) LookupOrRecover(ctx context.Context, oldName, newName, actorID string) (outcome Outcome, found bool, err error) {
	lockErr := c.withOpLock(ctx, oldName, newName, func(o op) error {
		st, readErr := o.readState()
		if errors.Is(readErr, errNoState) {
			return errUnknownRename
		}
		if readErr != nil {
			return readErr
		}
		outcome, readErr = c.converge(ctx, o, st, actorID, false)
		return readErr
	})
	if errors.Is(lockErr, errUnknownRename) {
		return Outcome{}, false, nil
	}
	if lockErr != nil {
		return Outcome{}, false, lockErr
	}
	return outcome, true, nil
}

// withOpLock serializes the same operation in-process and serializes
// overlapping names across processes.
func (c *Coordinator) withOpLock(ctx context.Context, oldName, newName string, fn func(o op) error) error {
	o := c.opFor(oldName, newName)

	c.opMutex(o.key).Lock()
	defer c.opMutex(o.key).Unlock()

	release, err := c.lockNames(ctx, oldName, newName)
	if err != nil {
		return err
	}
	defer release()

	return fn(o)
}

// startNew stages the candidate definition metadata and run configuration,
// durably records the prepared journal, and then converges the publication.
// Nothing is modified in the definition or settings stores until staging is
// durable.
func (c *Coordinator) startNew(ctx context.Context, o op, actorID string) (Outcome, error) {
	if err := c.defs.PrepareRename(ctx, o.oldName, o.newName); err != nil {
		return Outcome{}, err
	}
	spec, err := c.defs.GetSpec(ctx, o.oldName)
	if err != nil {
		return Outcome{}, err
	}

	now := c.now()
	st := &state{
		OldName:      o.oldName,
		NewName:      o.newName,
		SourceSHA256: hashSpec(spec),
		Stage:        stagePrepared,
		ActorID:      actorID,
		CreatedAt:    now,
	}

	if c.settings != nil {
		existing, err := c.settings.Get(ctx, o.oldName)
		switch {
		case err == nil:
			migrated := existing.Clone()
			migrated.DAGName = o.newName
			migrated.UpdatedBy = actorID
			migrated.UpdatedAt = now
			st.HasSettings = true
			if err := o.writeSettings(stagedFromSettings(migrated)); err != nil {
				return Outcome{}, err
			}
		case errors.Is(err, dagsettings.ErrNotFound):
			// Nothing to move.
		default:
			return Outcome{}, fmt.Errorf("load DAG settings for %s: %w", o.oldName, err)
		}
	}

	if err := o.writeState(st, now); err != nil {
		_ = o.remove()
		return Outcome{}, err
	}
	return c.converge(ctx, o, st, actorID, false)
}

// converge drives a journaled rename forward from its recorded stage. It is
// shared by live requests and crash recovery and is idempotent at every stage.
// In recovery mode stale or impossible journals are discarded without
// surfacing an error; a live request instead re-attempts the rename against
// the current filesystem state.
func (c *Coordinator) converge(ctx context.Context, o op, st *state, actorID string, recovery bool) (Outcome, error) {
	if st.Stage == stageCompleted {
		done, err := c.confirmCompleted(ctx, st)
		if err != nil {
			return Outcome{}, err
		}
		if done {
			return Outcome{OldName: st.OldName, NewName: st.NewName, AlreadyCompleted: true}, nil
		}
		// The terminal marker is stale (the new name is gone): a same-pair
		// rename is a new operation.
		if err := o.remove(); err != nil {
			return Outcome{}, err
		}
		if recovery {
			return Outcome{}, nil
		}
		return c.startNew(ctx, o, actorID)
	}

	oldHash, oldOK, err := c.specHash(ctx, st.OldName)
	if err != nil {
		return Outcome{}, err
	}
	newHash, newOK, err := c.specHash(ctx, st.NewName)
	if err != nil {
		return Outcome{}, err
	}

	switch {
	case oldOK && newOK:
		return Outcome{}, fmt.Errorf("%w: both %s and %s exist", ErrPendingRecovery, st.OldName, st.NewName)
	case !oldOK && !newOK:
		// The source disappeared before the switch completed: there is nothing
		// to converge to. Drop the journal.
		_ = o.remove()
		if recovery {
			return Outcome{}, nil
		}
		return Outcome{}, fmt.Errorf("DAG %s not found: %w", st.OldName, persis.ErrDAGNotFound)
	case oldOK && !newOK:
		if st.Stage != stagePrepared {
			return Outcome{}, fmt.Errorf("source DAG %s returned after rename switch: %w", st.OldName, ErrPendingRecovery)
		}
		if oldHash != st.SourceSHA256 {
			// The source changed after preparation. The prepared candidate is
			// stale; discard it so the live old name stays authoritative.
			_ = o.remove()
			if recovery {
				return Outcome{}, nil
			}
			return c.startNew(ctx, o, actorID)
		}
		if err := c.defs.PrepareRename(ctx, st.OldName, st.NewName); err != nil {
			return Outcome{}, err
		}
		if err := c.defs.CommitRename(ctx, st.OldName, st.NewName); err != nil {
			return Outcome{}, err
		}
		st.Stage = stageDefinitionSwitched
		if err := o.writeState(st, c.now()); err != nil {
			return Outcome{}, err
		}
	case !oldOK && newOK:
		if newHash != st.SourceSHA256 {
			return Outcome{}, fmt.Errorf("target DAG %s holds different content: %w", st.NewName, ErrPendingRecovery)
		}
		if st.Stage == stagePrepared {
			// The durable definition switch already happened (crash before the
			// journal advanced); resume from the switch completion.
			st.Stage = stageDefinitionSwitched
			if err := o.writeState(st, c.now()); err != nil {
				return Outcome{}, err
			}
		}
	}

	// The new name is now the durable definition. Move the run configuration.
	if st.Stage == stageDefinitionSwitched {
		if err := c.switchSettings(ctx, o, st); err != nil {
			return Outcome{}, err
		}
		st.Stage = stageSettingsSwitched
		if err := o.writeState(st, c.now()); err != nil {
			return Outcome{}, err
		}
	}

	// Publish only after definition and run configuration converged.
	if st.Stage == stageSettingsSwitched {
		if c.publish != nil {
			c.publish(ctx, Event{OldName: st.OldName, NewName: st.NewName, ActorID: st.ActorID})
		}
		st.Stage = stagePublished
		if err := o.writeState(st, c.now()); err != nil {
			return Outcome{}, err
		}
	}

	completedAt := c.now()
	st.Stage = stageCompleted
	st.CompletedAt = &completedAt
	if err := o.writeState(st, completedAt); err != nil {
		return Outcome{}, err
	}
	return Outcome{OldName: st.OldName, NewName: st.NewName}, nil
}

// switchSettings installs the staged run configuration under the new name and
// removes the old one only after the new record is in place.
func (c *Coordinator) switchSettings(ctx context.Context, o op, st *state) error {
	if c.settings == nil || !st.HasSettings {
		return nil
	}
	settings, err := o.stagedSettings()
	if err != nil {
		return err
	}
	if err := c.settings.Upsert(ctx, settings); err != nil {
		return fmt.Errorf("migrate DAG settings to %s: %w", st.NewName, err)
	}
	if err := c.settings.Delete(ctx, st.OldName); err != nil && !errors.Is(err, dagsettings.ErrNotFound) {
		return fmt.Errorf("remove DAG settings for %s: %w", st.OldName, err)
	}
	return nil
}

// confirmCompleted verifies a terminal journal still matches the filesystem:
// the new definition exists with the prepared content and the old definition
// is gone.
func (c *Coordinator) confirmCompleted(ctx context.Context, st *state) (bool, error) {
	newHash, newOK, err := c.specHash(ctx, st.NewName)
	if err != nil || !newOK || newHash != st.SourceSHA256 {
		return false, err
	}
	_, oldOK, err := c.specHash(ctx, st.OldName)
	if err != nil {
		return false, err
	}
	return !oldOK, nil
}

func (c *Coordinator) specHash(ctx context.Context, name string) (string, bool, error) {
	spec, err := c.defs.GetSpec(ctx, name)
	if err != nil {
		if errors.Is(err, persis.ErrDAGNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	return hashSpec(spec), true, nil
}

// RecoverAll reconciles every journaled rename at startup. Pending operations
// are completed forward when the filesystem still matches the journal; stale
// or impossible operations are discarded. Errors on individual operations are
// logged and aggregated; they remain retryable on the next duplicate request
// or restart.
func (c *Coordinator) RecoverAll(ctx context.Context) error {
	entries, err := os.ReadDir(c.opsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		o := c.opForName(entry.Name())
		data, readErr := os.ReadFile(o.statePath)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				// A process died after creating the directory but before the
				// prepared state was durable: nothing was switched.
				_ = fileutil.RemoveAll(o.dir)
			}
			continue
		}
		var st state
		if err := json.Unmarshal(data, &st); err != nil {
			logger.Warn(ctx, "Skipping unreadable DAG rename journal",
				tag.Error(err), slog.String("path", o.statePath))
			errs = append(errs, err)
			continue
		}
		o = c.opFor(st.OldName, st.NewName)

		release, lockErr := c.lockNames(ctx, st.OldName, st.NewName)
		if lockErr != nil {
			errs = append(errs, lockErr)
			continue
		}
		c.recoverOne(ctx, o, &errs)
		release()
	}
	return errors.Join(errs...)
}

// recoverOne re-reads the journal under the name locks and converges it.
func (c *Coordinator) recoverOne(ctx context.Context, o op, errs *[]error) {
	st, err := o.readState()
	if err != nil {
		if !errors.Is(err, errNoState) {
			*errs = append(*errs, err)
		}
		return
	}

	if st.Stage == stageCompleted {
		completedAt := st.UpdatedAt
		if st.CompletedAt != nil {
			completedAt = *st.CompletedAt
		}
		if c.now().Sub(completedAt) > completedMarkerRetention {
			if err := o.remove(); err != nil {
				*errs = append(*errs, err)
			}
			return
		}
	}

	if _, err := c.converge(ctx, o, st, st.ActorID, true); err != nil {
		logger.Warn(ctx, "Failed to recover DAG rename",
			tag.Error(err),
			slog.String("old_name", st.OldName),
			slog.String("new_name", st.NewName))
		*errs = append(*errs, err)
	}
}

func (c *Coordinator) opForName(dirName string) op {
	dir := filepath.Join(c.opsDir(), dirName)
	return op{
		key:          dirName,
		dir:          dir,
		statePath:    filepath.Join(dir, stateFileName),
		settingsPath: filepath.Join(dir, settingsFile),
	}
}

func (c *Coordinator) opMutex(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	mu, ok := c.opLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		c.opLocks[key] = mu
	}
	return mu
}

// lockNames acquires cross-process locks for both names in a deterministic
// order so concurrent renames touching the same name cannot interleave.
func (c *Coordinator) lockNames(ctx context.Context, names ...string) (func(), error) {
	keys := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		key := nameLockKey(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	locks := make([]dirlock.DirLock, 0, len(keys))
	for _, key := range keys {
		lock := dirlock.New(filepath.Join(c.stateDir, locksSubdir, key), &dirlock.LockOptions{
			StaleThreshold: 30 * time.Second,
			RetryInterval:  50 * time.Millisecond,
		})
		if err := lock.Lock(ctx); err != nil {
			for _, lock := range slices.Backward(locks) {
				_ = lock.Unlock()
			}
			return nil, fmt.Errorf("acquire DAG rename lock: %w", err)
		}
		locks = append(locks, lock)
	}
	return func() {
		for _, lock := range slices.Backward(locks) {
			if err := lock.Unlock(); err != nil {
				logger.Warn(ctx, "Failed to release DAG rename lock", tag.Error(err))
			}
		}
	}, nil
}
