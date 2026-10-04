// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package baseconfig_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dagucloud/dagu/v2/internal/baseconfig"
	"github.com/dagucloud/dagu/v2/internal/ir"
	"github.com/dagucloud/dagu/v2/internal/persis"
	filedag "github.com/dagucloud/dagu/v2/internal/persis/file/dag"
	"github.com/dagucloud/dagu/v2/internal/spec"
	"github.com/dagucloud/dagu/v2/internal/testutil"
	"github.com/dagucloud/dagu/v2/internal/workspace"
)

const (
	globalV1 = "schedule: \"0 2 * * *\"\n"
	globalV2 = "schedule: \"0 3 * * *\"\n"
	globalV3 = "schedule: \"0 4 * * *\"\n"
	badV2    = "schedule: \"definitely-not-a-schedule\"\n"
)

func waitForChangeEvent(t *testing.T, ch <-chan persis.DAGChangeEvent, timeout time.Duration) bool {
	t.Helper()
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

func assertNoChangeEvent(t *testing.T, ch <-chan persis.DAGChangeEvent, quiet time.Duration) {
	t.Helper()
	select {
	case event := <-ch:
		t.Fatalf("unexpected scheduler registry event during rejected promotion: %v %s",
			event.Type, event.DAGEntry.DAG.Name)
	case <-time.After(quiet):
	}
}

func drainChangeEvents(ch <-chan persis.DAGChangeEvent, settle time.Duration) {
	deadline := time.After(settle)
	for {
		select {
		case <-ch:
		case <-deadline:
			return
		}
	}
}

func TestPromotion_SchedulerIsolationSchedulabilityAndSnapshots(t *testing.T) {
	t.Parallel()
	root, dagsDir, dataDir, basePath := setupRepositoryFixture(t)
	repo := newTestRepository(t, dagsDir, dataDir, basePath)
	live := newFileLiveStores(t, basePath, dagsDir)
	stateDir := filepath.Join(root, "state")

	ctx := t.Context()

	reader := filedag.NewFileEntryReader(
		dagsDir, repo, false, basePath, workspace.BaseConfigDir(dagsDir),
	)
	require.NoError(t, reader.Init(ctx))
	go reader.Start(ctx)
	defer reader.Stop()

	// The first watcher-driven refresh after startup can re-announce entries
	// built through the init snapshot path. Drain that startup churn.
	drainChangeEvents(reader.Events(), 500*time.Millisecond)

	initialEntries := reader.Entries()
	require.Len(t, initialEntries, 4)
	entryNames := map[string]bool{}
	for _, entry := range initialEntries {
		entryNames[entry.DAG.Name] = true
		assert.NotEmpty(t, entry.DAG.Schedule, "workflow %s must keep an inherited schedule", entry.DAG.Name)
	}
	assert.True(t, entryNames["plain-wf"])

	// A queued attempt created before promotion keeps the old base config in
	// its dag.json.
	runRepo := testutil.NewFileDAGRunRepository(t.TempDir(), persis.DAGRunRepositoryOptions{})
	dagOld, err := repo.GetDetails(ctx, "plain", persis.DAGLoadOptions{})
	require.NoError(t, err)
	assert.Contains(t, string(dagOld.BaseConfigData), "0 2")
	oldRef := ir.NewDAGRunRef(dagOld.Name, "old-run")
	oldAttempt, err := runRepo.CreateAttempt(ctx, dagOld, time.Now(), oldRef.ID, persis.DAGRunCreateAttemptOptions{})
	require.NoError(t, err)
	require.NoError(t, oldAttempt.Open(ctx))
	require.NoError(t, oldAttempt.Write(ctx, ir.DAGRunStatus{Name: oldRef.Name, DAGRunID: oldRef.ID, Status: ir.Queued}))
	require.NoError(t, oldAttempt.Close(ctx))

	manager, err := baseconfig.New(ctx, baseconfig.Config{
		StateDir:   stateDir,
		Live:       live,
		Enumerator: baseconfig.NewPersisEnumerator(repo),
		Builder:    baseconfig.NewSpecBuilder(),
	})
	require.NoError(t, err)

	// A rejected candidate neither touches the live file nor the scheduler.
	_, err = manager.Submit(ctx, baseconfig.GlobalScope(), []byte(badV2))
	require.Error(t, err)
	_, ok := baseconfig.AsRejection(err)
	require.True(t, ok)
	assertNoChangeEvent(t, reader.Events(), 400*time.Millisecond)
	entriesAfterReject := reader.Entries()
	require.Len(t, entriesAfterReject, 4)
	for _, entry := range entriesAfterReject {
		assert.True(t, entryNames[entry.DAG.Name])
		assert.NotEmpty(t, entry.DAG.Schedule, "workflow %s lost its schedule", entry.DAG.Name)
	}

	// A valid promotion reaches the scheduler; no workflow disappears.
	outcome, err := manager.Submit(ctx, baseconfig.GlobalScope(), []byte(globalV2))
	require.NoError(t, err)
	assert.Equal(t, int64(1), outcome.Generation)
	assert.True(t, waitForChangeEvent(t, reader.Events(), 5*time.Second),
		"scheduler did not observe the promotion")

	require.Eventually(t, func() bool {
		entries := reader.Entries()
		if len(entries) != 4 {
			return false
		}
		for _, entry := range entries {
			if len(entry.DAG.Schedule) == 0 {
				return false
			}
		}
		return true
	}, 5*time.Second, 50*time.Millisecond)

	dagV2, err := repo.GetDetails(ctx, "plain", persis.DAGLoadOptions{})
	require.NoError(t, err)
	assert.Contains(t, string(dagV2.BaseConfigData), "0 3")
	assert.NotContains(t, string(dagV2.BaseConfigData), "0 2")

	// Restart reconciles and continues generation numbering.
	restarted, err := baseconfig.New(ctx, baseconfig.Config{
		StateDir:   stateDir,
		Live:       live,
		Enumerator: baseconfig.NewPersisEnumerator(repo),
		Builder:    baseconfig.NewSpecBuilder(),
	})
	require.NoError(t, err)
	outcome, err = restarted.Submit(ctx, baseconfig.GlobalScope(), []byte(globalV3))
	require.NoError(t, err)
	assert.Equal(t, int64(2), outcome.Generation)
	assert.True(t, waitForChangeEvent(t, reader.Events(), 5*time.Second),
		"scheduler did not observe the second promotion")

	// The old queued attempt still rebuilds from its captured base config.
	storedAttempt, err := runRepo.FindAttempt(ctx, oldRef)
	require.NoError(t, err)
	storedDAG, err := storedAttempt.ReadDAG(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(storedDAG.BaseConfigData), "0 2")
	rebuilt, err := spec.RebuildFromYAML(ctx, storedDAG)
	require.NoError(t, err)
	require.NotEmpty(t, rebuilt.Schedule)
	assert.True(t, strings.Contains(rebuilt.Schedule[0].Expression, "0 2"),
		"old attempt must keep its inherited schedule, got %q", rebuilt.Schedule[0].Expression)

	// A new attempt captures the confirmed new generation.
	dagV3, err := repo.GetDetails(ctx, "plain", persis.DAGLoadOptions{})
	require.NoError(t, err)
	assert.Contains(t, string(dagV3.BaseConfigData), "0 4")
	newRef := ir.NewDAGRunRef(dagV3.Name, "new-run")
	newAttempt, err := runRepo.CreateAttempt(ctx, dagV3, time.Now(), newRef.ID, persis.DAGRunCreateAttemptOptions{})
	require.NoError(t, err)
	require.NoError(t, newAttempt.Open(ctx))
	require.NoError(t, newAttempt.Close(ctx))
	storedNew, err := runRepo.FindAttempt(ctx, newRef)
	require.NoError(t, err)
	newDAG, err := storedNew.ReadDAG(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(newDAG.BaseConfigData), "0 4")
}
