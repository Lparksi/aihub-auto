package abi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestDispatcherRoutingFlows exercises the public JSON ABI at the boundary CPA
// uses, rather than invoking routing internals directly.
func TestDispatcherRoutingFlows(t *testing.T) {
	dispatcher := configuredDispatcher(t, "https://aihub.example", "price_band_min: 0\nprice_band_max: 0.2\n")
	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "account-a-default", Provider: PluginIdentifier, Attributes: routingAttributes("account-a", "pro", 1, 0.1, 400)},
		{ID: "account-a-luna", Provider: PluginIdentifier, Attributes: routingAttributes("account-a", "pro", 2, 0.1, 500)},
		{ID: "account-b-default", Provider: PluginIdentifier, Attributes: routingAttributes("account-b", "pro", 3, 0.1, 100)},
		{ID: "over-price", Provider: PluginIdentifier, Attributes: routingAttributes("account-a", "pro", 4, 0.3, 50)},
	}

	defaultRequest := schedulerRequest(candidates, map[string]any{
		"aihub_auto_account_id":  "account-a",
		"aihub_auto_plan":        "pro",
		"aihub_auto_session_key": "session-a",
	})
	if selectedAuthID := dispatchSchedulerPick(t, dispatcher, defaultRequest); selectedAuthID != "account-a-default" {
		t.Fatalf("default pool selection = %q, want account-a-default", selectedAuthID)
	}

	// Altering current scores makes the Luna scope choose group 2. The default
	// binding below must remain on group 1, proving scopes do not cross-contaminate.
	candidates[0].Attributes[routing.AttributeTTFT] = "900"
	candidates[1].Attributes[routing.AttributeTTFT] = "10"
	lunaRequest := schedulerRequest(candidates, map[string]any{
		"aihub_auto_account_id":  "account-a",
		"aihub_auto_plan":        "pro",
		"aihub_auto_pool":        "luna",
		"aihub_auto_session_key": "session-a",
	})
	if selectedAuthID := dispatchSchedulerPick(t, dispatcher, lunaRequest); selectedAuthID != "account-a-luna" {
		t.Fatalf("luna pool selection = %q, want account-a-luna", selectedAuthID)
	}

	// A second default request must retain its own affinity and never reuse Luna.
	if selectedAuthID := dispatchSchedulerPick(t, dispatcher, defaultRequest); selectedAuthID != "account-a-default" {
		t.Fatalf("default affinity selection = %q, want account-a-default", selectedAuthID)
	}

	// The inclusive price cap excludes group 1 for a new request and falls back
	// to the in-band account-a candidate rather than another account's auth.
	defaultRequest.Candidates[0].Attributes[routing.AttributeRate] = "0.3"
	defaultRequest.Options.Metadata["aihub_auto_session_key"] = "new-session"
	if selectedAuthID := dispatchSchedulerPick(t, dispatcher, defaultRequest); selectedAuthID != "account-a-luna" {
		t.Fatalf("price fallback = %q, want account-a-luna", selectedAuthID)
	}
}

func TestDispatcherManagementAndSSELimits(t *testing.T) {
	dispatcher := NewDispatcher()
	for _, request := range []pluginapi.ManagementRequest{
		{Method: http.MethodPost, Path: managementBasePath + "/lock", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"auth_id":"preferred"}`)},
		{Method: http.MethodPost, Path: managementBasePath + "/unlock", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{}`)},
	} {
		response := callManagement(t, dispatcher, request)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("management %s %s status = %d, want 200", request.Method, request.Path, response.StatusCode)
		}
	}

	unauthorizedByPlugin := callManagement(t, dispatcher, pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + "/lock", Headers: http.Header{"Content-Type": []string{"text/plain"}}, Body: []byte(`{"auth_id":"preferred"}`)})
	if unauthorizedByPlugin.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON management mutation status = %d, want 415", unauthorizedByPlugin.StatusCode)
	}

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, _ *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		_, _ = responseWriter.Write([]byte("data: oversized\n\n"))
	}))
	defer server.Close()
	limitedDispatcher := configuredDispatcher(t, server.URL, "max_response_bytes: 4\n")
	rawRequest, _ := json.Marshal(pluginapi.ExecutorRequest{Format: "chat-completions", StorageJSON: []byte(`{"access_token":"access-token"}`), Payload: []byte(`{"model":"model-a"}`)})
	var streamResponse struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeEnvelope(t, limitedDispatcher.Handle(pluginabi.MethodExecutorExecuteStream, rawRequest), &streamResponse)
	if streamResponse.OK || streamResponse.Error.Code != "response_too_large" {
		t.Fatalf("limited SSE response = %#v, want response_too_large", streamResponse)
	}
}

func schedulerRequest(candidates []pluginapi.SchedulerAuthCandidate, metadata map[string]any) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Model: "gpt-4", Options: pluginapi.SchedulerOptions{Metadata: metadata}, Candidates: candidates}
}

func dispatchSchedulerPick(testingHandle *testing.T, dispatcher *Dispatcher, request pluginapi.SchedulerPickRequest) string {
	testingHandle.Helper()
	rawRequest, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		testingHandle.Fatalf("marshal scheduler request: %v", errMarshal)
	}
	var response struct {
		OK     bool                            `json:"ok"`
		Result pluginapi.SchedulerPickResponse `json:"result"`
	}
	decodeEnvelope(testingHandle, dispatcher.Handle(pluginabi.MethodSchedulerPick, rawRequest), &response)
	if !response.OK || !response.Result.Handled {
		testingHandle.Fatalf("scheduler response = %#v, want handled response", response)
	}
	return response.Result.AuthID
}
