// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package dagrename

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dagucloud/dagu/v2/internal/dagsettings"
	"github.com/dagucloud/dagu/v2/internal/persis"
	persiststore "github.com/dagucloud/dagu/v2/internal/persis/store"
	persisttest "github.com/dagucloud/dagu/v2/internal/persis/testutil"
	"github.com/dagucloud/dagu/v2/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const oldDAG = "rename-old"
const newDAG = "rename-new"

func sampleSpec(name string) string {
	return "name: " + name + "\nsteps: []\n"
}

type fixture struct {
	t        *testing.T
	c        *Coordinator
	repo     *persis.DAGRepository
	settings dagsettings.Store
	events   []Event
	now      time.Time
}

func newFixture(t *testing.T, withSettings bool) *fixture {
	t.Helper()
	ctx := context.Background()

	dagsDir := filepath.Join(t.TempDir(), "dags")
	repo := testutil.NewFileDAGRepository(dagsDir)
	require.NoError(t, repo.Create(ctx, oldDAG, []byte(sampleSpec(oldDAG))))

	var settingsStore dagsettings.Store
	if withSettings {
		backend := persisttest.NewMemoryBackend()
		s, err := persiststore.NewDAGSettingsStore(backend.Collection("dag-settings"))
		require.NoError(t, err)
		settingsStore = s
	}

	now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	f := &fixture{
		t:        t,
		repo:     repo,
		settings: settingsStore,
		now:      now,
	}
	f.c = New(Config{
		StateDir:    filepath.Join(t.TempDir(), "dag-renames"),
		Definitions: repo,
		Settings:    settingsStore,
		Publish: func(_ context.Context, event Event) {
			f.events = append(f.events, event)
		},
		Now: func() time.Time { return f.now },
	})
	return f
}

func (f *fixture) putSettings(name, profile string) {
	f.t.Helper()
	s, err := dagsettings.New(dagsettings.UpdateInput{
		DAGName:   name,
		Profile:   profile,
		UpdatedBy: "creator",
	}, f.now.Add(-time.Hour))
	require.NoError(f.t, err)
	require.NoError(f.t, settingsOrNil(f).Upsert(context.Background(), s))
}

func settingsOrNil(f *fixture) dagsettings.Store {
	if f.settings == nil {
		panic("settings store not configured")
	}
	return f.settings
}

func (f *fixture) assertOnlyDefinition(name string) {
	f.t.Helper()
	ctx := context.Background()
	if name == oldDAG {
		_, err := f.repo.GetSpec(ctx, newDAG)
		require.ErrorIs(f.t, err, persis.ErrDAGNotFound)
	} else {
		_, err := f.repo.GetSpec(ctx, oldDAG)
		require.ErrorIs(f.t, err, persis.ErrDAGNotFound)
	}
	spec, err := f.repo.GetSpec(ctx, name)
	require.NoError(f.t, err)
	// Renaming moves the definition file without rewriting its contents.
	assert.Equal(f.t, sampleSpec(oldDAG), spec)
}

func (f *fixture) assertOnlySettings(name, profile string) {
	f.t.Helper()
	if f.settings == nil {
		return
	}
	ctx := context.Background()
	got, err := f.settings.Get(ctx, name)
	require.NoError(f.t, err)
	assert.Equal(f.t, name, got.DAGName)
	assert.Equal(f.t, profile, got.Profile)
	assert.Equal(f.t, "creator", got.CreatedBy)

	other := oldDAG
	if name == oldDAG {
		other = newDAG
	}
	_, err = f.settings.Get(ctx, other)
	require.ErrorIs(f.t, err, dagsettings.ErrNotFound)
}

func TestRenamePublishesOnceAndMigratesSettings(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	f.putSettings(oldDAG, "profile-a")

	outcome, err := f.c.Rename(ctx, oldDAG, newDAG, "actor-1")
	require.NoError(t, err)
	assert.False(t, outcome.AlreadyCompleted)
	f.assertOnlyDefinition(newDAG)
	f.assertOnlySettings(newDAG, "profile-a")
	require.Len(t, f.events, 1)
	assert.Equal(t, Event{OldName: oldDAG, NewName: newDAG, ActorID: "actor-1"}, f.events[0])

	// A duplicate request recognizes the completed rename and publishes again
	// neither a notification nor an audit.
	outcome, err = f.c.Rename(ctx, oldDAG, newDAG, "actor-1")
	require.NoError(t, err)
	assert.True(t, outcome.AlreadyCompleted)
	assert.Len(t, f.events, 1)
}

func TestRenameWithoutSettings(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	ctx := context.Background()

	_, err := f.c.Rename(ctx, oldDAG, newDAG, "actor-1")
	require.NoError(t, err)
	f.assertOnlyDefinition(newDAG)
	require.Len(t, f.events, 1)
}

func TestRenameTargetConflictLeavesOldUsableAndNoJournal(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	f.putSettings(oldDAG, "profile-a")
	require.NoError(t, f.repo.Create(ctx, newDAG, []byte(sampleSpec(newDAG))))

	_, err := f.c.Rename(ctx, oldDAG, newDAG, "actor-1")
	require.ErrorIs(t, err, persis.ErrDAGAlreadyExists)

	// The pre-existing source and target definitions both remain editable,
	// and the run configuration stays under the old name.
	oldSpec, err := f.repo.GetSpec(ctx, oldDAG)
	require.NoError(t, err)
	assert.Equal(t, sampleSpec(oldDAG), oldSpec)
	newSpec, err := f.repo.GetSpec(ctx, newDAG)
	require.NoError(t, err)
	assert.Equal(t, sampleSpec(newDAG), newSpec)
	f.assertOnlySettings(oldDAG, "profile-a")
	assert.Empty(t, f.events)

	entries, err := os.ReadDir(filepath.Join(f.c.stateDir, opsSubdir))
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else {
		require.NoError(t, err)
	}
	assert.Empty(t, entries)
}

// Crash right after the prepared journal landed: the definition still has the
// old name; startup recovery completes the publication forward.
func TestRecoverFromPreparedStage(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	f.putSettings(oldDAG, "profile-a")

	o := f.c.opFor(oldDAG, newDAG)
	spec, err := f.repo.GetSpec(ctx, oldDAG)
	require.NoError(t, err)
	settings, err := f.settings.Get(ctx, oldDAG)
	require.NoError(t, err)
	migrated := settings.Clone()
	migrated.DAGName = newDAG
	require.NoError(t, o.writeSettings(stagedFromSettings(migrated)))
	st := &state{
		OldName:      oldDAG,
		NewName:      newDAG,
		SourceSHA256: hashSpec(spec),
		HasSettings:  true,
		Stage:        stagePrepared,
		ActorID:      "actor-1",
		CreatedAt:    f.now,
	}
	require.NoError(t, o.writeState(st, f.now))

	require.NoError(t, f.c.RecoverAll(ctx))
	f.assertOnlyDefinition(newDAG)
	f.assertOnlySettings(newDAG, "profile-a")
	require.Len(t, f.events, 1)
	assert.Equal(t, newDAG, f.events[0].NewName)
}

// Crash after the definition switched and the new settings were upserted but
// before the old settings were deleted: two records exist briefly; recovery
// converges to the new name only.
func TestRecoverWithDuplicateSettingsAfterSwitch(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	f.putSettings(oldDAG, "profile-a")

	require.NoError(t, f.repo.PrepareRename(ctx, oldDAG, newDAG))
	require.NoError(t, f.repo.CommitRename(ctx, oldDAG, newDAG))

	// Both records present: the migration got interrupted mid-flight.
	existing, err := f.settings.Get(ctx, oldDAG)
	require.NoError(t, err)
	migrated := existing.Clone()
	migrated.DAGName = newDAG
	migrated.UpdatedAt = f.now
	require.NoError(t, f.settings.Upsert(ctx, migrated))

	o := f.c.opFor(oldDAG, newDAG)
	require.NoError(t, o.writeSettings(stagedFromSettings(migrated)))
	newSpec, err := f.repo.GetSpec(ctx, newDAG)
	require.NoError(t, err)
	st := &state{
		OldName:      oldDAG,
		NewName:      newDAG,
		SourceSHA256: hashSpec(newSpec),
		HasSettings:  true,
		Stage:        stageDefinitionSwitched,
		ActorID:      "actor-1",
		CreatedAt:    f.now.Add(-time.Second),
	}
	require.NoError(t, o.writeState(st, f.now))

	require.NoError(t, f.c.RecoverAll(ctx))
	f.assertOnlyDefinition(newDAG)
	f.assertOnlySettings(newDAG, "profile-a")
	require.Len(t, f.events, 1)

	// Recovery is idempotent.
	require.NoError(t, f.c.RecoverAll(ctx))
	assert.Len(t, f.events, 1)
}

// Crash after the settings converged but before publication: recovery emits
// the single notification/audit and then completes.
func TestRecoverPublishesOnceFromSettingsStage(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	f.putSettings(oldDAG, "profile-a")

	require.NoError(t, f.repo.CommitRename(ctx, oldDAG, newDAG))
	existing, err := f.settings.Get(ctx, oldDAG)
	require.NoError(t, err)
	migrated := existing.Clone()
	migrated.DAGName = newDAG
	require.NoError(t, f.settings.Upsert(ctx, migrated))
	require.NoError(t, f.settings.Delete(ctx, oldDAG))

	o := f.c.opFor(oldDAG, newDAG)
	newSpec, err := f.repo.GetSpec(ctx, newDAG)
	require.NoError(t, err)
	st := &state{
		OldName:      oldDAG,
		NewName:      newDAG,
		SourceSHA256: hashSpec(newSpec),
		HasSettings:  true,
		Stage:        stageSettingsSwitched,
		ActorID:      "actor-1",
		CreatedAt:    f.now.Add(-time.Second),
	}
	require.NoError(t, o.writeState(st, f.now))

	require.NoError(t, f.c.RecoverAll(ctx))
	f.assertOnlySettings(newDAG, "profile-a")
	require.Len(t, f.events, 1)
}

// Crash after publication but before the terminal marker: recovery completes
// without replaying the publication.
func TestRecoverFromPublishedStageDoesNotRepublish(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	ctx := context.Background()
	require.NoError(t, f.repo.CommitRename(ctx, oldDAG, newDAG))

	o := f.c.opFor(oldDAG, newDAG)
	newSpec, err := f.repo.GetSpec(ctx, newDAG)
	require.NoError(t, err)
	st := &state{
		OldName:      oldDAG,
		NewName:      newDAG,
		SourceSHA256: hashSpec(newSpec),
		Stage:        stagePublished,
		ActorID:      "actor-1",
		CreatedAt:    f.now.Add(-time.Second),
	}
	require.NoError(t, o.writeState(st, f.now))

	require.NoError(t, f.c.RecoverAll(ctx))
	assert.Empty(t, f.events)
}

// The source changed after prepare: the stale journal is rolled back and the
// live old definition remains authoritative and usable.
func TestRecoverRollsBackWhenSourceChanged(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx := context.Background()
	f.putSettings(oldDAG, "profile-a")

	o := f.c.opFor(oldDAG, newDAG)
	original, err := f.repo.GetSpec(ctx, oldDAG)
	require.NoError(t, err)
	require.NoError(t, f.repo.UpdateSpec(ctx, oldDAG, []byte("name: "+oldDAG+"\nsteps: []\n# changed\n")))

	st := &state{
		OldName:      oldDAG,
		NewName:      newDAG,
		SourceSHA256: hashSpec(original),
		HasSettings:  true,
		Stage:        stagePrepared,
		ActorID:      "actor-1",
		CreatedAt:    f.now,
	}
	require.NoError(t, o.writeState(st, f.now))

	require.NoError(t, f.c.RecoverAll(ctx))
	// Old definition is still live under the old name; the candidate is gone.
	_, err = f.repo.GetSpec(ctx, oldDAG)
	require.NoError(t, err)
	_, err = f.repo.GetSpec(ctx, newDAG)
	require.ErrorIs(t, err, persis.ErrDAGNotFound)
	f.assertOnlySettings(oldDAG, "profile-a")
	assert.Empty(t, f.events)

	// Old name remains usable for a fresh rename to a different target.
	_, err = f.c.Rename(ctx, oldDAG, "rename-final", "actor-1")
	require.NoError(t, err)
}

// A terminal marker whose new name has since been removed is treated as a new
// operation when the same pair is requested again.
func TestRenameRestartsAfterStaleCompletedMarker(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	ctx := context.Background()

	// First rename completes and leaves a marker.
	_, err := f.c.Rename(ctx, oldDAG, newDAG, "actor-1")
	require.NoError(t, err)
	require.Len(t, f.events, 1)

	// New name is later removed and the old name is recreated independently.
	require.NoError(t, f.repo.Delete(ctx, newDAG))
	require.NoError(t, f.repo.Create(ctx, oldDAG, []byte(sampleSpec(oldDAG))))

	outcome, err := f.c.Rename(ctx, oldDAG, newDAG, "actor-2")
	require.NoError(t, err)
	assert.False(t, outcome.AlreadyCompleted)
	f.assertOnlyDefinition(newDAG)
	require.Len(t, f.events, 2)
	assert.Equal(t, "actor-2", f.events[1].ActorID)
}

func TestRecoverAllWithoutStateDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	require.NoError(t, f.c.RecoverAll(context.Background()))
}

func TestRecoverPrunesExpiredCompletedMarkers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	ctx := context.Background()
	require.NoError(t, f.repo.CommitRename(ctx, oldDAG, newDAG))

	o := f.c.opFor(oldDAG, newDAG)
	newSpec, err := f.repo.GetSpec(ctx, newDAG)
	require.NoError(t, err)
	completedAt := f.now.Add(-completedMarkerRetention - time.Hour)
	st := &state{
		OldName:      oldDAG,
		NewName:      newDAG,
		SourceSHA256: hashSpec(newSpec),
		Stage:        stageCompleted,
		CreatedAt:    completedAt,
		CompletedAt:  &completedAt,
	}
	require.NoError(t, o.writeState(st, completedAt))

	require.NoError(t, f.c.RecoverAll(ctx))
	_, err = os.Stat(o.dir)
	assert.True(t, errors.Is(err, os.ErrNotExist))
}
