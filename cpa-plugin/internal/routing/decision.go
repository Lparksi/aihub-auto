package routing

import "time"

func Decide(candidates []ScoredCandidate, state RouteState, policy DecisionPolicy, traffic Traffic, now time.Time) Decision {
	if len(candidates) == 0 {
		return Decision{Reason: DecisionNoCandidate}
	}
	top := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.Score > top.Score {
			top = candidate
		}
	}
	if state.Current == (CandidateKey{}) || state.Current == top.Key {
		return Decision{Target: &top, ShouldSwitch: state.Current != top.Key, Reason: DecisionSwitch}
	}
	var current *ScoredCandidate
	for index := range candidates {
		if candidates[index].Key == state.Current {
			current = &candidates[index]
			break
		}
	}
	if current == nil {
		return Decision{Target: &top, ShouldSwitch: true, Reason: DecisionSwitch}
	}
	advantage := top.Score - current.Score
	if advantage <= policy.Stickiness {
		return Decision{Target: current, Reason: DecisionHoldSticky}
	}
	recent := traffic.Active || (!traffic.LastRequestAt.IsZero() && now.Sub(traffic.LastRequestAt) < policy.CacheIdle)
	if recent && advantage <= policy.Stickiness+policy.CachePenalty {
		return Decision{Target: current, Reason: DecisionHoldCache}
	}
	return Decision{Target: &top, ShouldSwitch: true, Reason: DecisionSwitch}
}
