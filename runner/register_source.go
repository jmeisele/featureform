// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package runner

import (
	"encoding/json"
	"fmt"

	"github.com/featureform/metadata"
	"github.com/featureform/provider"
	pc "github.com/featureform/provider/provider_config"
	pt "github.com/featureform/provider/provider_type"
	"github.com/featureform/types"
)

func (m *RegisterSourceRunner) Run() (types.CompletionWatcher, error) {
	done := make(chan interface{})
	registerFileWatcher := &SyncWatcher{
		ResultSync:  &ResultSync{},
		DoneChannel: done,
	}
	go func() {
		if _, err := m.Offline.RegisterPrimaryFromSourceTable(m.ResourceID, m.SourceTableName); err != nil {
			registerFileWatcher.EndWatch(err)
			return
		}
		registerFileWatcher.EndWatch(nil)
	}()
	return registerFileWatcher, nil
}

type RegisterSourceConfig struct {
	OfflineType     pt.Type
	OfflineConfig   pc.SerializedConfig
	ResourceID      provider.ResourceID
	SourceTableName string
}

type RegisterSourceRunner struct {
	Offline         provider.OfflineStore
	ResourceID      provider.ResourceID
	SourceTableName string
}

func (r RegisterSourceRunner) Resource() metadata.ResourceID {
	return metadata.ResourceID{
		Name:    r.ResourceID.Name,
		Variant: r.ResourceID.Variant,
		Type:    provider.ProviderToMetadataResourceType[r.ResourceID.Type],
	}
}

func (r RegisterSourceRunner) IsUpdateJob() bool {
	return false
}

func (c *RegisterSourceConfig) Serialize() (Config, error) {
	config, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return config, nil
}

func (c *RegisterSourceConfig) Deserialize(config Config) error {
	err := json.Unmarshal(config, c)
	if err != nil {
		return err
	}
	return nil
}

func RegisterSourceRunnerFactory(config Config) (types.Runner, error) {
	registerConfig := &RegisterSourceConfig{}
	if err := registerConfig.Deserialize(config); err != nil {
		return nil, fmt.Errorf("failed to deserialize register file config")
	}
	offlineProvider, err := provider.Get(registerConfig.OfflineType, registerConfig.OfflineConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to configure offline provider: %v", err)
	}
	offlineStore, err := offlineProvider.AsOfflineStore()
	if err != nil {
		return nil, fmt.Errorf("failed to convert provider to offline store: %v", err)
	}
	return &RegisterSourceRunner{
		Offline:         offlineStore,
		ResourceID:      registerConfig.ResourceID,
		SourceTableName: registerConfig.SourceTableName,
	}, nil

}
