package routing

import (
	"math"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/state"
)

type Observation struct {
	EWMA               float64
	Samples, Successes int
}
type ObservationStore struct{ entries map[CandidateKey]Observation }

func NewObservationStore() *ObservationStore {
	return &ObservationStore{entries: make(map[CandidateKey]Observation)}
}
func (store *ObservationStore) RecordSuccess(key CandidateKey, ttft float64, _ int64) {
	entry := store.entries[key]
	if ttft > 0 && !math.IsNaN(ttft) {
		if entry.EWMA == 0 {
			entry.EWMA = ttft
		} else {
			entry.EWMA = .3*ttft + .7*entry.EWMA
		}
	}
	entry.Samples++
	entry.Successes++
	store.entries[key] = entry
}
func (store *ObservationStore) RecordFailure(key CandidateKey, _ int64) {
	entry := store.entries[key]
	entry.Samples++
	store.entries[key] = entry
}
func (store *ObservationStore) Get(key CandidateKey, _ int64) Observation {
	if store == nil {
		return Observation{}
	}
	return store.entries[key]
}

// Snapshot returns durable, non-secret observations indexed by candidate.
func (store *ObservationStore) Snapshot() map[string]state.ObservationRecord {
	snapshot := make(map[string]state.ObservationRecord, len(store.entries))
	for key, observation := range store.entries {
		snapshot[candidateStateKey(key)] = state.ObservationRecord{EWMA: observation.EWMA, Samples: observation.Samples, Successes: observation.Successes}
	}
	return snapshot
}

// Restore imports valid observations from durable non-secret state.
func (store *ObservationStore) Restore(records map[string]state.ObservationRecord) {
	store.entries = make(map[CandidateKey]Observation, len(records))
	for serializedKey, record := range records {
		candidateKey, valid := parseCandidateStateKey(serializedKey)
		if !valid || record.Samples < 0 || record.Successes < 0 || record.Successes > record.Samples || math.IsNaN(record.EWMA) || math.IsInf(record.EWMA, 0) {
			continue
		}
		store.entries[candidateKey] = Observation{EWMA: record.EWMA, Samples: record.Samples, Successes: record.Successes}
	}
}
