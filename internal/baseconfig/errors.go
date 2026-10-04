// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package baseconfig

import (
	"errors"
	"fmt"
	"strings"
)

// WorkflowRef identifies a workflow rebuilt while validating a candidate.
type WorkflowRef struct {
	// Name is the DAG name.
	Name string
	// Location is the authored YAML file path.
	Location string
	// Workspace is the resolved workspace label, empty when unlabelled.
	Workspace string
}

func (r WorkflowRef) displayName() string {
	if r.Name != "" {
		return r.Name
	}
	if r.Location != "" {
		return r.Location
	}
	return "(unknown)"
}

// WorkflowFailure explains why one workflow failed to rebuild.
type WorkflowFailure struct {
	WorkflowRef
	// Reason is the build or validation error message.
	Reason string
}

// RejectionError is returned when a candidate is refused because at least one
// workflow healthy under the current generation fails under the candidate.
// The live configuration is left untouched in that case.
type RejectionError struct {
	// Scope is the rejected candidate's target scope.
	Scope Scope
	// Failures lists every healthy-to-broken workflow regression.
	Failures []WorkflowFailure
}

func (e *RejectionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "base configuration update for %s rejected: %d workflow(s) would fail",
		e.Scope, len(e.Failures))
	for _, f := range e.Failures {
		fmt.Fprintf(&b, "; workflow %q", f.displayName())
		if f.Location != "" && f.Location != f.Name {
			fmt.Fprintf(&b, " (%s)", f.Location)
		}
		if f.Workspace != "" {
			fmt.Fprintf(&b, " [workspace=%s]", f.Workspace)
		}
		fmt.Fprintf(&b, ": %s", f.Reason)
	}
	return b.String()
}

// AsRejection reports whether err is a candidate RejectionError.
func AsRejection(err error) (*RejectionError, bool) {
	var rejected *RejectionError
	if errors.As(err, &rejected) {
		return rejected, true
	}
	return nil, false
}

// ErrPublish is returned when an otherwise validated candidate could not be
// durably written to its live location. It wraps the underlying I/O error.
var ErrPublish = errors.New("failed to publish base configuration")

func publishError(scope Scope, err error) error {
	return fmt.Errorf("%w for %s: %v", ErrPublish, scope, err)
}

// AsPublishError reports whether err is a failed publication.
func AsPublishError(err error) bool {
	return errors.Is(err, ErrPublish)
}

// Outcome is the result of a successful candidate promotion.
type Outcome struct {
	// Generation is the confirmed generation number after promotion.
	Generation int64
	// Warnings lists non-blocking problems, such as workflows already broken
	// under the previous generation. Entries are locatable to a workflow.
	Warnings []string
}
