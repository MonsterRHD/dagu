// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package baseconfig

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeLiveStores struct {
	mu       sync.Mutex
	content  map[string][]byte
	failWrite map[string]error
	writes   []string
}

func newFakeLiveStores(global []byte) *fakeLiveStores {
	f := &fakeLiveStores{
		content:   map[string][]byte{"": append([]byte(nil), global...)},
		failWrite: map[string]error{},
	}
	return f
}

func (f *fakeLiveStores) Global() LiveStore { return &fakeLiveStore{f: f, scope: ""} }

func (f *fakeLiveStores) Workspace(name string) (LiveStore, error) {
	return &fakeLiveStore{f: f, scope: name}, nil
}

func (f *fakeLiveStores) set(scope string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.content[scope] = append([]byte(nil), content...)
}

func (f *fakeLiveStores) get(scope string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.content[scope]...)
}

type fakeLiveStore struct {
	f     *fakeLiveStores
	scope string
}

func (s *fakeLiveStore) Read(context.Context) ([]byte, error) {
	return s.f.get(s.scope), nil
}

func (s *fakeLiveStore) Write(_ context.Context, content []byte) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if err := s.f.failWrite[s.scope]; err != nil {
		return err
	}
	s.f.content[s.scope] = append([]byte(nil), content...)
	s.f.writes = append(s.f.writes, s.scope)
	return nil
}

func newTestManager(t *testing.T, live *fakeLiveStores) *Manager {
	t.Helper()
	clock := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	m, err := New(context.Background(), Config{
		StateDir: t.TempDir(),
		Live:     live,
		Now:      func() time.Time { return clock },
	})
	require.NoError(t, err)
	return m
}

func (m *Manager) plantCandidate(t *testing.T, rec candidateRecord) {
	t.Helper()
	require.NoError(t, m.writeCandidateRecord(rec))
}

func TestRecover_InitializesGenerationFromLive(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v0\n"))
	m := newTestManager(t, live)

	state := m.loadState()
	assert.Equal(t, int64(0), state.Generation)
	assert.Equal(t, digestOf([]byte("global: v0\n")), state.GlobalDigest)
	assert.True(t, state.ConfirmedAt.IsZero() == false)
}

func TestRecover_DiscardsCandidateWhenRenameDidNotHappen(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	m := newTestManager(t, live)
	initial := m.loadState()

	m.plantCandidate(t, candidateRecord{
		Generation: 1,
		Content:    []byte("global: v2\n"),
		BaseDigest: digestOf([]byte("global: v1\n")),
		CreatedAt:  m.now(),
	})

	// Restart with the live file still at the old content.
	restarted, err := New(context.Background(), Config{StateDir: m.stateDir, Live: live, Now: m.now})
	require.NoError(t, err)

	state := restarted.loadState()
	assert.Equal(t, int64(0), state.Generation)
	assert.Equal(t, initial.GlobalDigest, state.GlobalDigest)

	entries, err := os.ReadDir(restarted.candidatesDir())
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestRecover_AdoptsCandidateWhenRenameHappened(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	m := newTestManager(t, live)

	m.plantCandidate(t, candidateRecord{
		Generation: 1,
		Workspace: "ops",
		Content:   []byte("env:\n  OPS: v2\n"),
		BaseDigest: digestOf(nil),
		CreatedAt:  m.now(),
	})
	// Simulate a crash after the atomic rename but before the state write.
	live.set("ops", []byte("env:\n  OPS: v2\n"))

	restarted, err := New(context.Background(), Config{StateDir: m.stateDir, Live: live, Now: m.now})
	require.NoError(t, err)

	state := restarted.loadState()
	assert.Equal(t, int64(1), state.Generation)
	assert.Equal(t, digestOf([]byte("env:\n  OPS: v2\n")), state.WorkspaceDigests["ops"])
	assert.Equal(t, digestOf([]byte("global: v1\n")), state.GlobalDigest)

	entries, err := os.ReadDir(restarted.candidatesDir())
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestRecover_ReconcilesStaleDigestRecord(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v1\n"))
	m := newTestManager(t, live)

	stale := m.loadState()
	stale.WorkspaceDigests["ops"] = digestOf([]byte("old"))
	require.NoError(t, m.saveState(stale))
	live.set("ops", []byte("env:\n  OPS: current\n"))

	restarted, err := New(context.Background(), Config{StateDir: m.stateDir, Live: live, Now: m.now})
	require.NoError(t, err)

	state := restarted.loadState()
	assert.Equal(t, digestOf([]byte("env:\n  OPS: current\n")), state.WorkspaceDigests["ops"])
	assert.Equal(t, int64(0), state.Generation)
}

func TestRecover_RebuildsCorruptStateFromLive(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v9\n"))
	m := newTestManager(t, live)
	require.NoError(t, os.WriteFile(m.statePath(), []byte("{not-json"), 0o600))

	restarted, err := New(context.Background(), Config{StateDir: m.stateDir, Live: live, Now: m.now})
	require.NoError(t, err)

	state := restarted.loadState()
	assert.Equal(t, digestOf([]byte("global: v9\n")), state.GlobalDigest)
}

func TestRecover_DiscardsCorruptCandidate(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores(nil)
	m := newTestManager(t, live)
	require.NoError(t, os.MkdirAll(m.candidatesDir(), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(m.candidatesDir(), "00000000000000000001-global.json"),
		[]byte("garbage"), 0o600))

	restarted, err := New(context.Background(), Config{StateDir: m.stateDir, Live: live, Now: m.now})
	require.NoError(t, err)

	entries, err := os.ReadDir(restarted.candidatesDir())
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestRecover_AppliesMultipleInterruptedCandidatesInOrder(t *testing.T) {
	t.Parallel()
	live := newFakeLiveStores([]byte("global: v2\n"))
	m := newTestManager(t, live)

	// Gen 1 candidate was not renamed; gen 2 was.
	m.plantCandidate(t, candidateRecord{Generation: 1, Content: []byte("global: vX\n"), CreatedAt: m.now()})
	m.plantCandidate(t, candidateRecord{Generation: 2, Content: []byte("global: v2\n"), CreatedAt: m.now()})

	restarted, err := New(context.Background(), Config{StateDir: m.stateDir, Live: live, Now: m.now})
	require.NoError(t, err)

	state := restarted.loadState()
	assert.Equal(t, int64(2), state.Generation)
	assert.Equal(t, digestOf([]byte("global: v2\n")), state.GlobalDigest)
	entries, err := os.ReadDir(restarted.candidatesDir())
	require.NoError(t, err)
	assert.Empty(t, entries)
}
