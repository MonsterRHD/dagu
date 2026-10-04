// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

// Package baseconfig coordinates candidate generations of the global and
// workspace-scoped base configurations. A candidate is rebuilt against every
// affected workflow before it is allowed to replace the confirmed live
// configuration.
package baseconfig

import (
	"github.com/dagucloud/dagu/v2/internal/workspace"
)

// Scope identifies which live base config a candidate targets.
// The zero value targets the global base config.
type Scope struct {
	// Workspace is empty for the global base config; otherwise it is the
	// validated name of a workspace.
	Workspace string
}

// GlobalScope targets the global base config inherited by every workflow.
func GlobalScope() Scope {
	return Scope{}
}

// WorkspaceScope targets the override of a single workspace.
func WorkspaceScope(name string) (Scope, error) {
	if err := workspace.ValidateName(name); err != nil {
		return Scope{}, err
	}
	return Scope{Workspace: name}, nil
}

// IsGlobal reports whether the scope targets the global base config.
func (s Scope) IsGlobal() bool {
	return s.Workspace == ""
}

// Validate checks the scope's workspace name.
func (s Scope) Validate() error {
	if s.IsGlobal() {
		return nil
	}
	return workspace.ValidateName(s.Workspace)
}

func (s Scope) String() string {
	if s.IsGlobal() {
		return "global"
	}
	return "workspace:" + s.Workspace
}

// fileStem is the safe filename component for the scope. Workspace names are
// restricted to [A-Za-z0-9_-], so no escaping is needed.
func (s Scope) fileStem() string {
	if s.IsGlobal() {
		return "global"
	}
	return "workspace-" + s.Workspace
}
