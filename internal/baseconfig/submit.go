// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package baseconfig

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dagucloud/dagu/v2/internal/cmn/dirlock"
)

const (
	defaultBuildConcurrency   = 8
	defaultLockStaleThreshold = 10 * time.Minute
)

// WorkflowSource is one workflow affected by a candidate.
type WorkflowSource struct {
	WorkflowRef
	// Source is the authored YAML content (the whole file; multi-document
	// files keep every document).
	Source []byte
	// SourcePath is the authored file path, used for file-relative resolution.
	SourcePath string
	// Workspaces lists every valid workspace label across all documents of the
	// source file. WorkflowRef.Workspace is the root document's label and is
	// always included here when set.
	Workspaces []string
}

// Enumerator lists the workflows a candidate must be rebuilt against.
// A global scope lists every workflow; a workspace scope lists only the
// workflows carrying that workspace label.
type Enumerator interface {
	ListAffected(ctx context.Context, scope Scope) ([]WorkflowSource, error)
}

// Builder rebuilds one workflow against the supplied effective base config.
// globalContent is the effective global base (empty when absent), and
// workspaceOverride resolves each document's workspace override. A nil error
// means the workflow builds and validates successfully.
type Builder interface {
	Build(
		ctx context.Context,
		workflow WorkflowSource,
		globalContent []byte,
		workspaceOverride func(workspaceName string) (content []byte, ok bool),
	) error
}

// ErrFence is returned when the live configuration changed after the
// candidate snapshot was taken. The caller may retry with a fresh candidate.
var ErrFence = errors.New("base config generation changed during validation; please retry")

// Submit validates a candidate against every affected workflow and promotes it
// atomically when no healthy workflow regresses. On rejection or publish
// failure the live configuration stays on the previous generation.
func (m *Manager) Submit(ctx context.Context, scope Scope, candidate []byte) (*Outcome, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if m.enumerator == nil || m.builder == nil {
		return nil, errors.New("baseconfig: enumerator and builder are required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var outcome *Outcome
	err := m.withProcessLock(ctx, func(stillHeld func() bool) error {
		var inner error
		outcome, inner = m.submitLocked(ctx, scope, candidate, stillHeld)
		return inner
	})
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

func (m *Manager) submitLocked(
	ctx context.Context,
	scope Scope,
	candidate []byte,
	stillHeld func() bool,
) (*Outcome, error) {
	state := m.loadState()

	target, err := m.liveStoreFor(scope)
	if err != nil {
		return nil, err
	}
	currentTarget, err := target.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("read current base config: %w", err)
	}
	fenceDigest := digestOf(currentTarget)
	if recorded := state.digestFor(scope); recorded != "" && recorded != fenceDigest {
		// Live content moved ahead of the record (hand edit or a crash between
		// rename and record write). The lock guarantees no cooperating
		// promotion is in flight, so adopt the live truth once and re-snapshot;
		// the pre-publish fence still guards changes during validation.
		state, err = m.adoptLiveDrift(ctx, state)
		if err != nil {
			return nil, err
		}
		currentTarget, err = target.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read current base config after reconciliation: %w", err)
		}
		fenceDigest = digestOf(currentTarget)
	}

	record := candidateRecord{
		Generation: state.Generation + 1,
		Workspace:  scope.Workspace,
		Content:    append([]byte(nil), candidate...),
		BaseDigest: fenceDigest,
		CreatedAt:  m.now().UTC(),
	}
	if err := m.writeCandidateRecord(record); err != nil {
		return nil, err
	}

	workflows, err := m.enumerator.ListAffected(ctx, scope)
	if err != nil {
		_ = m.removeCandidateRecord(record)
		return nil, fmt.Errorf("list affected workflows: %w", err)
	}
	sort.Slice(workflows, func(i, j int) bool {
		return workflowOrderKey(workflows[i]) < workflowOrderKey(workflows[j])
	})

	verdicts, err := m.evaluate(ctx, scope, candidate, workflows)
	if err != nil {
		_ = m.removeCandidateRecord(record)
		return nil, err
	}

	regressions := make([]WorkflowFailure, 0)
	warnings := make([]string, 0)
	for _, verdict := range verdicts {
		switch {
		case verdict.baselineErr == nil && verdict.candidateErr != nil:
			regressions = append(regressions, WorkflowFailure{
				WorkflowRef: verdict.workflow.WorkflowRef,
				Reason:      verdict.candidateErr.Error(),
			})
		case verdict.baselineErr != nil && verdict.candidateErr != nil:
			warnings = append(warnings, fmt.Sprintf(
				"workflow %q (%s) already fails under the current base config: %s",
				verdict.workflow.displayName(), verdict.workflow.Location, verdict.baselineErr.Error()))
		}
	}

	if len(regressions) > 0 {
		sort.Slice(regressions, func(i, j int) bool {
			return workflowOrderKey(WorkflowSource{WorkflowRef: regressions[i].WorkflowRef}) <
				workflowOrderKey(WorkflowSource{WorkflowRef: regressions[j].WorkflowRef})
		})
		_ = m.removeCandidateRecord(record)
		return nil, &RejectionError{Scope: scope, Failures: regressions}
	}

	// Fence: a confirmed publish from another writer must not be overwritten
	// by validation performed against an older generation. Also re-check lock
	// ownership so a stale lock taken over during long validation can not
	// publish.
	if stillHeld != nil && !stillHeld() {
		_ = m.removeCandidateRecord(record)
		return nil, fmt.Errorf("%w: promotion lock for %s was lost", ErrFence, scope)
	}
	if fresh, err := m.liveDigest(ctx, scope); err != nil {
		_ = m.removeCandidateRecord(record)
		return nil, fmt.Errorf("re-read current base config: %w", err)
	} else if fresh != fenceDigest {
		_ = m.removeCandidateRecord(record)
		return nil, fmt.Errorf("%w: %s", ErrFence, scope)
	}

	if err := target.Write(ctx, append([]byte(nil), candidate...)); err != nil {
		_ = m.removeCandidateRecord(record)
		return nil, publishError(scope, err)
	}

	state.Generation = record.Generation
	state.setDigest(scope, digestOf(candidate))
	state.ConfirmedAt = m.now().UTC()
	if err := m.saveState(state); err != nil {
		return nil, publishError(scope, fmt.Errorf("generation record unavailable after publish: %w", err))
	}
	_ = m.removeCandidateRecord(record)

	return &Outcome{Generation: state.Generation, Warnings: warnings}, nil
}

type workflowVerdict struct {
	workflow     WorkflowSource
	baselineErr  error
	candidateErr error
}

// evaluate rebuilds every affected workflow under both the current and the
// candidate generation. Required workspace overrides are read once up front so
// the build workers stay pure and deterministic.
func (m *Manager) evaluate(
	ctx context.Context,
	scope Scope,
	candidate []byte,
	workflows []WorkflowSource,
) ([]workflowVerdict, error) {
	globalContent, err := m.live.Global().Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("read current global base config: %w", err)
	}

	workspaceNames := make(map[string]struct{})
	for _, workflow := range workflows {
		if len(workflow.Workspaces) > 0 {
			for _, name := range workflow.Workspaces {
				workspaceNames[name] = struct{}{}
			}
			continue
		}
		if workflow.Workspace != "" {
			workspaceNames[workflow.Workspace] = struct{}{}
		}
	}
	workspaceContents := make(map[string][]byte, len(workspaceNames))
	for name := range workspaceNames {
		store, err := m.live.Workspace(name)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace base config %q: %w", name, err)
		}
		content, err := store.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read workspace base config %q: %w", name, err)
		}
		workspaceContents[name] = content
	}

	baselineLookup := func(name string) ([]byte, bool) {
		content, ok := workspaceContents[name]
		return content, ok && len(content) > 0
	}
	candidateGlobal := globalContent
	if scope.IsGlobal() {
		candidateGlobal = candidate
	}
	candidateLookup := func(name string) ([]byte, bool) {
		if !scope.IsGlobal() && name == scope.Workspace {
			return candidate, true
		}
		content, ok := workspaceContents[name]
		return content, ok && len(content) > 0
	}

	workers := max(min(m.concurrency, len(workflows)), 1)

	jobs := make(chan int)
	results := make([]workflowVerdict, len(workflows))
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for index := range jobs {
				workflow := workflows[index]
				baseErr := m.builder.Build(ctx, workflow, globalContent, baselineLookup)
				candErr := m.builder.Build(ctx, workflow, candidateGlobal, candidateLookup)
				results[index] = workflowVerdict{
					workflow:     workflow,
					baselineErr:  baseErr,
					candidateErr: candErr,
				}
			}
		})
	}
	for i := range workflows {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()

	return results, nil
}

func workflowOrderKey(source WorkflowSource) string {
	if source.Location != "" {
		return source.Location + "\x00" + source.Name
	}
	return source.Name
}

// withProcessLock runs fn while holding the cross-process promotion lock. fn
// receives an ownership probe it must consult before publishing.
func (m *Manager) withProcessLock(ctx context.Context, fn func(stillHeld func() bool) error) error {
	lock := dirlock.New(m.stateDir, &dirlock.LockOptions{
		StaleThreshold: m.lockThreshold,
	})
	if err := lock.Lock(ctx); err != nil {
		return fmt.Errorf("acquire base config promotion lock: %w", err)
	}
	stillHeld := lock.IsHeldByMe

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Go(func() {
		ticker := time.NewTicker(m.lockThreshold / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = lock.Heartbeat(heartbeatCtx)
			case <-heartbeatCtx.Done():
				return
			}
		}
	})

	runErr := fn(stillHeld)
	stopHeartbeat()
	heartbeatWG.Wait()
	if err := lock.Unlock(); err != nil && runErr == nil {
		runErr = fmt.Errorf("release base config promotion lock: %w", err)
	}
	return runErr
}
