// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package build_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dagucloud/dagu/v2/internal/cmn/runenv"

	"github.com/dagucloud/dagu/v2/internal/build"
	"github.com/dagucloud/dagu/v2/internal/ir"
	"github.com/dagucloud/dagu/v2/internal/persis/file/materialization"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareCommitAndReuse(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input.txt")
	secondInputPath := filepath.Join(workingDir, "second.txt")
	outputPath := filepath.Join(workingDir, "output.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))
	require.NoError(t, os.WriteFile(secondInputPath, []byte("second"), 0o600))

	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	request := prepareRequest(workingDir, inputPath, outputPath)
	request.Step.Inputs = append(request.Step.Inputs, ir.StepInputDeclaration{Name: "second", Path: secondInputPath})
	request.Environment[runenv.EnvKeyDAGRunID] = "run-1"

	first, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, first.Evaluate(ctx))
	assert.Equal(t, ir.BuildDecisionExecute, first.Metadata().Decision)
	assert.Equal(t, ir.BuildReasonManifestMissing, first.Metadata().Reason)

	outputs, staging, err := first.NewAttempt(0)
	require.NoError(t, err)
	assert.Equal(t, staging, outputs["artifact"])
	require.NoError(t, os.WriteFile(staging, []byte("result"), 0o600))
	require.NoError(t, first.Commit(ctx, staging))
	require.NoError(t, first.Close(staging))

	request.DAGRunID = "run-2"
	request.AttemptID = "attempt-2"
	request.Environment[runenv.EnvKeyDAGRunID] = "run-2"
	request.Step.Inputs[0], request.Step.Inputs[1] = request.Step.Inputs[1], request.Step.Inputs[0]
	second, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close("")) })
	require.NoError(t, second.Evaluate(ctx))

	assert.True(t, second.Reused())
	assert.Equal(t, ir.BuildReasonMatched, second.Metadata().Reason)
	assert.Equal(t, ir.NewDAGRunRef("build-test", "run-1"), second.Metadata().ProducerRun)
	assert.Equal(t, outputPath, second.PublishedOutputs()["artifact"])
	content, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	assert.Equal(t, "result", string(content))
}

func TestPrepareCommitAndReuseDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory reuse test uses POSIX symlinks")
	}

	ctx := context.Background()
	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input-site")
	outputPath := filepath.Join(workingDir, "output-site")
	require.NoError(t, os.MkdirAll(filepath.Join(inputPath, "empty"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(inputPath, "input.txt"), []byte("input"), 0o600))
	require.NoError(t, os.Symlink("input.txt", filepath.Join(inputPath, "link.txt")))

	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	request := prepareRequest(workingDir, inputPath, outputPath)
	request.Step.Inputs[0].Type = ir.StepPathTypeDirectory
	request.Step.Outputs[0].Type = ir.StepPathTypeDirectory
	request.Environment[runenv.EnvKeyDAGRunID] = "run-1"

	first, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, first.Evaluate(ctx))
	assert.Equal(t, ir.BuildDecisionExecute, first.Metadata().Decision)
	outputs, staging, err := first.NewAttempt(0)
	require.NoError(t, err)
	assert.Equal(t, staging, outputs["artifact"])
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "empty"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "index.html"), []byte("result"), 0o600))
	require.NoError(t, os.Symlink("index.html", filepath.Join(staging, "index-link.html")))
	require.NoError(t, first.Commit(ctx, staging))
	require.NoError(t, first.Close(staging))
	require.NoDirExists(t, staging)
	require.FileExists(t, filepath.Join(outputPath, "index.html"))
	assert.DirExists(t, filepath.Join(outputPath, "empty"))

	request.DAGRunID = "run-2"
	request.AttemptID = "attempt-2"
	request.Environment[runenv.EnvKeyDAGRunID] = "run-2"
	second, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close("")) })
	require.NoError(t, second.Evaluate(ctx))
	assert.True(t, second.Reused())
	assert.Equal(t, ir.NewDAGRunRef("build-test", "run-1"), second.Metadata().ProducerRun)
	content, err := os.ReadFile(filepath.Join(outputPath, "index.html"))
	require.NoError(t, err)
	assert.Equal(t, "result", string(content))
}

func TestDirectoryDryRunNoReuseAndControlDependency(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input-site")
	outputPath := filepath.Join(workingDir, "output-site")
	require.NoError(t, os.MkdirAll(inputPath, 0o750))
	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	request := prepareRequest(workingDir, inputPath, outputPath)
	request.Step.Inputs[0].Type = ir.StepPathTypeDirectory
	request.Step.Outputs[0].Type = ir.StepPathTypeDirectory

	first, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, first.Evaluate(ctx))
	_, staging, err := first.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, "index.html"), []byte("result"), 0o600))
	require.NoError(t, first.Commit(ctx, staging))
	require.NoError(t, first.Close(staging))

	dryRequest := request
	dryRequest.Dry = true
	drySession, err := build.Prepare(ctx, &previewStore{}, dryRequest)
	require.NoError(t, err)
	require.NoError(t, drySession.Evaluate(ctx))
	assert.Equal(t, ir.BuildDecisionExecute, drySession.Metadata().Decision)
	matches, err := filepath.Glob(filepath.Join(workingDir, ".output-site.dagu-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, matches)

	noReuseRequest := request
	noReuseRequest.NoReuse = true
	noReuse, err := build.Prepare(ctx, store, noReuseRequest)
	require.NoError(t, err)
	require.NoError(t, noReuse.Evaluate(ctx))
	assert.Equal(t, ir.BuildReasonReuseDisabled, noReuse.Metadata().Reason)
	require.NoError(t, noReuse.Close(""))

	controlRequest := request
	controlRequest.ControlDependencyRan = true
	control, err := build.Prepare(ctx, store, controlRequest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, control.Close("")) })
	require.NoError(t, control.Evaluate(ctx))
	assert.Equal(t, ir.BuildReasonControlDependencyRan, control.Metadata().Reason)
}

func TestCommitDirectoryRejectsChangedInputAndWrongStagingType(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory commit test uses POSIX symlinks")
	}

	ctx := context.Background()
	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input-site")
	outputPath := filepath.Join(workingDir, "output-site")
	require.NoError(t, os.MkdirAll(filepath.Join(inputPath, "empty"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(inputPath, "input.txt"), []byte("input"), 0o600))
	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	request := prepareRequest(workingDir, inputPath, outputPath)
	request.Step.Inputs[0].Type = ir.StepPathTypeDirectory
	request.Step.Outputs[0].Type = ir.StepPathTypeDirectory

	session, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, session.Evaluate(ctx))
	_, staging, err := session.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(inputPath, "changed.txt"), []byte("changed"), 0o600))
	err = session.Commit(ctx, staging)
	require.ErrorContains(t, err, "changed during execution")
	require.NoFileExists(t, outputPath)
	require.NoError(t, session.Close(staging))

	session, err = build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, session.Evaluate(ctx))
	_, staging, err = session.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.Remove(staging))
	require.NoError(t, os.WriteFile(staging, []byte("not a directory"), 0o600))
	err = session.Commit(ctx, staging)
	require.ErrorContains(t, err, "want directory")
	require.NoFileExists(t, outputPath)
	require.NoError(t, session.Close(staging))
}

func TestDirectoryFileTypeSwitchCommitsAndReuses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input.txt")
	outputPath := filepath.Join(workingDir, "output")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))
	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	request := prepareRequest(workingDir, inputPath, outputPath)

	first, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, first.Evaluate(ctx))
	_, staging, err := first.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(staging, []byte("file"), 0o600))
	require.NoError(t, first.Commit(ctx, staging))
	require.NoError(t, first.Close(staging))

	directoryRequest := request
	directoryRequest.DAGRunID = "run-2"
	directoryRequest.AttemptID = "attempt-2"
	directoryRequest.Step.Outputs[0].Type = ir.StepPathTypeDirectory
	second, err := build.Prepare(ctx, store, directoryRequest)
	require.NoError(t, err)
	require.NoError(t, second.Evaluate(ctx))
	assert.Equal(t, ir.BuildReasonRecipeChanged, second.Metadata().Reason)
	_, staging, err = second.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "partition=1"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "partition=1", "data"), []byte("dir"), 0o600))
	require.NoError(t, second.Commit(ctx, staging))
	require.NoError(t, second.Close(staging))
	assert.DirExists(t, outputPath)

	third, err := build.Prepare(ctx, store, directoryRequest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, third.Close("")) })
	require.NoError(t, third.Evaluate(ctx))
	assert.True(t, third.Reused())

	fileRequest := directoryRequest
	fileRequest.DAGRunID = "run-3"
	fileRequest.AttemptID = "attempt-3"
	fileRequest.Step.Outputs[0].Type = ""
	require.NoError(t, third.Close(""))
	fourth, err := build.Prepare(ctx, store, fileRequest)
	require.NoError(t, err)
	require.NoError(t, fourth.Evaluate(ctx))
	assert.Equal(t, ir.BuildReasonRecipeChanged, fourth.Metadata().Reason)
	_, staging, err = fourth.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(staging, []byte("file again"), 0o600))
	require.NoError(t, fourth.Commit(ctx, staging))
	require.NoError(t, fourth.Close(staging))
	require.FileExists(t, outputPath)

	fifth, err := build.Prepare(ctx, store, fileRequest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fifth.Close("")) })
	require.NoError(t, fifth.Evaluate(ctx))
	assert.True(t, fifth.Reused())
}

func TestRecipeDigestIncludesPathType(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input.txt")
	outputPath := filepath.Join(workingDir, "output")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))
	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))

	request := prepareRequest(workingDir, inputPath, outputPath)
	first, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, first.Evaluate(ctx))
	fileFingerprint := first.Metadata().Fingerprint
	require.NoError(t, first.Close(""))

	directoryRequest := request
	directoryRequest.Step.Outputs[0].Type = ir.StepPathTypeDirectory
	second, err := build.Prepare(ctx, store, directoryRequest)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close("")) })
	require.NoError(t, second.Evaluate(ctx))
	assert.NotEmpty(t, fileFingerprint)
	assert.NotEqual(t, fileFingerprint, second.Metadata().Fingerprint)
}

func TestImplicitRunWorkingDirectoryReusesMaterialization(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	inputPath := filepath.Join(dataDir, "input.txt")
	outputPath := filepath.Join(dataDir, "output.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))

	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	firstRunWorkDir := t.TempDir()
	request := prepareRequest(firstRunWorkDir, inputPath, outputPath)
	request.RunWorkDir = firstRunWorkDir
	request.Environment["PWD"] = firstRunWorkDir
	first, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, first.Evaluate(ctx))
	_, staging, err := first.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(staging, []byte("result"), 0o600))
	require.NoError(t, first.Commit(ctx, staging))
	require.NoError(t, first.Close(staging))

	secondRunWorkDir := t.TempDir()
	request.DAGRunID = "run-2"
	request.AttemptID = "attempt-2"
	request.WorkingDir = secondRunWorkDir
	request.RunWorkDir = secondRunWorkDir
	request.Environment["PWD"] = secondRunWorkDir
	second, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close("")) })
	require.NoError(t, second.Evaluate(ctx))
	require.True(t, second.Reused())
}

func TestPrepareReevaluatesAfterWaitingForOutputLock(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input.txt")
	outputPath := filepath.Join(workingDir, "output.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))

	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	request := prepareRequest(workingDir, inputPath, outputPath)
	first, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, first.Evaluate(ctx))
	_, staging, err := first.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(staging, []byte("result"), 0o600))
	require.NoError(t, first.Commit(ctx, staging))

	acquireStarted := make(chan struct{})
	secondResult := make(chan prepareResult, 1)
	go func() {
		secondRequest := request
		secondRequest.DAGRunID = "run-2"
		secondRequest.AttemptID = "attempt-2"
		session, prepareErr := build.Prepare(ctx, notifyingStore{
			MaterializationStore: store,
			acquireStarted:       acquireStarted,
		}, secondRequest)
		secondResult <- prepareResult{session: session, err: prepareErr}
	}()

	select {
	case <-acquireStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, first.Close(""))

	var result prepareResult
	select {
	case result = <-secondResult:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, result.err)
	require.NotNil(t, result.session)
	t.Cleanup(func() { require.NoError(t, result.session.Close("")) })
	require.NoError(t, result.session.Evaluate(ctx))
	assert.True(t, result.session.Reused())
	assert.Equal(t, ir.BuildReasonMatched, result.session.Metadata().Reason)
}

func TestPrepareExplainsWhyExecutionIsRequired(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input.txt")
	outputPath := filepath.Join(workingDir, "output.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))
	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	request := prepareRequest(workingDir, inputPath, outputPath)

	first, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, first.Evaluate(ctx))
	_, staging, err := first.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(staging, []byte("result"), 0o600))
	require.NoError(t, first.Commit(ctx, staging))
	require.NoError(t, first.Close(staging))

	t.Run("input changed", func(t *testing.T) {
		require.NoError(t, os.WriteFile(inputPath, []byte("changed"), 0o600))
		session, err := build.Prepare(ctx, store, request)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, session.Close("")) })
		require.NoError(t, session.Evaluate(ctx))
		assert.Equal(t, ir.BuildDecisionExecute, session.Metadata().Decision)
		assert.Equal(t, ir.BuildReasonInputChanged, session.Metadata().Reason)
	})

	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))
	t.Run("recipe changed", func(t *testing.T) {
		changed := request
		changed.Environment = map[string]string{"MODE": "other"}
		session, err := build.Prepare(ctx, store, changed)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, session.Close("")) })
		require.NoError(t, session.Evaluate(ctx))
		assert.Equal(t, ir.BuildReasonRecipeChanged, session.Metadata().Reason)
	})

	t.Run("step environment changed", func(t *testing.T) {
		changed := request
		changed.Step.Env = []string{"TARGET=${outputs.artifact}"}
		session, err := build.Prepare(ctx, store, changed)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, session.Close("")) })
		require.NoError(t, session.Evaluate(ctx))
		assert.Equal(t, ir.BuildReasonRecipeChanged, session.Metadata().Reason)
	})

	t.Run("effective shell changed", func(t *testing.T) {
		changed := request
		changed.Shell = []string{"bash", "-e"}
		session, err := build.Prepare(ctx, store, changed)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, session.Close("")) })
		require.NoError(t, session.Evaluate(ctx))
		assert.Equal(t, ir.BuildReasonRecipeChanged, session.Metadata().Reason)
	})

	t.Run("reuse disabled", func(t *testing.T) {
		disabled := request
		disabled.NoReuse = true
		session, err := build.Prepare(ctx, store, disabled)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, session.Close("")) })
		require.NoError(t, session.Evaluate(ctx))
		assert.Equal(t, ir.BuildReasonReuseDisabled, session.Metadata().Reason)
	})

	t.Run("secret consuming step", func(t *testing.T) {
		secret := request
		secret.HasSecrets = true
		session, err := build.Prepare(ctx, store, secret)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, session.Close("")) })
		require.NoError(t, session.Evaluate(ctx))
		assert.Equal(t, ir.BuildDecisionAlways, session.Metadata().Decision)
		assert.Equal(t, ir.BuildReasonIneligible, session.Metadata().Reason)
		assert.Empty(t, session.Metadata().Fingerprint)
	})
}

func TestIneligibleCommitPreservesReusableManifest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input.txt")
	outputPath := filepath.Join(workingDir, "output.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))
	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	request := prepareRequest(workingDir, inputPath, outputPath)

	first, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	require.NoError(t, first.Evaluate(ctx))
	_, staging, err := first.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(staging, []byte("first"), 0o600))
	require.NoError(t, first.Commit(ctx, staging))
	require.NoError(t, first.Close(staging))

	ineligibleRequest := request
	ineligibleRequest.DAGRunID = "run-2"
	ineligibleRequest.AttemptID = "attempt-2"
	ineligibleRequest.HasSecrets = true
	ineligible, err := build.Prepare(ctx, store, ineligibleRequest)
	require.NoError(t, err)
	require.NoError(t, ineligible.Evaluate(ctx))
	require.Equal(t, ir.BuildDecisionAlways, ineligible.Metadata().Decision)
	_, staging, err = ineligible.NewAttempt(0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(staging, []byte("second"), 0o600))
	require.NoError(t, ineligible.Commit(ctx, staging))
	require.NoError(t, ineligible.Close(staging))

	request.DAGRunID = "run-3"
	request.AttemptID = "attempt-3"
	third, err := build.Prepare(ctx, store, request)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, third.Close("")) })
	require.NoError(t, third.Evaluate(ctx))
	require.Equal(t, ir.BuildDecisionExecute, third.Metadata().Decision)
	require.Equal(t, ir.BuildReasonOutputChanged, third.Metadata().Reason)
}

func TestPrepareDryRunDoesNotAcquirePathLocks(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))
	store := &previewStore{}
	request := prepareRequest(workingDir, inputPath, filepath.Join(workingDir, "output.txt"))
	request.Dry = true

	session, err := build.Prepare(context.Background(), store, request)
	require.NoError(t, err)
	require.NoError(t, session.Evaluate(context.Background()))
	assert.False(t, store.acquireCalled)
	assert.Equal(t, ir.BuildDecisionExecute, session.Metadata().Decision)
	assert.Equal(t, ir.BuildReasonManifestMissing, session.Metadata().Reason)
}

func TestCommitRejectsUnavailableStore(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input.txt")
	outputPath := filepath.Join(workingDir, "output.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))
	session, err := build.Prepare(context.Background(), nil, prepareRequest(workingDir, inputPath, outputPath))
	require.ErrorContains(t, err, "store is unavailable")
	require.NotNil(t, session)

	err = session.Commit(context.Background(), filepath.Join(workingDir, ".output.tmp"))
	require.ErrorContains(t, err, "store is unavailable")
}

func TestComparisonKeyUsesFilesystemCaseSemantics(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	upper := filepath.Join(dir, "Artifact.txt")
	lower := filepath.Join(dir, "artifact.txt")
	require.NoError(t, os.WriteFile(upper, []byte("output"), 0o600))

	upperInfo, err := os.Lstat(upper)
	require.NoError(t, err)
	lowerInfo, lowerErr := os.Lstat(lower)
	if lowerErr == nil && os.SameFile(upperInfo, lowerInfo) {
		assert.Equal(t, build.ComparisonKey(upper), build.ComparisonKey(lower))
		return
	}
	require.ErrorIs(t, lowerErr, os.ErrNotExist)
	assert.NotEqual(t, build.ComparisonKey(upper), build.ComparisonKey(lower))
}

func TestComparisonKeyResolvesExistingAncestorAliases(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	aliasDir := filepath.Join(dir, "alias")
	require.NoError(t, os.Mkdir(realDir, 0o750))
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("filesystem symlinks are unavailable: %v", err)
	}

	assert.Equal(t,
		build.ComparisonKey(filepath.Join(realDir, "artifact.txt")),
		build.ComparisonKey(filepath.Join(aliasDir, "artifact.txt")),
	)
	assert.Equal(t,
		build.IdentityKey(filepath.Join(realDir, "artifact.txt")),
		build.IdentityKey(filepath.Join(aliasDir, "artifact.txt")),
	)
}

func TestIdentityKeyPreservesAuthoredCase(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	assert.NotEqual(t,
		build.IdentityKey(filepath.Join(dir, "Artifact.txt")),
		build.IdentityKey(filepath.Join(dir, "artifact.txt")),
	)
}

func TestNewAttemptBoundsStagingFilename(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input.txt")
	outputPath := filepath.Join(workingDir, strings.Repeat("a", 240)+".txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))
	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	session, err := build.Prepare(context.Background(), store, prepareRequest(workingDir, inputPath, outputPath))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close("")) })

	_, staging, err := session.NewAttempt(0)
	require.NoError(t, err)
	require.LessOrEqual(t, len(filepath.Base(staging)), 255)
	require.NoError(t, os.WriteFile(staging, []byte("output"), 0o600))
}

func TestNewAttemptCreatesDirectoryStaging(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	inputPath := filepath.Join(workingDir, "input.txt")
	outputPath := filepath.Join(workingDir, "site")
	require.NoError(t, os.WriteFile(inputPath, []byte("input"), 0o600))
	store := materialization.New(filepath.Join(t.TempDir(), "materializations"))
	request := prepareRequest(workingDir, inputPath, outputPath)
	request.Step.Outputs[0].Type = ir.StepPathTypeDirectory
	session, err := build.Prepare(context.Background(), store, request)
	require.NoError(t, err)

	first, firstStaging, err := session.NewAttempt(0)
	require.NoError(t, err)
	require.DirExists(t, firstStaging)
	assert.Equal(t, firstStaging, first["artifact"])
	require.NoError(t, os.WriteFile(filepath.Join(firstStaging, "index.html"), []byte("first"), 0o600))

	second, secondStaging, err := session.NewAttempt(1)
	require.NoError(t, err)
	require.DirExists(t, secondStaging)
	assert.NotEqual(t, firstStaging, secondStaging)
	assert.Empty(t, second["artifact"][len(secondStaging):])

	require.NoError(t, os.RemoveAll(firstStaging))
	require.NoError(t, session.Close(secondStaging))
	require.NoDirExists(t, firstStaging)
	require.NoDirExists(t, secondStaging)
}

func TestResolvePathAcceptsExistingOutputDirectory(t *testing.T) {
	t.Parallel()

	outputPath := filepath.Join(t.TempDir(), "output")
	require.NoError(t, os.Mkdir(outputPath, 0o750))

	resolved, err := build.ResolvePath(outputPath, "", true)
	require.NoError(t, err)
	parent, err := filepath.EvalSymlinks(filepath.Dir(outputPath))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, filepath.Base(outputPath)), resolved)
}

type previewStore struct {
	acquireCalled bool
}

type notifyingStore struct {
	build.MaterializationStore
	acquireStarted chan struct{}
}

type prepareResult struct {
	session *build.Session
	err     error
}

func (s notifyingStore) AcquirePaths(
	ctx context.Context,
	requests []build.PathLockRequest,
) (build.MaterializationLock, error) {
	close(s.acquireStarted)
	return s.MaterializationStore.AcquirePaths(ctx, requests)
}

func (s *previewStore) Get(context.Context, string) (*build.Materialization, error) {
	return nil, build.ErrMaterializationNotFound
}

func (s *previewStore) AcquirePaths(context.Context, []build.PathLockRequest) (build.MaterializationLock, error) {
	s.acquireCalled = true
	return nil, nil
}

func (*previewStore) Commit(context.Context, build.MaterializationLock, build.MaterializationCommit) error {
	return nil
}

func prepareRequest(workingDir, inputPath, outputPath string) build.PrepareRequest {
	return build.PrepareRequest{
		DAG: &ir.DAG{
			Name:       "build-test",
			Type:       ir.TypeBuild,
			WorkingDir: workingDir,
		},
		Step: ir.Step{
			ID:       "build",
			Name:     "build",
			Commands: []ir.CommandEntry{{Command: "build"}},
			Inputs:   []ir.StepInputDeclaration{{Name: "source", Path: inputPath}},
			Outputs:  []ir.StepOutputDeclaration{{Name: "artifact", Path: outputPath}},
		},
		DAGRunID:   "run-1",
		AttemptID:  "attempt-1",
		WorkingDir: workingDir,
		Shell:      []string{"sh"},
		Environment: map[string]string{
			"MODE": "default",
		},
	}
}
