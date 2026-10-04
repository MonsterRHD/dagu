// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package baseconfig_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dagucloud/dagu/v2/internal/baseconfig"
	"github.com/dagucloud/dagu/v2/internal/cmn/config"
	"github.com/dagucloud/dagu/v2/internal/persis"
	persisfile "github.com/dagucloud/dagu/v2/internal/persis/file"
	filebaseconfig "github.com/dagucloud/dagu/v2/internal/persis/file/baseconfig"
	_ "github.com/dagucloud/dagu/v2/internal/runtime/builtin"
	"github.com/dagucloud/dagu/v2/internal/workspace"
)

func setupRepositoryFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	root := t.TempDir()
	dagsDir := filepath.Join(root, "dags")
	dataDir := filepath.Join(root, "data")
	basePath := filepath.Join(root, "base.yaml")
	alphaPath := workspace.BaseConfigPath(dagsDir, "alpha")
	require.NoError(t, os.MkdirAll(filepath.Dir(alphaPath), 0o750))

	require.NoError(t, os.WriteFile(basePath, []byte("schedule: \"0 2 * * *\"\n"), 0o600))
	require.NoError(t, os.WriteFile(alphaPath, []byte("env:\n  ALPHA_VAR: cur\n"), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(dagsDir, "plain.yaml"), []byte(`
name: plain-wf
steps:
  - name: s1
    run: echo plain
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dagsDir, "alpha.yaml"), []byte(`
name: alpha-wf
labels:
  - workspace=alpha
steps:
  - name: s1
    run: echo alpha
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dagsDir, "bravo.yaml"), []byte(`
name: bravo-wf
labels:
  - workspace=bravo
steps:
  - name: s1
    run: echo bravo
`), 0o600))

	// Multi-document file whose ROOT document is unlabelled but whose second
	// document belongs to workspace alpha. Workspace candidates must cover it.
	require.NoError(t, os.WriteFile(filepath.Join(dagsDir, "team.yaml"), []byte(`
name: team-plain
steps:
  - name: s1
    run: echo team
---
name: team-alpha
labels:
  - workspace=alpha
steps:
  - name: s1
    run: echo team-alpha
`), 0o600))

	return root, dagsDir, dataDir, basePath
}

func newTestRepository(t *testing.T, dagsDir, dataDir, basePath string) *persis.DAGRepository {
	t.Helper()
	cfg := &config.Config{
		Paths: config.PathsConfig{
			DAGsDir:    dagsDir,
			DataDir:    dataDir,
			BaseConfig: basePath,
		},
	}
	repo, err := persisfile.NewDAGRepository(cfg, persisfile.WithDAGSkipExamples(true))
	require.NoError(t, err)
	return repo
}

func TestPersisEnumerator_ScopeAndWorkspaceLabels(t *testing.T) {
	t.Parallel()
	_, dagsDir, dataDir, basePath := setupRepositoryFixture(t)
	repo := newTestRepository(t, dagsDir, dataDir, basePath)
	en := baseconfig.NewPersisEnumerator(repo)
	ctx := context.Background()

	all, err := en.ListAffected(ctx, baseconfig.GlobalScope())
	require.NoError(t, err)
	require.Len(t, all, 4)
	byName := map[string]baseconfig.WorkflowSource{}
	for _, wf := range all {
		byName[wf.Name] = wf
		assert.NotEmpty(t, wf.Location)
		assert.NotEmpty(t, wf.Source)
	}
	assert.Equal(t, "", byName["plain-wf"].Workspace)
	assert.Equal(t, "alpha", byName["alpha-wf"].Workspace)
	assert.Equal(t, "bravo", byName["bravo-wf"].Workspace)
	assert.Equal(t, "", byName["team-plain"].Workspace)
	assert.Equal(t, []string{"alpha"}, byName["team-plain"].Workspaces)

	alphaScope, err := baseconfig.WorkspaceScope("alpha")
	require.NoError(t, err)
	alphaOnly, err := en.ListAffected(ctx, alphaScope)
	require.NoError(t, err)
	require.Len(t, alphaOnly, 2)
	alphaNames := map[string]bool{}
	for _, wf := range alphaOnly {
		alphaNames[wf.Name] = true
	}
	assert.True(t, alphaNames["alpha-wf"])
	assert.True(t, alphaNames["team-plain"])

	bravoScope, err := baseconfig.WorkspaceScope("bravo")
	require.NoError(t, err)
	bravoOnly, err := en.ListAffected(ctx, bravoScope)
	require.NoError(t, err)
	require.Len(t, bravoOnly, 1)
	assert.Equal(t, "bravo-wf", bravoOnly[0].Name)
}

func TestSpecBuilder_CurrentGenerationBuildsAndBadCandidateFails(t *testing.T) {
	t.Parallel()
	_, dagsDir, dataDir, basePath := setupRepositoryFixture(t)
	repo := newTestRepository(t, dagsDir, dataDir, basePath)
	en := baseconfig.NewPersisEnumerator(repo)
	builder := baseconfig.NewSpecBuilder()
	ctx := context.Background()

	currentGlobal, err := os.ReadFile(basePath)
	require.NoError(t, err)
	currentAlpha, err := os.ReadFile(filepath.Join(dagsDir, "workspaces", "alpha", "base.yaml"))
	require.NoError(t, err)
	lookup := func(name string) ([]byte, bool) {
		if name == "alpha" {
			return currentAlpha, true
		}
		return nil, false
	}

	workflows, err := en.ListAffected(ctx, baseconfig.GlobalScope())
	require.NoError(t, err)
	for _, wf := range workflows {
		require.NoError(t, builder.Build(ctx, wf, currentGlobal, lookup), "workflow %s", wf.Name)
	}

	badCandidate := []byte("schedule: \"definitely-not-a-schedule\"\n")
	failures := 0
	for _, wf := range workflows {
		if err := builder.Build(ctx, wf, badCandidate, lookup); err != nil {
			failures++
			assert.Contains(t, err.Error(), "schedule")
		}
	}
	assert.Equal(t, len(workflows), failures)

	goodCandidate := []byte("schedule: \"0 3 * * *\"\n")
	for _, wf := range workflows {
		require.NoError(t, builder.Build(ctx, wf, goodCandidate, lookup), "workflow %s", wf.Name)
	}
}

// fileLiveStores wires the real file base config stores as promotion targets.
type fileLiveStores struct {
	dagsDir string
	global  fileLiveStore
}

type fileLiveStore struct{ store *filebaseconfig.Store }

func (s fileLiveStore) Read(ctx context.Context) ([]byte, error) {
	yamlSpec, err := s.store.GetSpec(ctx)
	return []byte(yamlSpec), err
}

func (s fileLiveStore) Write(ctx context.Context, content []byte) error {
	return s.store.UpdateSpec(ctx, content)
}

func (l fileLiveStores) Global() baseconfig.LiveStore {
	return l.global
}

func (l fileLiveStores) Workspace(name string) (baseconfig.LiveStore, error) {
	store, err := filebaseconfig.New(workspace.BaseConfigPath(l.dagsDir, name), filebaseconfig.WithSkipDefault(true))
	if err != nil {
		return nil, err
	}
	return fileLiveStore{store: store}, nil
}

func newFileLiveStores(t *testing.T, basePath, dagsDir string) fileLiveStores {
	t.Helper()
	globalStore, err := filebaseconfig.New(basePath, filebaseconfig.WithSkipDefault(true))
	require.NoError(t, err)
	return fileLiveStores{dagsDir: dagsDir, global: fileLiveStore{store: globalStore}}
}

func TestManager_PersisEndToEnd(t *testing.T) {
	t.Parallel()
	root, dagsDir, dataDir, basePath := setupRepositoryFixture(t)
	repo := newTestRepository(t, dagsDir, dataDir, basePath)
	alphaPath := workspace.BaseConfigPath(dagsDir, "alpha")

	live := newFileLiveStores(t, basePath, dagsDir)
	stateDir := filepath.Join(root, "state")
	newManager := func() *baseconfig.Manager {
		m, err := baseconfig.New(context.Background(), baseconfig.Config{
			StateDir:   stateDir,
			Live:       live,
			Enumerator: baseconfig.NewPersisEnumerator(repo),
			Builder:    baseconfig.NewSpecBuilder(),
		})
		require.NoError(t, err)
		return m
	}
	m := newManager()
	ctx := context.Background()

	// A breaking global candidate is rejected for every workflow; live stays.
	_, err := m.Submit(ctx, baseconfig.GlobalScope(), []byte("schedule: \"definitely-not-a-schedule\"\n"))
	require.Error(t, err)
	rejected, ok := baseconfig.AsRejection(err)
	require.True(t, ok)
	assert.Len(t, rejected.Failures, 4)
	current, err := os.ReadFile(basePath)
	require.NoError(t, err)
	assert.Equal(t, "schedule: \"0 2 * * *\"\n", string(current))

	// A valid candidate promotes; the digest survives a restart.
	outcome, err := m.Submit(ctx, baseconfig.GlobalScope(), []byte("schedule: \"0 3 * * *\"\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), outcome.Generation)
	promoted, err := os.ReadFile(basePath)
	require.NoError(t, err)
	assert.Equal(t, "schedule: \"0 3 * * *\"\n", string(promoted))

	// Restart reconciles the promoted generation; the next successful submit
	// continues at generation 2.
	restarted := newManager()
	outcome, err = restarted.Submit(ctx, baseconfig.GlobalScope(), []byte("schedule: \"0 4 * * *\"\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(2), outcome.Generation)

	// A breaking workspace candidate is rejected and names only that workflow.
	wsScope, err := baseconfig.WorkspaceScope("alpha")
	require.NoError(t, err)
	_, err = restarted.Submit(ctx, wsScope, []byte("schedule: \"also-bad\"\n"))
	require.Error(t, err)
	rejected, ok = baseconfig.AsRejection(err)
	require.True(t, ok)
	require.Len(t, rejected.Failures, 2)
	wsFailureNames := map[string]bool{}
	for _, failure := range rejected.Failures {
		wsFailureNames[failure.Name] = true
	}
	assert.True(t, wsFailureNames["alpha-wf"])
	assert.True(t, wsFailureNames["team-plain"])
	alphaCurrent, err := os.ReadFile(alphaPath)
	require.NoError(t, err)
	assert.Contains(t, string(alphaCurrent), "ALPHA_VAR")
}
