package routing

import (
	"strings"
	"sync"
	"time"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/sessions"
	"github.com/mimmer/aihub-auto/cpa-plugin/internal/state"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Scheduler keeps phase-three route signals in memory. Phase four owns durable state.
type Scheduler struct {
	mutex        sync.Mutex
	mode         Mode
	priceBand    PriceBand
	manualLockID string
	observations *ObservationStore
	breaker      *Breaker
	affinity     *sessions.Affinity
	selected     map[string]Candidate
	cloudStats   *CloudStats
}

func NewScheduler(mode Mode, priceBand PriceBand, manualLockID string) *Scheduler {
	return NewSchedulerWithSessionTTL(mode, priceBand, manualLockID, time.Hour)
}

// NewSchedulerWithSessionTTL enables metadata-provided hashed session affinity.
func NewSchedulerWithSessionTTL(mode Mode, priceBand PriceBand, manualLockID string, sessionTTL time.Duration) *Scheduler {
	return &Scheduler{mode: mode, priceBand: priceBand, manualLockID: manualLockID, observations: NewObservationStore(), breaker: NewBreaker(BreakerOptions{}), affinity: sessions.New(sessionTTL), selected: map[string]Candidate{}}
}

// SetCloudStats installs best-effort AIHub public provider metrics used by
// candidate scoring. A nil value disables cloud-aware routing.
func (scheduler *Scheduler) SetCloudStats(stats *CloudStats) {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	scheduler.cloudStats = stats
}

func (scheduler *Scheduler) Pick(request pluginapi.SchedulerPickRequest, providerID string) pluginapi.SchedulerPickResponse {
	accountID, accountOK := metadataString(request.Options.Metadata, AttributeAccountID)
	plan, planOK := metadataString(request.Options.Metadata, AttributePlan)
	if !accountOK || !planOK {
		accountID, plan, accountOK = SchedulerScopeFromCandidates(request.Candidates, providerID)
		planOK = accountOK
		if !accountOK || !planOK {
			return pluginapi.SchedulerPickResponse{}
		}
	}

	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	candidates := CandidatesFromScheduler(request.Candidates, providerID, accountID, plan, request.Model)
	pool := sessions.DefaultPool
	if request.Options.Metadata["aihub_auto_pool"] == string(sessions.LunaPool) {
		pool = sessions.LunaPool
	}
	scope := sessions.Scope{AccountID: accountID, Plan: plan, Pool: pool}
	sessionKey, sessionOK := metadataString(request.Options.Metadata, "aihub_auto_session_key")
	if sessionOK && scheduler.affinity != nil {
		if groupID, affinityOK := scheduler.affinity.Resolve(sessionKey, scope, request.Model, time.Now()); affinityOK {
			for _, candidate := range candidates {
				if candidate.Key.GroupID == groupID {
					affineSelection := Select([]Candidate{candidate}, SelectionOptions{Mode: scheduler.mode, PriceBand: scheduler.priceBand, Model: request.Model, ManualLockAuthID: scheduler.manualLockID, Observations: scheduler.observations, Breaker: scheduler.breaker, CloudStats: scheduler.cloudStats, Now: time.Now()})
					if affineSelection.Candidate != nil {
						scheduler.selected[affineSelection.Candidate.AuthID] = affineSelection.Candidate.Candidate
						return pluginapi.SchedulerPickResponse{AuthID: affineSelection.Candidate.AuthID, Handled: true}
					}
					break
				}
			}
			scheduler.affinity.Invalidate(sessionKey, scope, groupID)
		}
	}
	selection := Select(candidates, SelectionOptions{Mode: scheduler.mode, PriceBand: scheduler.priceBand, Model: request.Model, ManualLockAuthID: scheduler.manualLockID, Observations: scheduler.observations, Breaker: scheduler.breaker, CloudStats: scheduler.cloudStats, Now: time.Now()})
	if selection.Candidate == nil {
		return pluginapi.SchedulerPickResponse{}
	}
	if sessionOK && scheduler.affinity != nil {
		scheduler.affinity.Bind(sessionKey, scope, request.Model, selection.Candidate.Key.GroupID, time.Now())
	}
	scheduler.selected[selection.Candidate.AuthID] = selection.Candidate.Candidate
	return pluginapi.SchedulerPickResponse{AuthID: selection.Candidate.AuthID, Handled: true}
}

// RecordExecutionOutcome applies a real executor result to the selected candidate.
// CPA's executor request identifies the selected auth ID through its persisted account ID.
func (scheduler *Scheduler) RecordExecutionOutcome(authID string, success bool, ttftMilliseconds float64, now time.Time) {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	candidate, selected := scheduler.selected[authID]
	if !selected {
		return
	}
	if success {
		scheduler.observations.RecordSuccess(candidate.Key, ttftMilliseconds, now.UnixMilli())
		scheduler.breaker.RecordSuccess(candidate.Key, now)
		return
	}
	scheduler.observations.RecordFailure(candidate.Key, now.UnixMilli())
	scheduler.breaker.RecordFailure(candidate.Key, now)
}

// SetManualLock updates the in-memory scheduler preference only.
func (scheduler *Scheduler) SetManualLock(authID string) {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	scheduler.manualLockID = strings.TrimSpace(authID)
}

// ManualLockActive reports whether an in-memory manual preference exists.
func (scheduler *Scheduler) ManualLockActive() bool {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	return scheduler.manualLockID != ""
}

// ClearSessions removes in-memory affinity bindings without touching CPA auths.
func (scheduler *Scheduler) ClearSessions() int {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	if scheduler.affinity == nil {
		return 0
	}
	return scheduler.affinity.ClearBindings()
}

// ClearAliases removes in-memory Responses aliases without touching CPA auths.
func (scheduler *Scheduler) ClearAliases() int {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	if scheduler.affinity == nil {
		return 0
	}
	return scheduler.affinity.ClearAliases()
}

// AffinityCounts reports aggregate values without exposing identifiers.
func (scheduler *Scheduler) AffinityCounts() (bindings, aliases int) {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	if scheduler.affinity == nil {
		return 0, 0
	}
	return scheduler.affinity.Counts()
}

// Snapshot returns affinity records in the state package's non-secret format.
func (scheduler *Scheduler) Snapshot() (state.RoutingSnapshot, map[string]state.SessionRecord, map[string]state.AliasRecord) {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	bindings, aliases := scheduler.affinity.Snapshot()
	sessionsSnapshot := make(map[string]state.SessionRecord, len(bindings))
	aliasesSnapshot := make(map[string]state.AliasRecord, len(aliases))
	for key, binding := range bindings {
		sessionsSnapshot[key] = state.SessionRecord{AccountID: binding.Scope.AccountID, Plan: binding.Scope.Plan, Pool: state.Pool(binding.Scope.Pool), GroupID: binding.GroupID, Model: binding.Model, LastUsedAt: binding.LastUsedAt}
	}
	for key, alias := range aliases {
		aliasesSnapshot[key] = state.AliasRecord{SessionKey: alias.SessionKey, AccountID: alias.Scope.AccountID, Plan: alias.Scope.Plan, Pool: state.Pool(alias.Scope.Pool), GroupID: alias.GroupID, Model: alias.Model, LastUsedAt: alias.LastUsedAt}
	}
	routingSnapshot := state.RoutingSnapshot{Breaker: scheduler.breaker.Snapshot(), Observations: scheduler.observations.Snapshot()}
	return routingSnapshot, sessionsSnapshot, aliasesSnapshot
}

// Restore imports only validated non-secret affinity records.
func (scheduler *Scheduler) Restore(routingSnapshot state.RoutingSnapshot, sessionsSnapshot map[string]state.SessionRecord, aliasesSnapshot map[string]state.AliasRecord) {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	bindings := make(map[string]sessions.Binding, len(sessionsSnapshot))
	aliases := make(map[string]sessions.Alias, len(aliasesSnapshot))
	for key, record := range sessionsSnapshot {
		bindings[key] = sessions.Binding{Scope: sessions.Scope{AccountID: record.AccountID, Plan: record.Plan, Pool: sessions.Pool(record.Pool)}, GroupID: record.GroupID, Model: record.Model, LastUsedAt: record.LastUsedAt}
	}
	for key, record := range aliasesSnapshot {
		aliases[key] = sessions.Alias{SessionKey: record.SessionKey, Scope: sessions.Scope{AccountID: record.AccountID, Plan: record.Plan, Pool: sessions.Pool(record.Pool)}, GroupID: record.GroupID, Model: record.Model, LastUsedAt: record.LastUsedAt}
	}
	scheduler.affinity.Restore(bindings, aliases)
	scheduler.breaker.Restore(routingSnapshot.Breaker)
	scheduler.observations.Restore(routingSnapshot.Observations)
}

func metadataString(metadata map[string]any, name string) (string, bool) {
	value, ok := metadata[name].(string)
	value = strings.TrimSpace(value)
	return value, ok && value != ""
}

// ModelMatches reports whether a configured model pattern owns a model route.
// Empty patterns deliberately handle nothing so the host's built-in routes remain safe.
func ModelMatches(patterns []string, requestedModel string) bool {
	return supportsModel(patterns, requestedModel) && len(patterns) > 0
}
