// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dagucloud/dagu/v2/internal/cmn/config"
	"github.com/dagucloud/dagu/v2/internal/workspace"
)

func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func TestBaseConfigStateDirIsDisjointFromWatchedLocations(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := &config.Config{
		Paths: config.PathsConfig{
			DataDir:    filepath.Join(root, "data"),
			DAGsDir:    filepath.Join(root, "dags"),
			BaseConfig: filepath.Join(root, "config", "base.yaml"),
		},
	}

	stateDir := baseConfigStateDir(cfg)
	assert.True(t, withinDir(cfg.Paths.DataDir, stateDir))
	assert.False(t, withinDir(cfg.Paths.DAGsDir, stateDir),
		"generation state must not live in the watched DAGs directory")
	assert.False(t, withinDir(workspace.BaseConfigDir(cfg.Paths.DAGsDir), stateDir),
		"generation state must not live under the workspace base config directory")
	assert.False(t, withinDir(filepath.Dir(cfg.Paths.BaseConfig), stateDir),
		"generation state must not share the live base config directory")
}
