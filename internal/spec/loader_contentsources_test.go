// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package spec_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dagucloud/dagu/v2/internal/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WithBaseConfigContentSources must feed the same per-document merge pipeline
// that file-backed base configs use, so candidate validation observes exactly
// what promotion would publish.
func TestWithBaseConfigContentSources_PerDocumentMerge(t *testing.T) {
	t.Parallel()

	candidateGlobal := []byte(`
env:
  GLOBAL_ONLY: "global-v2"
  SHARED: "global"
log_dir: "/global/logs-v2"
`)
	workspaceA := []byte(`
env:
  A_ONLY: "a"
  SHARED: "a"
`)
	workspaceB := []byte(`
env:
  B_ONLY: "b"
  SHARED: "b"
`)

	// The first document sets the file-level base; later documents only switch
	// the effective base when they carry their own workspace label.
	multiDoc := createTempYAMLFile(t, `
name: dag-plain
steps:
  - name: plain-step
    run: echo plain
---
name: dag-a
labels:
  - workspace=alpha
steps:
  - name: a-step
    run: echo a
---
name: dag-b
labels:
  - workspace=bravo
steps:
  - name: b-step
    run: echo b
`)

	lookup := func(name string) ([]byte, bool) {
		switch name {
		case "alpha":
			return workspaceA, true
		case "bravo":
			return workspaceB, true
		default:
			return nil, false
		}
	}

	dag, err := spec.Load(context.Background(), multiDoc,
		spec.WithBaseConfigContentSources(spec.BaseConfigContentSources{
			GlobalContent:     candidateGlobal,
			WorkspaceOverride: lookup,
		}),
	)
	require.NoError(t, err)

	// First (unlabelled) document inherits the candidate global only.
	assert.Equal(t, "dag-plain", dag.Name)
	assert.Contains(t, dag.Env, "GLOBAL_ONLY=global-v2")
	assert.Contains(t, dag.Env, "SHARED=global")
	assert.NotContains(t, dag.Env, "A_ONLY=a")
	assert.NotContains(t, dag.Env, "B_ONLY=b")
	assert.Equal(t, "/global/logs-v2", dag.LogDir)
	assert.Contains(t, string(dag.BaseConfigData), "global-v2")

	// Second document: candidate global merged with workspace alpha override.
	dagA, ok := dag.LocalDAGs["dag-a"]
	require.True(t, ok)
	assert.Contains(t, dagA.Env, "GLOBAL_ONLY=global-v2")
	assert.Contains(t, dagA.Env, "A_ONLY=a")
	assert.Contains(t, dagA.Env, "SHARED=a")
	assert.NotContains(t, dagA.Env, "B_ONLY=b")
	assert.Contains(t, string(dagA.BaseConfigData), "A_ONLY")
	assert.NotContains(t, string(dagA.BaseConfigData), "B_ONLY")

	// Third document resolves the bravo override independently.
	dagB, ok := dag.LocalDAGs["dag-b"]
	require.True(t, ok)
	assert.Contains(t, dagB.Env, "GLOBAL_ONLY=global-v2")
	assert.Contains(t, dagB.Env, "B_ONLY=b")
	assert.Contains(t, dagB.Env, "SHARED=b")
	assert.NotContains(t, dagB.Env, "A_ONLY=a")
	assert.Contains(t, string(dagB.BaseConfigData), "B_ONLY")
}

func TestWithBaseConfigContentSources_MissingOverrideFallsBackToGlobal(t *testing.T) {
	t.Parallel()

	dagFile := createTempYAMLFile(t, `
name: dag-x
labels:
  - workspace=ghost
steps:
  - name: x-step
    run: echo x
`)

	dag, err := spec.Load(context.Background(), dagFile,
		spec.WithBaseConfigContentSources(spec.BaseConfigContentSources{
			GlobalContent: []byte(`
env:
  GLOBAL_ONLY: "global"
`),
			WorkspaceOverride: func(string) ([]byte, bool) { return nil, false },
		}),
	)
	require.NoError(t, err)
	assert.Contains(t, dag.Env, "GLOBAL_ONLY=global")
}

// Content sources take precedence over any configured file locations, since
// validating a candidate must never read live files for base config content.
func TestWithBaseConfigContentSources_TakesPrecedenceOverFilePaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	liveBase := filepath.Join(root, "base.yaml")
	require.NoError(t, os.WriteFile(liveBase, []byte(`
env:
  LIVE_ONLY: "live"
`), 0600))

	dagFile := createTempYAMLFile(t, `
name: dag-y
steps:
  - name: y-step
    run: echo y
`)

	dag, err := spec.Load(context.Background(), dagFile,
		spec.WithBaseConfig(liveBase),
		spec.WithWorkspaceBaseConfigDir(root),
		spec.WithBaseConfigContentSources(spec.BaseConfigContentSources{
			GlobalContent: []byte(`
env:
  CANDIDATE_ONLY: "candidate"
`),
		}),
	)
	require.NoError(t, err)
	assert.Contains(t, dag.Env, "CANDIDATE_ONLY=candidate")
	assert.NotContains(t, dag.Env, "LIVE_ONLY=live")
}

func TestWorkspaceLabelsFromDocuments_CoversAllDocuments(t *testing.T) {
	t.Parallel()

	names := spec.WorkspaceLabelsFromDocuments([]byte(`
name: root
steps:
  - name: s
    run: echo r
---
name: child-a
labels:
  - workspace=alpha
steps:
  - name: s
    run: echo a
---
name: child-b
labels:
  - workspace=bravo
steps:
  - name: s
    run: echo b
---
name: child-duplicate
labels:
  - workspace=alpha
steps:
  - name: s
    run: echo d
`))
	assert.Equal(t, []string{"alpha", "bravo"}, names)

	assert.Empty(t, spec.WorkspaceLabelsFromDocuments([]byte("name: broken\n")))
	assert.Empty(t, spec.WorkspaceLabelsFromDocuments(nil))
}

func TestWithBaseConfigContentSources_EmptyGlobalMeansNoBase(t *testing.T) {
	t.Parallel()

	dagFile := createTempYAMLFile(t, `
name: dag-z
steps:
  - name: z-step
    run: echo z
`)

	dag, err := spec.Load(context.Background(), dagFile,
		spec.WithBaseConfigContentSources(spec.BaseConfigContentSources{
			GlobalContent: nil,
		}),
		spec.WithoutEval(),
	)
	require.NoError(t, err)
	assert.Empty(t, dag.BaseConfigData)
}
