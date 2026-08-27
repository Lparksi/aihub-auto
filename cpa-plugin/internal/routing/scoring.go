package routing

import (
	"math"
	"sort"
	"strings"
)

func supportsModel(models []string, requested string) bool {
	if requested == "" || len(models) == 0 {
		return true
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == requested || strings.HasSuffix(model, "*") && strings.HasPrefix(requested, strings.TrimSuffix(model, "*")) {
			return true
		}
	}
	return false
}

func scoreCandidate(candidate Candidate, options SelectionOptions) ScoredCandidate {
	observation := options.Observations.Get(candidate.Key, 0)
	ttft := candidate.TTFT
	if observation.EWMA > 0 {
		ttft = observation.EWMA
	}
	if ttft <= 0 {
		ttft = 20_000
	}
	successRate := 1.0
	if observation.Samples > 0 {
		successRate = float64(observation.Successes) / float64(observation.Samples)
	}
	priceWeight, latencyWeight := .5, .5
	switch options.Mode {
	case ModeEconomy:
		priceWeight, latencyWeight = .8, .2
	case ModeSpeed:
		priceWeight, latencyWeight = .2, .8
	}
	rate := candidate.Rate
	if options.CloudStats != nil {
		if modelPrices, ok := options.CloudStats.Prices[candidate.Key.GroupID]; ok {
			if price, ok := modelPrices[strings.ToLower(strings.TrimSpace(options.Model))]; ok && price >= 0 {
				rate = price
			}
		}
	}
	return ScoredCandidate{Candidate: candidate, Score: -(priceWeight*rate + latencyWeight*(ttft/10_000)) - (1 - successRate), SuccessRate: successRate, Samples: observation.Samples}
}

func Select(candidates []Candidate, options SelectionOptions) Selection {
	if options.Mode == "" {
		options.Mode = ModeBalanced
	}
	if options.PriceBand.Max == 0 {
		options.PriceBand.Max = math.MaxFloat64
	}
	if options.EconomyPolicy.MinSamples == 0 {
		options.EconomyPolicy = DefaultEconomyPolicy
	}
	selection := Selection{}
	for _, candidate := range candidates {
		if !candidate.Available {
			selection.Excluded = append(selection.Excluded, ExcludedCandidate{candidate, "unavailable"})
			continue
		}
		if options.CloudStats != nil {
			if modelHealth, ok := options.CloudStats.Health[candidate.Key.GroupID]; ok {
				if healthy, known := modelHealth[strings.ToLower(strings.TrimSpace(options.Model))]; known && !healthy {
					selection.Excluded = append(selection.Excluded, ExcludedCandidate{candidate, "model_unavailable"})
					continue
				}
			}
		}
		if candidate.Rate < options.PriceBand.Min || candidate.Rate > options.PriceBand.Max {
			selection.Excluded = append(selection.Excluded, ExcludedCandidate{candidate, "price_band"})
			continue
		}
		if !supportsModel(candidate.Models, options.Model) {
			selection.Excluded = append(selection.Excluded, ExcludedCandidate{candidate, "model_unavailable"})
			continue
		}
		if options.Breaker != nil && options.Breaker.State(candidate.Key, options.Now) != BreakerClosed {
			selection.Excluded = append(selection.Excluded, ExcludedCandidate{candidate, "circuit_open"})
			continue
		}
		scored := scoreCandidate(candidate, options)
		if options.Mode == ModeEconomy && ((scored.Samples > 0 && scored.SuccessRate == 0) || (scored.Samples >= options.EconomyPolicy.MinSamples && scored.SuccessRate < options.EconomyPolicy.MinSuccessRate) || candidate.TTFT > options.EconomyPolicy.MaxTTFT) {
			selection.Excluded = append(selection.Excluded, ExcludedCandidate{candidate, "economy_unstable"})
			continue
		}
		selection.Eligible = append(selection.Eligible, scored)
	}
	sort.SliceStable(selection.Eligible, func(left, right int) bool { return selection.Eligible[left].Score > selection.Eligible[right].Score })
	if options.ManualLockAuthID != "" {
		for index := range selection.Eligible {
			if selection.Eligible[index].AuthID == options.ManualLockAuthID {
				selection.Candidate = &selection.Eligible[index]
				return selection
			}
		}
	}
	if len(selection.Eligible) > 0 {
		selection.Candidate = &selection.Eligible[0]
	}
	return selection
}
