// Package routing provides the phase-three in-memory candidate routing engine.
package routing

import "time"

type Mode string

const (
	ModeEconomy  Mode = "economy"
	ModeBalanced Mode = "balanced"
	ModeSpeed    Mode = "speed"
)

type CandidateKey struct {
	AccountID string
	Plan      string
	GroupID   int
}

type Candidate struct {
	AuthID    string
	Key       CandidateKey
	Rate      float64
	TTFT      float64
	Available bool
	Models    []string
}

type PriceBand struct{ Min, Max float64 }

type EconomyPolicy struct {
	MinSamples     int
	MinSuccessRate float64
	MaxTTFT        float64
}

var DefaultEconomyPolicy = EconomyPolicy{MinSamples: 3, MinSuccessRate: .8, MaxTTFT: 20_000}

type ScoredCandidate struct {
	Candidate
	Score       float64
	SuccessRate float64
	Samples     int
}

type ExcludedCandidate struct {
	Candidate Candidate
	Reason    string
}
type Selection struct {
	Candidate *ScoredCandidate
	Eligible  []ScoredCandidate
	Excluded  []ExcludedCandidate
}

type SelectionOptions struct {
	Mode             Mode
	PriceBand        PriceBand
	Model            string
	ManualLockAuthID string
	Observations     *ObservationStore
	Breaker          *Breaker
	EconomyPolicy    EconomyPolicy
	CloudStats       *CloudStats
	Now              time.Time
}

// CloudStats carries best-effort AIHub public provider metrics keyed by group.
// Semantics are not yet confirmed, so every field is optional and missing
// values fall back to the candidate's own attributes.
type CloudStats struct {
	// Health reports whether a group is healthy for a model. A group absent
	// from the map is treated as healthy (conservative fallback).
	Health map[int]map[string]bool
	// Prices reports a group's per-model price. A group/model absent from the
	// map falls back to the candidate's configured rate multiplier.
	Prices map[int]map[string]float64
}

type RouteState struct {
	Current      CandidateKey
	LastSwitchAt time.Time
}
type Traffic struct {
	Active        bool
	LastRequestAt time.Time
}
type DecisionPolicy struct {
	Stickiness, CachePenalty float64
	CacheIdle                time.Duration
}
type DecisionReason string

const (
	DecisionSwitch      DecisionReason = "switch"
	DecisionHoldCache   DecisionReason = "hold_cache"
	DecisionHoldSticky  DecisionReason = "hold_sticky"
	DecisionNoCandidate DecisionReason = "no_candidate"
)

type Decision struct {
	Target       *ScoredCandidate
	ShouldSwitch bool
	Reason       DecisionReason
}
