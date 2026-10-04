// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package build

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacyFileSnapshotDefaultsToFileType(t *testing.T) {
	t.Parallel()

	var snapshot FileSnapshot
	require.NoError(t, json.Unmarshal([]byte(`{"name":"artifact","path":"output.txt","size":3,"digest":"sha256:abc"}`), &snapshot))
	NormalizeSnapshot(&snapshot)
	assert.Equal(t, SnapshotTypeFile, snapshot.Type)
	assert.True(t, snapshotEqual(snapshot, FileSnapshot{
		Name: "artifact", Path: "output.txt", Type: SnapshotTypeFile, Size: 3, Digest: "sha256:abc",
	}))
}
