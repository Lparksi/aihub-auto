package routing

import (
	"strconv"
	"strings"
)

func candidateStateKey(key CandidateKey) string {
	return strings.Join([]string{key.AccountID, key.Plan, strconv.Itoa(key.GroupID)}, "\x00")
}

func parseCandidateStateKey(serializedKey string) (CandidateKey, bool) {
	parts := strings.Split(serializedKey, "\x00")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return CandidateKey{}, false
	}
	groupID, conversionError := strconv.Atoi(parts[2])
	if conversionError != nil || groupID <= 0 {
		return CandidateKey{}, false
	}
	return CandidateKey{AccountID: parts[0], Plan: parts[1], GroupID: groupID}, true
}
