// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package runner

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/featureform/metadata"
	"github.com/featureform/provider"
	pc "github.com/featureform/provider/provider_config"
	pt "github.com/featureform/provider/provider_type"
	"github.com/featureform/types"
	"go.uber.org/zap"
)

type IndexRunner interface {
	types.Runner
	SetIndex(index int) error
}

type MaterializedChunkRunner struct {
	Materialized provider.Materialization
	Table        provider.OnlineStoreTable
	Store        provider.OnlineStore
	ChunkSize    int64
	ChunkIdx     int64
}

type ResultSync struct {
	err  error
	done bool
	mu   sync.RWMutex
}

func (m *MaterializedChunkRunner) Resource() metadata.ResourceID {
	return metadata.ResourceID{}
}

func (m *MaterializedChunkRunner) IsUpdateJob() bool {
	return false
}

func (m *MaterializedChunkRunner) Run() (types.CompletionWatcher, error) {
	done := make(chan interface{})
	jobWatcher := &SyncWatcher{
		ResultSync:  &ResultSync{},
		DoneChannel: done,
	}
	go func() {
		if m.ChunkSize == 0 {
			jobWatcher.EndWatch(nil)
			return
		}
		numRows, err := m.Materialized.NumRows()
		if err != nil {
			jobWatcher.EndWatch(fmt.Errorf("failed to get number of rows: %w", err))
			return
		}
		if numRows == 0 {
			jobWatcher.EndWatch(nil)
			return
		}

		rowStart := m.ChunkIdx * m.ChunkSize
		rowEnd := rowStart + m.ChunkSize
		if rowEnd > numRows {
			rowEnd = numRows
		}
		it, err := m.Materialized.IterateSegment(rowStart, rowEnd)
		if err != nil {
			jobWatcher.EndWatch(fmt.Errorf("failed to create iterator: %w", err))
			return
		}
		i := 0
		for it.Next() {
			i += 1
			value := it.Value().Value
			entity := it.Value().Entity
			err := m.Table.Set(entity, value)
			if err != nil {
				jobWatcher.EndWatch(fmt.Errorf("could not set table: %w", err))
				return
			}
		}
		if err = it.Err(); err != nil {
			jobWatcher.EndWatch(fmt.Errorf("iteration failed with error: %w", err))
			return
		}
		err = it.Close()
		if err != nil {
			jobWatcher.EndWatch(fmt.Errorf("failed to close iterator: %w", err))
		}
		err = m.Store.Close()
		if err != nil {
			jobWatcher.EndWatch(fmt.Errorf("failed to close Online Store: %w", err))
		}
		jobWatcher.EndWatch(nil)
	}()
	return jobWatcher, nil
}

func (m *MaterializedChunkRunner) SetIndex(index int) error {
	m.ChunkIdx = int64(index)
	return nil
}

func (c *SyncWatcher) EndWatch(err error) {
	c.ResultSync.DoneWithError(err)
	close(c.DoneChannel)
}

func (r *ResultSync) Done() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.done
}

func (r *ResultSync) Err() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.err
}

func (r *ResultSync) DoneWithError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
	r.done = true
}

type SyncWatcher struct {
	ResultSync  *ResultSync
	DoneChannel chan interface{}
}

func (m *SyncWatcher) Err() error {
	return m.ResultSync.Err()
}

func (m *SyncWatcher) Wait() error {
	<-m.DoneChannel
	return m.ResultSync.Err()
}

func (m *SyncWatcher) Complete() bool {
	return m.ResultSync.Done()
}

func (m *SyncWatcher) String() string {
	done := m.ResultSync.Done()
	err := m.ResultSync.Err()
	if err != nil {
		return fmt.Sprintf("Job failed with error: %v", err)
	}
	if !done {
		return "Job still running."
	}
	return "Job completed succesfully."
}

type MaterializedChunkRunnerConfig struct {
	OnlineType     pt.Type
	OfflineType    pt.Type
	OnlineConfig   pc.SerializedConfig
	OfflineConfig  pc.SerializedConfig
	MaterializedID provider.MaterializationID
	ResourceID     provider.ResourceID
	ChunkSize      int64
	ChunkIdx       int64
	IsUpdate       bool
	Logger         *zap.SugaredLogger
}

func (m *MaterializedChunkRunnerConfig) Serialize() (Config, error) {
	config, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return config, nil
}

func (m *MaterializedChunkRunnerConfig) Deserialize(config Config) error {
	err := json.Unmarshal(config, m)
	if err != nil {
		return err
	}
	return nil
}

func MaterializedChunkRunnerFactory(config Config) (types.Runner, error) {
	runnerConfig := &MaterializedChunkRunnerConfig{}
	if err := runnerConfig.Deserialize(config); err != nil {
		return nil, fmt.Errorf("failed to deserialize materialize chunk runner config: %v", err)
	}

	onlineProvider, err := provider.Get(runnerConfig.OnlineType, runnerConfig.OnlineConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to configure online provider: %v", err)
	}
	offlineProvider, err := provider.Get(runnerConfig.OfflineType, runnerConfig.OfflineConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to configure offline provider: %v", err)
	}
	onlineStore, err := onlineProvider.AsOnlineStore()
	if err != nil {
		return nil, fmt.Errorf("failed to convert provider to online store: %v", err)
	}
	offlineStore, err := offlineProvider.AsOfflineStore()
	if err != nil {
		return nil, fmt.Errorf("failed to convert provider to offline store: %v", err)
	}
	materialization, err := offlineStore.GetMaterialization(runnerConfig.MaterializedID)
	if err != nil {
		return nil, fmt.Errorf("cannot get materialization: %v", err)
	}
	numRows, err := materialization.NumRows()
	if err != nil {
		return nil, fmt.Errorf("cannot get materialization num rows: %v", err)
	}
	if runnerConfig.ChunkSize*runnerConfig.ChunkIdx > numRows {
		return nil, fmt.Errorf("chunk runner starts after end of materialization rows")
	}
	table, err := onlineStore.GetTable(runnerConfig.ResourceID.Name, runnerConfig.ResourceID.Variant)
	if err != nil {
		return nil, fmt.Errorf("error getting online table: %v", err)
	}
	return &MaterializedChunkRunner{
		Materialized: materialization,
		Table:        table,
		Store:        onlineStore,
		ChunkSize:    runnerConfig.ChunkSize,
		ChunkIdx:     runnerConfig.ChunkIdx,
	}, nil
}
