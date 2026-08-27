package routing

import (
	"testing"
	"time"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/sessions"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSelectCandidateHonorsPriceBandAvailabilityAndModel(t *testing.T) {
	candidates := []Candidate{
		{AuthID: "cheap", Key: CandidateKey{"account-a", "pro", 1}, Rate: 0.10, TTFT: 900, Available: true, Models: []string{"gpt-*"}},
		{AuthID: "fast", Key: CandidateKey{"account-a", "pro", 2}, Rate: 0.30, TTFT: 200, Available: true, Models: []string{"gpt-4"}},
		{AuthID: "offline", Key: CandidateKey{"account-a", "pro", 3}, Rate: 0.05, TTFT: 100, Available: false},
	}
	selection := Select(candidates, SelectionOptions{Mode: ModeBalanced, PriceBand: PriceBand{Min: 0.05, Max: 0.20}, Model: "gpt-4"})
	if selection.Candidate == nil || selection.Candidate.AuthID != "cheap" {
		t.Fatalf("selection = %#v, want cheap eligible candidate", selection)
	}
	if len(selection.Excluded) != 2 {
		t.Fatalf("excluded = %#v, want price/model availability exclusions", selection.Excluded)
	}
}

func TestEconomyUsesSuccessSamplesAndSpeedUsesTTFT(t *testing.T) {
	cheapUnstable := Candidate{AuthID: "cheap", Key: CandidateKey{"account-a", "pro", 1}, Rate: 0.10, TTFT: 100, Available: true}
	fastReliable := Candidate{AuthID: "fast", Key: CandidateKey{"account-a", "pro", 2}, Rate: 0.20, TTFT: 200, Available: true}
	observations := NewObservationStore()
	for sample := 0; sample < 3; sample++ {
		observations.RecordFailure(cheapUnstable.Key, 0)
	}
	for sample := 0; sample < 3; sample++ {
		observations.RecordSuccess(fastReliable.Key, 200, 0)
	}
	economy := Select([]Candidate{cheapUnstable, fastReliable}, SelectionOptions{Mode: ModeEconomy, Observations: observations, EconomyPolicy: DefaultEconomyPolicy})
	if economy.Candidate == nil || economy.Candidate.AuthID != "fast" {
		t.Fatalf("economy = %#v, want reliable fallback", economy)
	}
	speed := Select([]Candidate{cheapUnstable, fastReliable}, SelectionOptions{Mode: ModeSpeed})
	if speed.Candidate == nil || speed.Candidate.AuthID != "cheap" {
		t.Fatalf("speed = %#v, want low TTFT candidate", speed)
	}
}

func TestCacheHitHoldsCurrentRouteUntilIdle(t *testing.T) {
	current := Candidate{AuthID: "current", Key: CandidateKey{"account-a", "pro", 1}, Rate: 0.20, TTFT: 400, Available: true}
	better := Candidate{AuthID: "better", Key: CandidateKey{"account-a", "pro", 2}, Rate: 0.10, TTFT: 200, Available: true}
	decision := Decide([]ScoredCandidate{{Candidate: current, Score: 0.2}, {Candidate: better, Score: 0.35}}, RouteState{Current: current.Key}, DecisionPolicy{Stickiness: 0.05, CachePenalty: 0.2, CacheIdle: time.Minute}, Traffic{Active: true}, time.UnixMilli(0))
	if decision.ShouldSwitch || decision.Reason != DecisionHoldCache {
		t.Fatalf("decision = %#v, want cache hold", decision)
	}
	decision = Decide([]ScoredCandidate{{Candidate: current, Score: 0.2}, {Candidate: better, Score: 0.35}}, RouteState{Current: current.Key}, DecisionPolicy{Stickiness: 0.05, CachePenalty: 0.2, CacheIdle: time.Minute}, Traffic{LastRequestAt: time.UnixMilli(0)}, time.UnixMilli(60_001))
	if !decision.ShouldSwitch || decision.Target.AuthID != "better" {
		t.Fatalf("idle decision = %#v, want switch", decision)
	}
}

func TestManualLockFallbackBreakerAndIsolation(t *testing.T) {
	locked := Candidate{AuthID: "locked", Key: CandidateKey{"account-a", "pro", 1}, Rate: .1, TTFT: 900, Available: true}
	fallback := Candidate{AuthID: "fallback", Key: CandidateKey{"account-a", "pro", 2}, Rate: .2, TTFT: 300, Available: true}
	breaker := NewBreaker(BreakerOptions{ConsecutiveFailures: 2, BaseCooldown: time.Second, MaxCooldown: time.Second})
	selection := Select([]Candidate{locked, fallback}, SelectionOptions{ManualLockAuthID: "locked", Breaker: breaker, Now: time.Unix(0, 0)})
	if selection.Candidate == nil || selection.Candidate.AuthID != "locked" {
		t.Fatalf("lock selection = %#v", selection)
	}
	breaker.RecordFailure(locked.Key, time.Unix(0, 0))
	breaker.RecordFailure(locked.Key, time.Unix(0, 0))
	selection = Select([]Candidate{locked, fallback}, SelectionOptions{ManualLockAuthID: "locked", Breaker: breaker, Now: time.Unix(0, 0)})
	if selection.Candidate == nil || selection.Candidate.AuthID != "fallback" {
		t.Fatalf("fallback selection = %#v", selection)
	}
	if !breaker.Allow(locked.Key, time.Unix(2, 0)) {
		t.Fatal("cooldown recovery should allow one probe")
	}
	breaker.RecordSuccess(locked.Key, time.Unix(2, 0))
	if breaker.State(locked.Key, time.Unix(2, 0)) != BreakerClosed {
		t.Fatal("successful probe should close breaker")
	}

	observations := NewObservationStore()
	observations.RecordFailure(locked.Key, 0)
	otherAccount := CandidateKey{AccountID: "account-b", Plan: "pro", GroupID: 1}
	if observations.Get(otherAccount, 0).Samples != 0 {
		t.Fatal("observations must not cross account boundaries")
	}
}

func TestCloudStatsPrefiltersUnhealthyAndOverridesPrice(t *testing.T) {
	healthy := Candidate{AuthID: "healthy", Key: CandidateKey{"account-a", "pro", 1}, Rate: 0.5, TTFT: 100, Available: true}
	unhealthy := Candidate{AuthID: "unhealthy", Key: CandidateKey{"account-a", "pro", 2}, Rate: 0.1, TTFT: 100, Available: true}
	cloud := &CloudStats{
		Health: map[int]map[string]bool{2: {"gpt-4": false}},
		Prices: map[int]map[string]float64{1: {"gpt-4": 0.05}},
	}
	selection := Select([]Candidate{healthy, unhealthy}, SelectionOptions{Mode: ModeEconomy, Model: "gpt-4", CloudStats: cloud})
	if selection.Candidate == nil || selection.Candidate.AuthID != "healthy" {
		t.Fatalf("selection = %#v, want healthy candidate after cloud prefilter", selection)
	}
	if len(selection.Excluded) != 1 || selection.Excluded[0].Reason != "model_unavailable" {
		t.Fatalf("excluded = %#v, want unhealthy model exclusion", selection.Excluded)
	}
	// The cloud price (0.05) overrides the candidate rate (0.5), so healthy wins
	// on economy price even though its configured rate is higher.
	if selection.Candidate.Score >= 0 {
		t.Fatalf("score = %v, want cloud-price-driven economy score", selection.Candidate.Score)
	}
}

func TestCloudStatsMissingValuesFallBackToCandidateAttributes(t *testing.T) {
	candidate := Candidate{AuthID: "c", Key: CandidateKey{"account-a", "pro", 1}, Rate: 0.2, TTFT: 100, Available: true}
	// No health entry for group 1 and no price for gpt-4: both fall back.
	cloud := &CloudStats{Health: map[int]map[string]bool{2: {"gpt-4": true}}, Prices: map[int]map[string]float64{1: {"claude": 0.01}}}
	selection := Select([]Candidate{candidate}, SelectionOptions{Mode: ModeBalanced, Model: "gpt-4", CloudStats: cloud})
	if selection.Candidate == nil || selection.Candidate.AuthID != "c" {
		t.Fatalf("selection = %#v, want candidate with conservative fallback", selection)
	}
}

func TestSchedulerSelectsImportedAuthWithoutRequestOrOptionalRoutingMetadata(t *testing.T) {
	scheduler := NewScheduler(ModeBalanced, PriceBand{Min: 0, Max: 1}, "")
	request := pluginSchedulerRequest("account-a", "pro", []pluginCandidate{
		{ID: "wrong-account", AccountID: "account-b", Plan: "pro", GroupID: 1, Rate: .1, TTFT: 100},
		{ID: "matching", AccountID: "account-a", Plan: "pro", GroupID: 2, Rate: .2, TTFT: 100},
	})
	response := scheduler.Pick(request, "aihub-auto")
	if !response.Handled || response.AuthID != "matching" {
		t.Fatalf("response = %#v, want matching account-plan candidate", response)
	}
	request.Options.Metadata = nil
	request.Candidates = []pluginapi.SchedulerAuthCandidate{{
		ID:       "imported",
		Provider: "aihub-auto",
		Attributes: map[string]string{
			AttributeAccountID: "account-a",
			AttributePlan:      "unknown",
		},
	}}
	if response = scheduler.Pick(request, "aihub-auto"); !response.Handled || response.AuthID != "imported" {
		t.Fatalf("response = %#v, want imported auth selected with safe defaults", response)
	}
}

func TestSchedulerSnapshotsAndRestoresBreakerObservationsAndAffinity(t *testing.T) {
	candidateKey := CandidateKey{AccountID: "account-a", Plan: "pro", GroupID: 7}
	scheduler := NewSchedulerWithSessionTTL(ModeBalanced, PriceBand{Min: 0, Max: 1}, "", time.Hour)
	scheduler.breaker.RecordFailure(candidateKey, time.Unix(100, 0))
	scheduler.observations.RecordSuccess(candidateKey, 250, 100)
	scheduler.affinity.Bind("session-hash", sessions.Scope{AccountID: "account-a", Plan: "pro", Pool: sessions.DefaultPool}, "gpt-4", 7, time.Unix(100, 0))
	scheduler.affinity.BindAlias("alias-hash", "session-hash", sessions.Scope{AccountID: "account-a", Plan: "pro", Pool: sessions.DefaultPool}, "gpt-4", 7, time.Unix(100, 0))

	routingSnapshot, sessionsSnapshot, aliasesSnapshot := scheduler.Snapshot()
	restoredScheduler := NewSchedulerWithSessionTTL(ModeBalanced, PriceBand{Min: 0, Max: 1}, "", time.Hour)
	restoredScheduler.Restore(routingSnapshot, sessionsSnapshot, aliasesSnapshot)

	if restoredScheduler.breaker.entries[candidateKey].failures != 1 || restoredScheduler.observations.Get(candidateKey, 0).Successes != 1 {
		t.Fatalf("restored routing state = breaker %#v observation %#v, want persisted entries", restoredScheduler.breaker.entries[candidateKey], restoredScheduler.observations.Get(candidateKey, 0))
	}
	if _, resolved := restoredScheduler.affinity.Resolve("session-hash", sessions.Scope{AccountID: "account-a", Plan: "pro", Pool: sessions.DefaultPool}, "gpt-4", time.Unix(101, 0)); !resolved {
		t.Fatal("restored session affinity did not resolve")
	}
	if _, resolved := restoredScheduler.affinity.ResolveAlias("alias-hash", sessions.Scope{AccountID: "account-a", Plan: "pro", Pool: sessions.DefaultPool}, "gpt-4", time.Unix(101, 0)); !resolved {
		t.Fatal("restored alias affinity did not resolve")
	}
}
