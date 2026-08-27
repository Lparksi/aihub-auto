package abi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegistrationDeclaresImplementedPhaseTwoCapabilities(t *testing.T) {
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			SchemaVersion uint32 `json:"schema_version"`
			Capabilities  struct {
				ManagementAPI         bool     `json:"management_api"`
				AuthProvider          bool     `json:"auth_provider"`
				ModelProvider         bool     `json:"model_provider"`
				Executor              bool     `json:"executor"`
				Scheduler             bool     `json:"scheduler"`
				ModelRouter           bool     `json:"model_router"`
				ExecutorModelScope    string   `json:"executor_model_scope"`
				ExecutorInputFormats  []string `json:"executor_input_formats"`
				ExecutorOutputFormats []string `json:"executor_output_formats"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	decodeEnvelope(t, NewDispatcher().Handle(pluginabi.MethodPluginRegister, nil), &response)
	if !response.OK || response.Result.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("registration = %#v, want successful schema v%d registration", response, pluginabi.SchemaVersion)
	}
	capabilities := response.Result.Capabilities
	if !capabilities.ManagementAPI || !capabilities.AuthProvider || !capabilities.ModelProvider || !capabilities.Executor || !capabilities.Scheduler || !capabilities.ModelRouter {
		t.Fatalf("capabilities = %#v, want all implemented phase-two handlers declared", capabilities)
	}
	if capabilities.ExecutorModelScope != string(pluginapi.ExecutorModelScopeOAuth) || len(capabilities.ExecutorInputFormats) != 1 || capabilities.ExecutorInputFormats[0] != "chat-completions" || len(capabilities.ExecutorOutputFormats) != 1 || capabilities.ExecutorOutputFormats[0] != "chat-completions" {
		t.Fatalf("executor capabilities = %#v, want OAuth chat-completions pass-through", capabilities)
	}
}

func TestModelRouteAndSchedulerPickUseOnlyConfiguredAIHubMetadata(t *testing.T) {
	dispatcher := configuredDispatcher(t, "https://aihub.example", "model_patterns: ['gpt-*']\nmode: speed\nprice_band_min: 0\nprice_band_max: 1\n")
	modelRequest, errMarshal := json.Marshal(pluginapi.ModelRouteRequest{RequestedModel: "gpt-4"})
	if errMarshal != nil {
		t.Fatalf("marshal model route request: %v", errMarshal)
	}
	var modelResponse struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.ModelRouteResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodModelRoute, modelRequest), &modelResponse)
	if !modelResponse.OK || !modelResponse.Result.Handled || modelResponse.Result.TargetKind != pluginapi.ModelRouteTargetExecutor || modelResponse.Result.Target != PluginIdentifier {
		t.Fatalf("model route = %#v, want configured plugin executor", modelResponse)
	}

	modelRequest, _ = json.Marshal(pluginapi.ModelRouteRequest{RequestedModel: "claude-sonnet"})
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodModelRoute, modelRequest), &modelResponse)
	if !modelResponse.OK || modelResponse.Result.Handled {
		t.Fatalf("foreign route = %#v, want unhandled", modelResponse)
	}

	pickRequest := pluginapi.SchedulerPickRequest{Model: "gpt-4", Options: pluginapi.SchedulerOptions{Metadata: map[string]any{"aihub_auto_account_id": "account-a", "aihub_auto_plan": "pro"}}, Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "wrong-provider", Provider: "other", Attributes: routingAttributes("account-a", "pro", 1, .1, 100)},
		{ID: "wrong-account", Provider: PluginIdentifier, Attributes: routingAttributes("account-b", "pro", 2, .1, 100)},
		{ID: "selected", Provider: PluginIdentifier, Attributes: routingAttributes("account-a", "pro", 3, .2, 100)},
	}}
	rawPickRequest, _ := json.Marshal(pickRequest)
	var pickResponse struct {
		OK     bool                            `json:"ok"`
		Result pluginapi.SchedulerPickResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodSchedulerPick, rawPickRequest), &pickResponse)
	if !pickResponse.OK || !pickResponse.Result.Handled || pickResponse.Result.AuthID != "selected" {
		t.Fatalf("scheduler pick = %#v, want matching AIHub candidate", pickResponse)
	}

	pickRequest.Options.Metadata = nil
	rawPickRequest, _ = json.Marshal(pickRequest)
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodSchedulerPick, rawPickRequest), &pickResponse)
	if !pickResponse.OK || pickResponse.Result.Handled {
		t.Fatalf("metadata-free scheduler pick = %#v, want unhandled", pickResponse)
	}
}

func routingAttributes(accountID, plan string, groupID int, rate, ttft float64) map[string]string {
	return map[string]string{"aihub_auto_account_id": accountID, "aihub_auto_plan": plan, "aihub_auto_group_id": fmt.Sprintf("%d", groupID), "aihub_auto_rate_multiplier": fmt.Sprintf("%g", rate), "aihub_auto_ttft_ms": fmt.Sprintf("%g", ttft)}
}

func TestMalformedHandlerRequestsReturnTypedInvalidRequest(t *testing.T) {
	for _, method := range []string{
		pluginabi.MethodAuthParse,
		pluginabi.MethodAuthLoginStart,
		pluginabi.MethodAuthLoginPoll,
		pluginabi.MethodAuthRefresh,
		pluginabi.MethodModelForAuth,
		pluginabi.MethodExecutorExecute,
		pluginabi.MethodExecutorExecuteStream,
	} {
		var response struct {
			OK    bool `json:"ok"`
			Error struct {
				Code       string `json:"code"`
				HTTPStatus int    `json:"http_status"`
			} `json:"error"`
		}
		decodeEnvelope(t, NewDispatcher().Handle(method, []byte("{")), &response)
		if response.OK || response.Error.Code != "invalid_request" || response.Error.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("%s response = %#v, want invalid_request 400", method, response)
		}
	}
}

func TestParseObservedCredentialsAndRefreshPersistedStorage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/auth/refresh" {
			t.Fatalf("path = %q, want refresh endpoint", request.URL.Path)
		}
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"code":0,"data":{"access_token":"new-access","expires_in":3600}}`))
	}))
	defer server.Close()
	dispatcher := configuredDispatcher(t, server.URL)

	parseRequest, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: PluginIdentifier, FileName: "observed.json", RawJSON: []byte(`{"accessToken":"old-access","refreshToken":"refresh-secret","email":"user@example.test","expiresAt":1760000000000}`)})
	var parseResponse struct {
		OK     bool                        `json:"ok"`
		Result pluginapi.AuthParseResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodAuthParse, parseRequest), &parseResponse)
	if !parseResponse.OK || !parseResponse.Result.Handled || parseResponse.Result.Auth.Label != "user@example.test" {
		t.Fatalf("parse response = %#v, want observed credential", parseResponse)
	}

	refreshRequest, _ := json.Marshal(pluginapi.AuthRefreshRequest{StorageJSON: parseResponse.Result.Auth.StorageJSON})
	var refreshResponse struct {
		OK     bool                          `json:"ok"`
		Result pluginapi.AuthRefreshResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodAuthRefresh, refreshRequest), &refreshResponse)
	var refreshedStorage authStorage
	if errUnmarshal := json.Unmarshal(refreshResponse.Result.Auth.StorageJSON, &refreshedStorage); errUnmarshal != nil {
		t.Fatalf("unmarshal refreshed storage: %v", errUnmarshal)
	}
	if !refreshResponse.OK || refreshedStorage.AccessToken != "new-access" || refreshedStorage.RefreshToken != "refresh-secret" {
		t.Fatalf("refresh response = %#v, want refreshed access token and preserved refresh token", refreshResponse)
	}
}

func TestParseAuthRejectsUnclaimedAndForeignCredentialJSON(t *testing.T) {
	dispatcher := NewDispatcher()
	for testName, request := range map[string]pluginapi.AuthParseRequest{
		"empty provider with generic token": {
			RawJSON: []byte(`{"access_token":"foreign-token","refresh_token":"foreign-refresh"}`),
		},
		"foreign provider": {
			Provider: "another-provider",
			RawJSON:  []byte(`{"access_token":"foreign-token","refresh_token":"foreign-refresh"}`),
		},
		"foreign persisted record": {
			RawJSON: []byte(`{"type":"another-provider","access_token":"foreign-token"}`),
		},
	} {
		t.Run(testName, func(testingHandle *testing.T) {
			rawRequest, errMarshal := json.Marshal(request)
			if errMarshal != nil {
				testingHandle.Fatalf("marshal request: %v", errMarshal)
			}
			var response struct {
				OK     bool                        `json:"ok"`
				Result pluginapi.AuthParseResponse `json:"result"`
			}
			decodeEnvelope(testingHandle, dispatcher.Handle(pluginabi.MethodAuthParse, rawRequest), &response)
			if !response.OK || response.Result.Handled {
				testingHandle.Fatalf("parse response = %#v, want unhandled foreign material", response)
			}
		})
	}
}

func TestParseAuthRecognizesAIHubPersistedRecordWithoutProvider(t *testing.T) {
	dispatcher := NewDispatcher()
	rawRequest, errMarshal := json.Marshal(pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"aihub-auto","access_token":"access-token","refresh_token":"refresh-token"}`)})
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	var response struct {
		OK     bool                        `json:"ok"`
		Result pluginapi.AuthParseResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodAuthParse, rawRequest), &response)
	if !response.OK || !response.Result.Handled || response.Result.Auth.Provider != PluginIdentifier {
		t.Fatalf("parse response = %#v, want aihub persisted credentials", response)
	}
}

func TestCustomProviderOwnsOnlyItsPersistedCredentials(t *testing.T) {
	dispatcher := configuredDispatcher(t, "https://aihub.example", "provider_id: custom-aihub\n")
	for _, provider := range []string{"custom-aihub", PluginIdentifier} {
		rawRequest, errMarshal := json.Marshal(pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"` + provider + `","access_token":"access-token"}`)})
		if errMarshal != nil {
			t.Fatalf("marshal parse request: %v", errMarshal)
		}
		var response struct {
			OK     bool                        `json:"ok"`
			Result pluginapi.AuthParseResponse `json:"result"`
		}
		decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodAuthParse, rawRequest), &response)
		if response.Result.Handled != (provider == "custom-aihub") {
			t.Fatalf("provider %q handled=%v, want strict custom provider ownership", provider, response.Result.Handled)
		}
	}
}

func TestCredentialImportUsesManagementEndpointAndPersistsOnlyAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/auth/login":
			requestBody, errRead := io.ReadAll(request.Body)
			if errRead != nil || string(requestBody) != `{"email":"user@example.test","password":"login-secret"}` {
				t.Fatalf("login body = %q, want only supplied credential fields", requestBody)
			}
			_, _ = responseWriter.Write([]byte(`{"code":0,"data":{"access_token":"login-access","refresh_token":"login-refresh","expires_in":3600}}`))
		case "/api/v1/auth/me":
			if request.Header.Get("Authorization") != "Bearer login-access" {
				t.Fatalf("Authorization = %q, want login access token", request.Header.Get("Authorization"))
			}
			_, _ = responseWriter.Write([]byte(`{"code":0,"data":{"id":"account-id","email":"user@example.test"}}`))
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
	}))
	defer server.Close()
	dispatcher := configuredDispatcher(t, server.URL)
	var savedName string
	var savedAuth []byte
	dispatcher.SetAuthSaver(func(_ context.Context, name string, authJSON []byte) error {
		savedName, savedAuth = name, authJSON
		return nil
	})
	rawRequest, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/aihub-auto/credentials", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"email":"user@example.test","password":"login-secret"}`)})
	var response struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.ManagementResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodManagementHandle, rawRequest), &response)
	if !response.OK || response.Result.StatusCode != http.StatusCreated || savedName == "" || string(response.Result.Body) != `{"status":"imported"}` {
		t.Fatalf("credential import response = %#v, want persisted import", response)
	}
	if string(savedAuth) == "" || string(savedAuth) == "login-secret" || string(savedAuth) == `{"email":"user@example.test","password":"login-secret"}` {
		t.Fatalf("persisted auth = %s, must exclude password", savedAuth)
	}
}

func TestCredentialImportDoesNotReportSuccessAfterHostSaveEnvelopeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/auth/login":
			_, _ = responseWriter.Write([]byte(`{"code":0,"data":{"access_token":"login-access","refresh_token":"login-refresh","expires_in":3600}}`))
		case "/api/v1/auth/me":
			_, _ = responseWriter.Write([]byte(`{"code":0,"data":{"id":"account-id","email":"user@example.test"}}`))
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
	}))
	defer server.Close()

	dispatcher := configuredDispatcher(t, server.URL)
	dispatcher.SetAuthSaver(func(_ context.Context, _ string, _ []byte) error {
		_, errDecode := DecodeHostEnvelope(ErrorEnvelope("auth_save_denied", "simulated C host response", false, http.StatusForbidden))
		return errDecode
	})
	rawRequest, errMarshal := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + "/credentials", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"email":"user@example.test","password":"login-secret"}`)})
	if errMarshal != nil {
		t.Fatalf("marshal credential import request: %v", errMarshal)
	}
	var response struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.ManagementResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodManagementHandle, rawRequest), &response)
	if !response.OK || response.Result.StatusCode != http.StatusBadGateway || string(response.Result.Body) == `{"status":"imported"}` {
		t.Fatalf("credential import = %#v, want failed host save response", response)
	}
}

func TestAuthDataSuppliesSafeSchedulerAttributesFromImportedAuth(t *testing.T) {
	dispatcher := NewDispatcher()
	auth := dispatcher.authData(authStorage{AccountID: "account-id", Label: "user@example.test"}, "credential.json")
	wantAttributes := map[string]string{
		routing.AttributeAccountID: "account-id",
		routing.AttributePlan:      "unknown",
		routing.AttributeGroupID:   "1",
		routing.AttributeRate:      "1",
		routing.AttributeTTFT:      "1000",
	}
	for attributeName, wantValue := range wantAttributes {
		if gotValue := auth.Attributes[attributeName]; gotValue != wantValue {
			t.Fatalf("attribute %s = %q, want %q", attributeName, gotValue, wantValue)
		}
	}
}

func TestDiscoversModelsAndForwardsNonStreamingAndSSEResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("Authorization = %q, want auth storage token", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/v1/models":
			_, _ = responseWriter.Write([]byte(`{"data":[{"id":"model-a","display_name":"Model A"}]}`))
		case "/v1/chat/completions":
			responseWriter.Header().Set("Content-Type", "text/event-stream")
			_, _ = responseWriter.Write([]byte("data: {\"id\":\"reply\"}\n\ndata: [DONE]\n\n"))
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
	}))
	defer server.Close()
	dispatcher := configuredDispatcher(t, server.URL)
	storageJSON := []byte(`{"access_token":"access-token","refresh_token":"refresh-token"}`)

	modelRequest, _ := json.Marshal(pluginapi.AuthModelRequest{StorageJSON: storageJSON})
	var modelResponse struct {
		OK     bool                    `json:"ok"`
		Result pluginapi.ModelResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodModelForAuth, modelRequest), &modelResponse)
	if !modelResponse.OK || len(modelResponse.Result.Models) != 1 || modelResponse.Result.Models[0].ID != "model-a" {
		t.Fatalf("model response = %#v, want discovered model", modelResponse)
	}

	executorRequest, _ := json.Marshal(pluginapi.ExecutorRequest{Format: "chat-completions", StorageJSON: storageJSON, Payload: []byte(`{"model":"model-a"}`)})
	var executorResponse struct {
		OK     bool                       `json:"ok"`
		Result pluginapi.ExecutorResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodExecutorExecute, executorRequest), &executorResponse)
	if !executorResponse.OK || string(executorResponse.Result.Payload) != "data: {\"id\":\"reply\"}\n\ndata: [DONE]\n\n" {
		t.Fatalf("executor response = %#v, want unchanged upstream body", executorResponse)
	}

	var streamResponseEnvelope struct {
		OK     bool           `json:"ok"`
		Result streamResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodExecutorExecuteStream, executorRequest), &streamResponseEnvelope)
	streamResult := streamResponseEnvelope.Result
	if !streamResponseEnvelope.OK || len(streamResult.Chunks) != 1 || string(streamResult.Chunks[0].Payload) != "data: {\"id\":\"reply\"}\n\ndata: [DONE]\n\n" || streamResult.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream response = %#v, want ABI encoded SSE chunk", streamResponseEnvelope)
	}
}

func TestStreamExecutionRejectsResponseOverConfiguredLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		_, _ = responseWriter.Write([]byte("data: too-large\n\n"))
	}))
	defer server.Close()
	dispatcher := configuredDispatcher(t, server.URL, "max_response_bytes: 4\n")
	rawRequest, _ := json.Marshal(pluginapi.ExecutorRequest{Format: "chat-completions", StorageJSON: []byte(`{"access_token":"access-token"}`), Payload: []byte(`{"model":"model-a"}`)})
	var response struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodExecutorExecuteStream, rawRequest), &response)
	if response.OK || response.Error.Code != "response_too_large" {
		t.Fatalf("stream response = %#v, want bounded response error", response)
	}
}

func TestStreamExecutionPushesChunksToHostBridge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		_, _ = responseWriter.Write([]byte("data: one\n\n"))
		if flusher, ok := responseWriter.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = responseWriter.Write([]byte("data: two\n\n"))
		if flusher, ok := responseWriter.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer server.Close()
	dispatcher := configuredDispatcher(t, server.URL)
	var emitted [][]byte
	var closed bool
	dispatcher.SetStreamEmitter(
		func(_ context.Context, _ string, payload []byte, errorMessage string) error {
			if errorMessage != "" {
				t.Fatalf("unexpected emit error %q", errorMessage)
			}
			emitted = append(emitted, append([]byte(nil), payload...))
			return nil
		},
		func(_ context.Context, _ string, _ string) error { closed = true; return nil },
	)
	rawRequest, _ := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Format: "chat-completions", StorageJSON: []byte(`{"access_token":"access-token"}`), Payload: []byte(`{"model":"model-a"}`)},
		StreamID:        "stream-1",
	})
	var response struct {
		OK     bool           `json:"ok"`
		Result streamResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodExecutorExecuteStream, rawRequest), &response)
	if !response.OK || len(response.Result.Chunks) != 0 {
		t.Fatalf("stream response = %#v, want immediate empty-chunk handoff", response)
	}
	// The pump goroutine forwards chunks asynchronously; wait for the close.
	deadline := time.Now().Add(2 * time.Second)
	for !closed && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !closed {
		t.Fatal("host stream was not closed after upstream finished")
	}
	var joined []byte
	for _, chunk := range emitted {
		joined = append(joined, chunk...)
	}
	if string(joined) != "data: one\n\ndata: two\n\n" {
		t.Fatalf("emitted chunks = %#v, want upstream SSE body", emitted)
	}
}

func TestStreamExecutionEmitsErrorAndClosesOnOversize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "text/event-stream")
		_, _ = responseWriter.Write([]byte("data: too-large\n\n"))
	}))
	defer server.Close()
	dispatcher := configuredDispatcher(t, server.URL, "max_response_bytes: 4\n")
	var emittedError string
	var closed bool
	dispatcher.SetStreamEmitter(
		func(_ context.Context, _ string, _ []byte, errorMessage string) error { emittedError = errorMessage; return nil },
		func(_ context.Context, _ string, _ string) error { closed = true; return nil },
	)
	rawRequest, _ := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Format: "chat-completions", StorageJSON: []byte(`{"access_token":"access-token"}`), Payload: []byte(`{"model":"model-a"}`)},
		StreamID:        "stream-1",
	})
	var response struct {
		OK bool `json:"ok"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodExecutorExecuteStream, rawRequest), &response)
	if !response.OK {
		t.Fatalf("oversize stream handoff = %#v, want immediate ok", response)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !closed && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !closed || emittedError == "" {
		t.Fatalf("oversize stream: closed=%t error=%q, want error emit and close", closed, emittedError)
	}
}

func TestPoolModeExecutorUsesManagedKeyAsUpstreamBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/keys":
			if request.Header.Get("Authorization") != "Bearer account-token" {
				t.Fatalf("key create Authorization = %q, want account token", request.Header.Get("Authorization"))
			}
			_, _ = responseWriter.Write([]byte(`{"code":0,"data":{"id":"key-1","key":"sk-managed","group_id":3}}`))
		case request.Method == http.MethodPost && request.URL.Path == "/v1/chat/completions":
			if request.Header.Get("Authorization") != "Bearer sk-managed" {
				t.Fatalf("executor Authorization = %q, want managed key material", request.Header.Get("Authorization"))
			}
			_, _ = responseWriter.Write([]byte(`{"id":"reply"}`))
		default:
			t.Fatalf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	dispatcher := configuredDispatcher(t, server.URL, "key_mode: pool\n")
	rawRequest, _ := json.Marshal(pluginapi.ExecutorRequest{
		Format:        "chat-completions",
		StorageJSON:   []byte(`{"account_id":"account-a","access_token":"account-token"}`),
		Payload:       []byte(`{"model":"model-a"}`),
		AuthAttributes: map[string]string{
			routing.AttributeAccountID: "account-a",
			routing.AttributePlan:      "pro",
			routing.AttributeGroupID:   "3",
		},
	})
	var response struct {
		OK     bool                       `json:"ok"`
		Result pluginapi.ExecutorResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodExecutorExecute, rawRequest), &response)
	if !response.OK || string(response.Result.Payload) != `{"id":"reply"}` {
		t.Fatalf("pool executor response = %#v, want managed key execution", response)
	}
}

func TestSingleModeExecutorUsesAccountTokenAsUpstreamBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer account-token" {
			t.Fatalf("executor Authorization = %q, want account token in single mode", request.Header.Get("Authorization"))
		}
		_, _ = responseWriter.Write([]byte(`{"id":"reply"}`))
	}))
	defer server.Close()
	dispatcher := configuredDispatcher(t, server.URL)
	rawRequest, _ := json.Marshal(pluginapi.ExecutorRequest{
		Format:      "chat-completions",
		StorageJSON: []byte(`{"account_id":"account-a","access_token":"account-token"}`),
		Payload:     []byte(`{"model":"model-a"}`),
	})
	var response struct {
		OK     bool                       `json:"ok"`
		Result pluginapi.ExecutorResponse `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodExecutorExecute, rawRequest), &response)
	if !response.OK || string(response.Result.Payload) != `{"id":"reply"}` {
		t.Fatalf("single executor response = %#v, want account token execution", response)
	}
}

func TestManagementRegistrationHonorsConfiguration(t *testing.T) {
	dispatcher := configuredDispatcher(t, "https://aihub.example", "management_enabled: false\n")
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			Routes []pluginapi.ManagementRoute `json:"routes"`
		} `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodManagementRegister, nil), &response)
	if !response.OK || len(response.Result.Routes) != 0 {
		t.Fatalf("management registration = %#v, want no routes", response)
	}
}

func configuredDispatcher(testingHandle *testing.T, baseURL string, additionalYAML ...string) *Dispatcher {
	testingHandle.Helper()
	configurationYAML := "base_url: " + baseURL + "\n"
	if len(additionalYAML) > 0 {
		configurationYAML += additionalYAML[0]
	}
	rawRequest, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: []byte(configurationYAML)})
	if errMarshal != nil {
		testingHandle.Fatalf("marshal reconfigure request: %v", errMarshal)
	}
	dispatcher := NewDispatcher()
	if !responseIsOK(testingHandle, dispatcher.Handle(pluginabi.MethodPluginReconfigure, rawRequest)) {
		testingHandle.Fatal("reconfigure did not succeed")
	}
	return dispatcher
}

func decodeEnvelope(testingHandle *testing.T, rawResponse []byte, destination any) {
	testingHandle.Helper()
	if errUnmarshal := json.Unmarshal(rawResponse, destination); errUnmarshal != nil {
		testingHandle.Fatalf("unmarshal response %s: %v", rawResponse, errUnmarshal)
	}
}

func responseIsOK(testingHandle *testing.T, rawResponse []byte) bool {
	testingHandle.Helper()
	var response struct {
		OK bool `json:"ok"`
	}
	decodeEnvelope(testingHandle, rawResponse, &response)
	return response.OK
}
