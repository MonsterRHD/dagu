// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package baseconfig

import (
	"context"
	"errors"
	"sync"
	"time"
)

// LiveStore reads and durably writes one base config location (the global
// file or one workspace override). Implementations must be safe for
// concurrent use, and Write must replace the file atomically and invalidate
// any read caches.
type LiveStore interface {
	// Read returns the current YAML content; an empty slice means no
	// configuration exists.
	Read(ctx context.Context) ([]byte, error)
	// Write publishes the YAML content atomically.
	Write(ctx context.Context, content []byte) error
}

// LiveStores resolves the live stores for the two scope kinds.
type LiveStores interface {
	Global() LiveStore
	Workspace(name string) (LiveStore, error)
}

// Manager serializes candidate promotions and owns the generation record.
type Manager struct {
	stateDir   string
	live       LiveStores
	enumerator Enumerator
	builder    Builder

	// mu serializes promotions within this process. The on-disk lock
	// additionally coordinates other processes sharing the state directory.
	mu            sync.Mutex
	now           func() time.Time
	concurrency   int
	lockThreshold time.Duration
}

// Config constructs a Manager.
type Config struct {
	// StateDir holds the generation record, candidate records, and the
	// coordinating lock. It must not be inside the DAGs directory, the global
	// base config path, or the workspace base config directory.
	StateDir string
	// Live resolves the confirmed base config locations.
	Live LiveStores
	// Enumerator lists the workflows affected by a candidate scope.
	Enumerator Enumerator
	// Builder rebuilds workflows against candidate base config content.
	Builder Builder
	// Now overrides the clock for tests.
	Now func() time.Time
	// Concurrency bounds parallel workflow rebuilds (default: 8).
	Concurrency int
}

// New creates a Manager and immediately reconciles any interrupted switch.
func New(ctx context.Context, cfg Config) (*Manager, error) {
	if cfg.StateDir == "" {
		return nil, errors.New("baseconfig: state directory is required")
	}
	if cfg.Live == nil || cfg.Live.Global() == nil {
		return nil, errors.New("baseconfig: live stores are required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = defaultBuildConcurrency
	}
	m := &Manager{
		stateDir:     cfg.StateDir,
		live:         cfg.Live,
		enumerator:   cfg.Enumerator,
		builder:      cfg.Builder,
		now:          now,
		concurrency:  concurrency,
		lockThreshold: defaultLockStaleThreshold,
	}
	if err := m.recover(ctx); err != nil {
		return nil, err
	}
	return m, nil
}
