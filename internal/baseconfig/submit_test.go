// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package baseconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type buildCall struct {
	workflow    WorkflowSource
	global      string
	workspace   string
	override    string
	hasOverride bool
}

type fakeEnumerator struct {
	mu        sync.Mutex
	workflows []WorkflowSource
	scopes    []Scope
	err       error
}

func (e *fakeEnumerator) ListAffected(_ context.Context, scope Scope) ([]WorkflowSource, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scopes = append(e.scopes, scope)
	if e.err != nil {
		return nil, e.err
	}
	return append([]WorkflowSource(nil), e.workflows...), nil
}

type fakeBuilder struct {
	mu                     sync.Mutex
	calls                  []buildCall
	failWhenContentContain string // marker in the effective global or override content
	failAlways             map[string]bool
	delay                  time.Duration
	externalTrigger        string
	externalWrite          func()
}

func (b *fakeBuilder) Build(_ context.Context, wf WorkflowSource, global []byte, lookup func(string) ([]byte, bool)) error {
	call := buildCall{workflow: wf, global: string(global)}
	effective := string(global)
	if wf.Workspace != "" {
		content, ok := lookup(wf.Workspace)
		call.workspace = wf.Workspace
		call.override = string(content)
		call.hasOverride = ok
		effective += "\n" + string(content)
	}
	b.mu.Lock()
	b.calls = append(b.calls, call)
	marker := b.failWhenContentContain
	trigger := b.externalTrigger
	delay := b.delay
	external := b.externalWrite
	failAlways := b.failAlways[wf.Name]
	b.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if external != nil && trigger != "" && strings.Contains(effective, trigger) {
		external()
	}
	if failAlways {
		return fmt.Errorf("broken %s", wf.Name)
	}
	if marker != "" && strings.Contains(effective, marker) {
		return fmt.Errorf("candidate broke %s", wf.Name)
	}
	return nil
}

func newSubmitManager(t *testing.T, live *fakeLiveStores, en *fakeEnumerator, b *fakeBuilder) *Manager {
	t.Helper()
	m, err := New(context.Background(), Config{
		StateDir:    t.TempDir(),
		Live:        live,
		Enumerator:  en,
		Builder:     b,
		Concurrency: 4,
	})
	require.NoError(t, err)
	return m
}

func wf(name, location, workspace string) WorkflowSource {
	return WorkflowSource{
		WorkflowRef: WorkflowRef{Name: name, Location: location, Workspace: workspace},
		Source:      []byte("name: " + name + "\nsteps: []\n"),
		SourcePath:  location,
	}
}

func candidateFileNames(t *testing.T, m *Manager) []string {
	t.Helper()
	entries, err := os.ReadDir(m.candidatesDir())
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestSubmit_RejectsRegressionWithoutTouchingLive(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	live.set("alpha", []byte("env:\n  A: cur\n"))
	en := &fakeEnumerator{workflows: []WorkflowSource{
		wf("wf-a", "/dags/a.yaml", ""),
		wf("wf-b", "/dags/sub/b.yaml", "alpha"),
	}}
	b := &fakeBuilder{failWhenContentContain: "v2-bad"}
	m := newSubmitManager(t, live, en, b)

	outcome, err := m.Submit(context.Background(), GlobalScope(), []byte("global: v2-bad\n"))
	require.Error(t, err)
	assert.Nil(t, outcome)
	rejected, ok := AsRejection(err)
	require.True(t, ok)
	assert.Len(t, rejected.Failures, 2)
	msg := err.Error()
	assert.Contains(t, msg, "wf-a")
	assert.Contains(t, msg, "/dags/a.yaml")
	assert.Contains(t, msg, "wf-b")
	assert.Contains(t, msg, "/dags/sub/b.yaml")

	assert.Equal(t, "global: v1\n", string(live.get("")))
	assert.Empty(t, live.writes)
	assert.Equal(t, int64(0), m.loadState().Generation)
	assert.Empty(t, candidateFileNames(t, m))

	var sawCandidateA, sawCandidateB bool
	for _, call := range b.calls {
		if call.global != "global: v2-bad\n" {
			continue
		}
		switch call.workflow.Name {
		case "wf-a":
			sawCandidateA = true
		case "wf-b":
			sawCandidateB = true
			assert.Equal(t, "alpha", call.workspace)
			assert.Contains(t, call.override, "A: cur")
		}
	}
	assert.True(t, sawCandidateA && sawCandidateB)
}

func TestSubmit_WorkspaceScopeUsesCandidateOverrideOnlyForTarget(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	live.set("alpha", []byte("env:\n  A: cur\n"))
	en := &fakeEnumerator{workflows: []WorkflowSource{wf("wf-a", "/dags/a.yaml", "alpha")}}
	b := &fakeBuilder{failWhenContentContain: "A: new-bad"}
	m := newSubmitManager(t, live, en, b)

	_, err := m.Submit(context.Background(), mustScope(t, "alpha"), []byte("env:\n  A: new-bad\n"))
	require.Error(t, err)
	rejected, ok := AsRejection(err)
	require.True(t, ok)
	require.Len(t, rejected.Failures, 1)
	assert.Equal(t, "wf-a", rejected.Failures[0].Name)

	var sawCandidate, sawBaseline bool
	for _, call := range b.calls {
		if call.workspace != "alpha" {
			continue
		}
		if call.override == "env:\n  A: new-bad\n" && call.global == "global: v1\n" {
			sawCandidate = true
		}
		if call.override == "env:\n  A: cur\n" {
			sawBaseline = true
		}
	}
	assert.True(t, sawCandidate && sawBaseline)
	assert.Equal(t, "env:\n  A: cur\n", string(live.get("alpha")))
}

func TestSubmit_PreExistingFailuresAreWarningsNotBlockers(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	en := &fakeEnumerator{workflows: []WorkflowSource{
		wf("wf-ok", "/dags/ok.yaml", ""),
		wf("wf-broken", "/dags/broken.yaml", ""),
	}}
	b := &fakeBuilder{failAlways: map[string]bool{"wf-broken": true}}
	m := newSubmitManager(t, live, en, b)

	outcome, err := m.Submit(context.Background(), GlobalScope(), []byte("global: v2\n"))
	require.NoError(t, err)
	require.NotNil(t, outcome)
	assert.Equal(t, int64(1), outcome.Generation)
	require.Len(t, outcome.Warnings, 1)
	assert.Contains(t, outcome.Warnings[0], "wf-broken")
	assert.Contains(t, outcome.Warnings[0], "/dags/broken.yaml")
	assert.NotContains(t, outcome.Warnings[0], "reject")
	assert.Equal(t, "global: v2\n", string(live.get("")))
	assert.Equal(t, int64(1), m.loadState().Generation)
}

func TestSubmit_ImprovedWorkflowProducesNoWarning(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1-bad\n"))
	en := &fakeEnumerator{workflows: []WorkflowSource{wf("wf-a", "/dags/a.yaml", "")}}
	b := &fakeBuilder{failWhenContentContain: "v1-bad"}
	m := newSubmitManager(t, live, en, b)

	outcome, err := m.Submit(context.Background(), GlobalScope(), []byte("global: v2-ok\n"))
	require.NoError(t, err)
	assert.Empty(t, outcome.Warnings)
	assert.Equal(t, int64(1), outcome.Generation)
}

func TestSubmit_PublishFailureKeepsLiveAndReleasesLock(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	live.failWrite[""] = errors.New("disk full")
	en := &fakeEnumerator{workflows: []WorkflowSource{wf("wf-a", "/dags/a.yaml", "")}}
	b := &fakeBuilder{}
	m := newSubmitManager(t, live, en, b)

	_, err := m.Submit(context.Background(), GlobalScope(), []byte("global: v2\n"))
	require.Error(t, err)
	assert.True(t, AsPublishError(err))
	assert.ErrorIs(t, err, ErrPublish)
	assert.Equal(t, "global: v1\n", string(live.get("")))

	// Lock must be released: a follow-up submit after the fault clears works.
	live.failWrite = map[string]error{}
	outcome, err := m.Submit(context.Background(), GlobalScope(), []byte("global: v3\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), outcome.Generation)
	assert.Equal(t, "global: v3\n", string(live.get("")))
}

func TestSubmit_FenceBlocksOverwriteOfNewerContent(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	en := &fakeEnumerator{workflows: []WorkflowSource{wf("wf-a", "/dags/a.yaml", "")}}
	b := &fakeBuilder{
		externalTrigger: "candidate",
		externalWrite: func() {
			// Simulate a non-cooperating writer publishing newer content while
			// this candidate is still being validated.
			live.set("", []byte("global: external-newer\n"))
		},
	}
	m := newSubmitManager(t, live, en, b)

	_, err := m.Submit(context.Background(), GlobalScope(), []byte("global: candidate\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFence)
	assert.Equal(t, "global: external-newer\n", string(live.get("")))
	assert.Empty(t, candidateFileNames(t, m))
}

func TestSubmit_AdoptsExternalLiveDriftInsteadOfHardFence(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	en := &fakeEnumerator{workflows: []WorkflowSource{wf("wf-a", "/dags/a.yaml", "")}}
	b := &fakeBuilder{}
	m := newSubmitManager(t, live, en, b)

	// A non-cooperating hand edit moves live ahead of generation.json.
	live.set("", []byte("global: hand-edited\n"))

	outcome, err := m.Submit(context.Background(), GlobalScope(), []byte("global: v2\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), outcome.Generation)
	assert.Equal(t, "global: v2\n", string(live.get("")))

	// Baseline builds must have run against the drifted live content.
	sawDriftedBaseline := false
	for _, call := range b.calls {
		if call.workflow.Name == "wf-a" && call.global == "global: hand-edited\n" {
			sawDriftedBaseline = true
		}
	}
	assert.True(t, sawDriftedBaseline)
	assert.Equal(t, digestOf([]byte("global: v2\n")), m.loadState().GlobalDigest)
}

func TestSubmit_EmptyCandidateClearsConfig(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	en := &fakeEnumerator{workflows: []WorkflowSource{wf("wf-a", "/dags/a.yaml", "")}}
	b := &fakeBuilder{}
	m := newSubmitManager(t, live, en, b)

	outcome, err := m.Submit(context.Background(), GlobalScope(), nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), outcome.Generation)
	assert.Empty(t, live.get(""))
	assert.Equal(t, digestOf(nil), m.loadState().GlobalDigest)
}

func TestSubmit_InvalidScopeRejected(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores(nil)
	m := newSubmitManager(t, live, &fakeEnumerator{}, &fakeBuilder{})

	_, err := m.Submit(context.Background(), Scope{Workspace: "bad/name"}, []byte("x"))
	require.Error(t, err)
}

func TestSubmit_ConcurrentManagersConvergeInGenerationOrder(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v0\n"))
	live.set("alpha", []byte("alpha: v0\n"))
	stateDir := t.TempDir()

	newManager := func() (*Manager, *fakeEnumerator) {
		en := &fakeEnumerator{workflows: []WorkflowSource{wf("wf-a", "/dags/a.yaml", "alpha")}}
		b := &fakeBuilder{delay: 15 * time.Millisecond}
		m, err := New(context.Background(), Config{
			StateDir: stateDir, Live: live, Enumerator: en, Builder: b,
		})
		require.NoError(t, err)
		return m, en
	}
	m1, _ := newManager()
	m2, en2 := newManager()

	var wg sync.WaitGroup
	results := make(chan *Outcome, 3)
	errCh := make(chan error, 3)
	submit := func(m *Manager, scope Scope, content []byte) {
		wg.Go(func() {
			outcome, err := m.Submit(context.Background(), scope, content)
			if err != nil {
				errCh <- err
				return
			}
			results <- outcome
		})
	}
	submit(m1, GlobalScope(), []byte("global: g1\n"))
	submit(m2, GlobalScope(), []byte("global: g2\n"))
	submit(m1, mustScope(t, "alpha"), []byte("alpha: w1\n"))
	wg.Wait()
	close(results)
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	gens := map[int64]bool{}
	for outcome := range results {
		assert.False(t, gens[outcome.Generation], "duplicate generation")
		gens[outcome.Generation] = true
	}
	assert.Len(t, gens, 3)
	state := m1.loadState()
	assert.Equal(t, int64(3), state.Generation)
	assert.Equal(t, digestOf(live.get("")), state.GlobalDigest)
	assert.Equal(t, digestOf(live.get("alpha")), state.WorkspaceDigests["alpha"])
	assert.Contains(t, []string{"global: g1\n", "global: g2\n"}, string(live.get("")))
	assert.Equal(t, "alpha: w1\n", string(live.get("alpha")))
	assert.NotEmpty(t, en2.scopes)
}

func TestSubmit_PrereadsAllWorkspacesInMultiLabelSource(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	live.set("alpha", []byte("env:\n  A: cur\n"))
	live.set("bravo", []byte("env:\n  B: cur\n"))
	en := &fakeEnumerator{workflows: []WorkflowSource{{
		WorkflowRef: WorkflowRef{Name: "crew", Location: "/dags/crew.yaml", Workspace: "alpha"},
		Workspaces:   []string{"alpha", "bravo"},
	}}}

	var mu sync.Mutex
	var observed []string
	builder := funcBuilder(func(_ context.Context, wf WorkflowSource, global []byte, lookup func(string) ([]byte, bool)) error {
		if string(global) != "global: v1\n" {
			return nil // only inspect builds against the current global
		}
		mu.Lock()
		defer mu.Unlock()
		if wf.Name != "crew" {
			return nil
		}
		if content, ok := lookup("bravo"); ok {
			observed = append(observed, "bravo:"+string(content))
		}
		if content, ok := lookup("alpha"); ok {
			observed = append(observed, "alpha:"+string(content))
		}
		return nil
	})
	m, err := New(context.Background(), Config{
		StateDir:    t.TempDir(),
		Live:        live,
		Enumerator:  en,
		Builder:     builder,
	})
	require.NoError(t, err)

	outcome, err := m.Submit(context.Background(), mustScope(t, "alpha"), []byte("env:\n  A: new\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), outcome.Generation)
	assert.Contains(t, observed, "bravo:env:\n  B: cur\n",
		"the non-candidate bravo document must receive live bravo override, not an empty merge")
	assert.Contains(t, observed, "alpha:env:\n  A: cur\n",
		"baseline build must read the current alpha override")
}

// funcBuilder adapts a function to the Builder interface.
type funcBuilder func(context.Context, WorkflowSource, []byte, func(string) ([]byte, bool)) error

func (f funcBuilder) Build(ctx context.Context, wf WorkflowSource, global []byte, lookup func(string) ([]byte, bool)) error {
	return f(ctx, wf, global, lookup)
}

func TestSubmit_RejectionCleansCandidatesAndAllowsImmediateRetry(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	en := &fakeEnumerator{workflows: []WorkflowSource{wf("wf-a", "/dags/a.yaml", "")}}
	b := &fakeBuilder{failWhenContentContain: "bad"}
	m := newSubmitManager(t, live, en, b)

	_, err := m.Submit(context.Background(), GlobalScope(), []byte("global: bad\n"))
	require.Error(t, err)
	_, ok := AsRejection(err)
	require.True(t, ok)

	b.failWhenContentContain = "never-used-marker"
	outcome, err := m.Submit(context.Background(), GlobalScope(), []byte("global: good\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), outcome.Generation)
}

func mustScope(t *testing.T, name string) Scope {
	t.Helper()
	scope, err := WorkspaceScope(name)
	require.NoError(t, err)
	return scope
}
