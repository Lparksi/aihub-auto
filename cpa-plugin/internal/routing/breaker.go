package routing

import (
	"time"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/state"
)

type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half-open"
)

type BreakerOptions struct {
	ConsecutiveFailures       int
	BaseCooldown, MaxCooldown time.Duration
}
type breakerEntry struct {
	state           BreakerState
	failures, opens int
	openedAt        time.Time
	probe           bool
}
type Breaker struct {
	options BreakerOptions
	entries map[CandidateKey]*breakerEntry
}

func NewBreaker(options BreakerOptions) *Breaker {
	if options.ConsecutiveFailures <= 0 {
		options.ConsecutiveFailures = 3
	}
	if options.BaseCooldown <= 0 {
		options.BaseCooldown = 30 * time.Second
	}
	if options.MaxCooldown <= 0 {
		options.MaxCooldown = 10 * time.Minute
	}
	return &Breaker{options: options, entries: make(map[CandidateKey]*breakerEntry)}
}
func (breaker *Breaker) entry(key CandidateKey) *breakerEntry {
	entry := breaker.entries[key]
	if entry == nil {
		entry = &breakerEntry{state: BreakerClosed}
		breaker.entries[key] = entry
	}
	return entry
}
func (breaker *Breaker) refresh(entry *breakerEntry, now time.Time) {
	if entry.state != BreakerOpen {
		return
	}
	cooldown := breaker.options.BaseCooldown
	for count := 1; count < entry.opens && cooldown < breaker.options.MaxCooldown; count++ {
		cooldown *= 2
	}
	if cooldown > breaker.options.MaxCooldown {
		cooldown = breaker.options.MaxCooldown
	}
	if now.Sub(entry.openedAt) >= cooldown {
		entry.state = BreakerHalfOpen
		entry.probe = false
	}
}
func (breaker *Breaker) State(key CandidateKey, now time.Time) BreakerState {
	entry := breaker.entry(key)
	breaker.refresh(entry, now)
	return entry.state
}
func (breaker *Breaker) Allow(key CandidateKey, now time.Time) bool {
	entry := breaker.entry(key)
	breaker.refresh(entry, now)
	if entry.state == BreakerClosed {
		return true
	}
	if entry.state == BreakerOpen || entry.probe {
		return false
	}
	entry.probe = true
	return true
}
func (breaker *Breaker) RecordSuccess(key CandidateKey, now time.Time) {
	entry := breaker.entry(key)
	breaker.refresh(entry, now)
	entry.failures = 0
	if entry.state == BreakerHalfOpen {
		entry.state = BreakerClosed
		entry.opens = 0
		entry.probe = false
		entry.openedAt = time.Time{}
	}
}
func (breaker *Breaker) RecordFailure(key CandidateKey, now time.Time) {
	entry := breaker.entry(key)
	breaker.refresh(entry, now)
	entry.failures++
	if entry.state == BreakerHalfOpen || entry.failures >= breaker.options.ConsecutiveFailures {
		entry.state = BreakerOpen
		entry.opens++
		entry.openedAt = now
		entry.probe = false
	}
}

// Snapshot returns durable, non-secret breaker entries indexed by candidate.
func (breaker *Breaker) Snapshot() map[string]state.BreakerRecord {
	snapshot := make(map[string]state.BreakerRecord, len(breaker.entries))
	for key, entry := range breaker.entries {
		snapshot[candidateStateKey(key)] = state.BreakerRecord{State: string(entry.state), Failures: entry.failures, Opens: entry.opens, OpenedAt: entry.openedAt}
	}
	return snapshot
}

// Restore imports valid breaker entries from durable non-secret state.
func (breaker *Breaker) Restore(records map[string]state.BreakerRecord) {
	breaker.entries = make(map[CandidateKey]*breakerEntry, len(records))
	for serializedKey, record := range records {
		candidateKey, valid := parseCandidateStateKey(serializedKey)
		if !valid || record.Failures < 0 || record.Opens < 0 {
			continue
		}
		breakerState := BreakerState(record.State)
		if breakerState == "" {
			breakerState = BreakerClosed
		}
		if breakerState != BreakerClosed && breakerState != BreakerOpen && breakerState != BreakerHalfOpen {
			continue
		}
		breaker.entries[candidateKey] = &breakerEntry{state: breakerState, failures: record.Failures, opens: record.Opens, openedAt: record.OpenedAt}
	}
}
