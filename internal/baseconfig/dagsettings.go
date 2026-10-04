// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package baseconfig

import (
	"context"

	"github.com/dagucloud/dagu/v2/internal/dagsettings"
)

// dagSettingsLiveStore adapts a dagsettings.BaseConfigStore to LiveStore.
type dagSettingsLiveStore struct {
	store dagsettings.BaseConfigStore
}

func (s dagSettingsLiveStore) Read(ctx context.Context) ([]byte, error) {
	yamlSpec, err := s.store.GetSpec(ctx)
	if err != nil {
		return nil, err
	}
	return []byte(yamlSpec), nil
}

func (s dagSettingsLiveStore) Write(ctx context.Context, content []byte) error {
	return s.store.UpdateSpec(ctx, content)
}

type dagSettingsLiveStores struct {
	global   LiveStore
	provider dagsettings.BaseConfigProvider
}

// NewDAGSettingsLiveStores adapts the existing global base config store and
// workspace store provider to the LiveStores interface. Writes keep going
// through the stores' atomic write and cache-invalidation path.
func NewDAGSettingsLiveStores(
	global dagsettings.BaseConfigStore,
	provider dagsettings.BaseConfigProvider,
) LiveStores {
	return &dagSettingsLiveStores{
		global:   dagSettingsLiveStore{store: global},
		provider: provider,
	}
}

func (l *dagSettingsLiveStores) Global() LiveStore {
	return l.global
}

func (l *dagSettingsLiveStores) Workspace(name string) (LiveStore, error) {
	store, err := l.provider(name)
	if err != nil {
		return nil, err
	}
	return dagSettingsLiveStore{store: store}, nil
}
