package routing

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	AttributeAccountID = "aihub_auto_account_id"
	AttributePlan      = "aihub_auto_plan"
	AttributeGroupID   = "aihub_auto_group_id"
	AttributeRate      = "aihub_auto_rate_multiplier"
	AttributeTTFT      = "aihub_auto_ttft_ms"
	AttributeAvailable = "aihub_auto_provider_available"
	AttributeModels    = "aihub_auto_models"
)

func CandidatesFromScheduler(source []pluginapi.SchedulerAuthCandidate, providerID, accountID, plan, model string) []Candidate {
	candidates := make([]Candidate, 0, len(source))
	for _, auth := range source {
		if auth.Provider != providerID || (accountID != "" && auth.Attributes[AttributeAccountID] != accountID) || (plan != "" && auth.Attributes[AttributePlan] != plan) {
			continue
		}
		candidate, err := candidateFromAttributes(auth, model)
		if err == nil {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

// SchedulerScopeFromCandidates derives the only safe scope when CPA did not
// provide request metadata. Multiple account/plan pairs remain unhandled to
// prevent a scheduler request from crossing account boundaries.
func SchedulerScopeFromCandidates(source []pluginapi.SchedulerAuthCandidate, providerID string) (accountID, plan string, ok bool) {
	for _, auth := range source {
		if auth.Provider != providerID {
			continue
		}
		candidateAccountID := strings.TrimSpace(auth.Attributes[AttributeAccountID])
		candidatePlan := strings.TrimSpace(auth.Attributes[AttributePlan])
		if candidateAccountID == "" || candidatePlan == "" {
			continue
		}
		if accountID == "" {
			accountID, plan = candidateAccountID, candidatePlan
			continue
		}
		if accountID != candidateAccountID || plan != candidatePlan {
			return "", "", false
		}
	}
	return accountID, plan, accountID != ""
}

func candidateFromAttributes(auth pluginapi.SchedulerAuthCandidate, _ string) (Candidate, error) {
	attributes := auth.Attributes
	accountID, plan := strings.TrimSpace(attributes[AttributeAccountID]), strings.TrimSpace(attributes[AttributePlan])
	if accountID == "" || plan == "" {
		return Candidate{}, fmt.Errorf("account and plan attributes are required")
	}
	groupID := 1
	if groupValue := strings.TrimSpace(attributes[AttributeGroupID]); groupValue != "" {
		parsedGroupID, errParse := strconv.Atoi(groupValue)
		if errParse != nil || parsedGroupID <= 0 {
			return Candidate{}, fmt.Errorf("valid group attribute is required")
		}
		groupID = parsedGroupID
	}
	rate := 1.0
	if rateValue := strings.TrimSpace(attributes[AttributeRate]); rateValue != "" {
		parsedRate, errParse := strconv.ParseFloat(rateValue, 64)
		if errParse != nil || parsedRate < 0 {
			return Candidate{}, fmt.Errorf("valid rate attribute is required")
		}
		rate = parsedRate
	}
	ttft := 1000.0
	if ttftValue := strings.TrimSpace(attributes[AttributeTTFT]); ttftValue != "" {
		parsedTTFT, errParse := strconv.ParseFloat(ttftValue, 64)
		if errParse != nil || parsedTTFT <= 0 {
			return Candidate{}, fmt.Errorf("valid ttft attribute is required")
		}
		ttft = parsedTTFT
	}
	available := attributes[AttributeAvailable] != "false"
	models := strings.FieldsFunc(attributes[AttributeModels], func(value rune) bool { return value == ',' })
	return Candidate{AuthID: auth.ID, Key: CandidateKey{AccountID: accountID, Plan: plan, GroupID: groupID}, Rate: rate, TTFT: ttft, Available: available, Models: models}, nil
}
