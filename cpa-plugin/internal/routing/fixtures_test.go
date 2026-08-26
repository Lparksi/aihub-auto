package routing

import (
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type pluginCandidate struct {
	ID, AccountID, Plan string
	GroupID             int
	Rate, TTFT          float64
}

func pluginSchedulerRequest(accountID, plan string, fixture []pluginCandidate) pluginapi.SchedulerPickRequest {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(fixture))
	for _, candidate := range fixture {
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: candidate.ID, Provider: "aihub-auto", Attributes: map[string]string{
			AttributeAccountID: candidate.AccountID, AttributePlan: candidate.Plan,
			AttributeGroupID: strconv.Itoa(candidate.GroupID), AttributeRate: strconv.FormatFloat(candidate.Rate, 'f', -1, 64), AttributeTTFT: strconv.FormatFloat(candidate.TTFT, 'f', -1, 64),
		}})
	}
	return pluginapi.SchedulerPickRequest{Model: "gpt-4", Options: pluginapi.SchedulerOptions{Metadata: map[string]any{AttributeAccountID: accountID, AttributePlan: plan}}, Candidates: candidates}
}
