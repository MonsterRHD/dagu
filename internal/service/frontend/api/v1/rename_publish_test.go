// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/dagucloud/dagu/v2/api/v1"
	"github.com/dagucloud/dagu/v2/internal/dagrename"
	"github.com/dagucloud/dagu/v2/internal/dagsettings"
	"github.com/dagucloud/dagu/v2/internal/persis"
	persiststore "github.com/dagucloud/dagu/v2/internal/persis/store"
	persisttest "github.com/dagucloud/dagu/v2/internal/persis/testutil"
	localapi "github.com/dagucloud/dagu/v2/internal/service/frontend/api/v1"
	"github.com/dagucloud/dagu/v2/internal/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func renameAPI(
	helper test.Helper,
	settingsStore dagsettings.Store,
	notified *[]string,
) *localapi.API {
	opts := []localapi.APIOption{}
	if settingsStore != nil {
		opts = append(opts, localapi.WithDAGSettingsStore(settingsStore))
	}
	if notified != nil {
		opts = append(opts, localapi.WithDAGMutationNotifier(func(fileName string) {
			*notified = append(*notified, fileName)
		}))
	}
	return localapi.New(
		helper.DAGRepository,
		helper.DAGRunRepository,
		helper.QueueStore,
		helper.ProcRepository,
		helper.DAGRunMgr,
		helper.Config,
		nil,
		helper.ServiceRegistry,
		nil,
		nil,
		opts...,
	)
}

func renameSettingsStore(t *testing.T) dagsettings.Store {
	t.Helper()
	backend := persisttest.NewMemoryBackend()
	store, err := persiststore.NewDAGSettingsStore(backend.Collection(persis.CollectionDAGSettings))
	require.NoError(t, err)
	return store
}

// A successful rename switches definition and run configuration as one
// publication: the new name owns the only settings record, the change
// notification fires exactly once for the new name, and a duplicate request
// is an idempotent success with no second publication.
func TestRenameDAGAtomicPublication(t *testing.T) {
	t.Parallel()

	helper := test.Setup(t, test.WithStatusPersistence())
	ctx := context.Background()
	dag := helper.DAG(t, "name: atomic-rename-dag\nsteps: []\n")
	newName := "atomic-rename-dag-new"

	settingsStore := renameSettingsStore(t)
	settings, err := dagsettings.New(dagsettings.UpdateInput{
		DAGName:   dag.FileName(),
		UpdatedBy: "creator",
	}, time.Now())
	require.NoError(t, err)
	require.NoError(t, settingsStore.Upsert(ctx, settings))

	var notified []string
	apiImpl := renameAPI(helper, settingsStore, &notified)

	resp, err := apiImpl.RenameDAG(ctx, api.RenameDAGRequestObject{
		FileName: dag.FileName(),
		Body:     &api.RenameDAGJSONRequestBody{NewFileName: newName},
	})
	require.NoError(t, err)
	require.IsType(t, api.RenameDAG200Response{}, resp)

	_, err = helper.DAGRepository.GetSpec(ctx, dag.FileName())
	require.ErrorIs(t, err, persis.ErrDAGNotFound)
	newSpec, err := helper.DAGRepository.GetSpec(ctx, newName)
	require.NoError(t, err)
	assert.Contains(t, newSpec, "atomic-rename-dag")

	_, err = settingsStore.Get(ctx, dag.FileName())
	require.ErrorIs(t, err, dagsettings.ErrNotFound)
	migrated, err := settingsStore.Get(ctx, newName)
	require.NoError(t, err)
	assert.Equal(t, newName, migrated.DAGName)
	assert.Equal(t, "creator", migrated.CreatedBy)
	assert.Equal(t, []string{newName}, notified)

	// Duplicate request: the completed rename is recognized, no re-audit or
	// second change notification.
	resp, err = apiImpl.RenameDAG(ctx, api.RenameDAGRequestObject{
		FileName: dag.FileName(),
		Body:     &api.RenameDAGJSONRequestBody{NewFileName: newName},
	})
	require.NoError(t, err)
	require.IsType(t, api.RenameDAG200Response{}, resp)
	assert.Equal(t, []string{newName}, notified)

	// Only the new name holds run configuration.
	_, err = settingsStore.Get(ctx, dag.FileName())
	require.ErrorIs(t, err, dagsettings.ErrNotFound)
}

// Target conflicts are rejected before anything is staged: the old definition
// and its run configuration remain usable.
func TestRenameDAGConflictRejectsBeforeSwitch(t *testing.T) {
	t.Parallel()

	helper := test.Setup(t, test.WithStatusPersistence())
	ctx := context.Background()
	dag := helper.DAG(t, "name: atomic-rename-conflict\nsteps: []\n")
	newName := "atomic-rename-conflict-target"
	require.NoError(t, helper.DAGRepository.Create(ctx, newName, []byte("name: "+newName+"\nsteps: []\n")))

	settingsStore := renameSettingsStore(t)
	settings, err := dagsettings.New(dagsettings.UpdateInput{DAGName: dag.FileName()}, time.Now())
	require.NoError(t, err)
	require.NoError(t, settingsStore.Upsert(ctx, settings))

	var notified []string
	apiImpl := renameAPI(helper, settingsStore, &notified)

	_, err = apiImpl.RenameDAG(ctx, api.RenameDAGRequestObject{
		FileName: dag.FileName(),
		Body:     &api.RenameDAGJSONRequestBody{NewFileName: newName},
	})
	require.ErrorIs(t, err, persis.ErrDAGAlreadyExists)

	_, err = settingsStore.Get(ctx, dag.FileName())
	require.NoError(t, err)
	_, err = settingsStore.Get(ctx, newName)
	require.ErrorIs(t, err, dagsettings.ErrNotFound)
	assert.Empty(t, notified)
}

// failAfterCommitDefinitions moves the definition and then reports failure,
// reproducing a crash in the window after the durable switch but before the
// journal advanced.
type failAfterCommitDefinitions struct {
	inner dagrename.Definitions
}

var errInjectedCommitFailure = assert.AnError

func (d *failAfterCommitDefinitions) GetSpec(ctx context.Context, id string) (string, error) {
	return d.inner.GetSpec(ctx, id)
}

func (d *failAfterCommitDefinitions) PrepareRename(ctx context.Context, oldID, newID string) error {
	return d.inner.PrepareRename(ctx, oldID, newID)
}

func (d *failAfterCommitDefinitions) CommitRename(ctx context.Context, oldID, newID string) error {
	if err := d.inner.CommitRename(ctx, oldID, newID); err != nil {
		return err
	}
	return errInjectedCommitFailure
}

// A new process (fresh coordinator over the same persistent state directory)
// completes the interrupted rename on startup: the run configuration converges
// to the new name and the publication happens exactly once.
func TestRenameDAGRecoveredByFreshCoordinator(t *testing.T) {
	t.Parallel()

	helper := test.Setup(t, test.WithStatusPersistence())
	ctx := context.Background()
	dag := helper.DAG(t, "name: atomic-rename-recovery\nsteps: []\n")
	newName := "atomic-rename-recovery-new"

	settingsStore := renameSettingsStore(t)
	settings, err := dagsettings.New(dagsettings.UpdateInput{
		DAGName:   dag.FileName(),
		UpdatedBy: "creator",
	}, time.Now())
	require.NoError(t, err)
	require.NoError(t, settingsStore.Upsert(ctx, settings))

	stateDir := helper.Config.Paths.DataDir + "/dag-renames"
	var events []dagrename.Event
	first := dagrename.New(dagrename.Config{
		StateDir:    stateDir,
		Definitions: &failAfterCommitDefinitions{inner: helper.DAGRepository},
		Settings:    settingsStore,
		Publish: func(_ context.Context, event dagrename.Event) {
			events = append(events, event)
		},
	})

	_, err = first.Rename(ctx, dag.FileName(), newName, "actor-1")
	require.ErrorIs(t, err, errInjectedCommitFailure)

	// Definition switched durably; old settings are still in place because the
	// publication did not run.
	_, err = helper.DAGRepository.GetSpec(ctx, newName)
	require.NoError(t, err)
	_, err = settingsStore.Get(ctx, dag.FileName())
	require.NoError(t, err)
	assert.Empty(t, events)

	// Restart equivalent: a fresh coordinator reconciles the journal.
	var recoveredEvents []dagrename.Event
	second := dagrename.New(dagrename.Config{
		StateDir:    stateDir,
		Definitions: helper.DAGRepository,
		Settings:    settingsStore,
		Publish: func(_ context.Context, event dagrename.Event) {
			recoveredEvents = append(recoveredEvents, event)
		},
	})
	require.NoError(t, second.RecoverAll(ctx))

	migrated, err := settingsStore.Get(ctx, newName)
	require.NoError(t, err)
	assert.Equal(t, newName, migrated.DAGName)
	_, err = settingsStore.Get(ctx, dag.FileName())
	require.ErrorIs(t, err, dagsettings.ErrNotFound)
	require.Len(t, recoveredEvents, 1)
	assert.Equal(t, dagrename.Event{OldName: dag.FileName(), NewName: newName, ActorID: "actor-1"}, recoveredEvents[0])

	// Recovery is idempotent and does not republish.
	require.NoError(t, second.RecoverAll(ctx))
	assert.Len(t, recoveredEvents, 1)
}
