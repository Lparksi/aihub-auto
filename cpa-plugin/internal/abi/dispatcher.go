// Package abi implements CPA's JSON RPC contract without cgo.
package abi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/aihub"
	"github.com/mimmer/aihub-auto/cpa-plugin/internal/configuration"
	"github.com/mimmer/aihub-auto/cpa-plugin/internal/keypool"
	"github.com/mimmer/aihub-auto/cpa-plugin/internal/routing"
	"github.com/mimmer/aihub-auto/cpa-plugin/internal/state"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const PluginIdentifier = "aihub-auto"

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	ManagementAPI         bool                         `json:"management_api"`
	AuthProvider          bool                         `json:"auth_provider"`
	ModelProvider         bool                         `json:"model_provider"`
	Executor              bool                         `json:"executor"`
	Scheduler             bool                         `json:"scheduler"`
	ModelRouter           bool                         `json:"model_router"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

// These wrappers mirror CPA's internal/pluginhost/rpc_schema.go. The public
// pluginapi requests intentionally omit callback transport details.
type rpcAuthRefreshRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcAuthModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type hostCallbackContextKey struct{}

// WithHostCallbackID retains CPA's callback context through plugin code.
func WithHostCallbackID(contextValue context.Context, callbackID string) context.Context {
	if contextValue == nil {
		contextValue = context.Background()
	}
	return context.WithValue(contextValue, hostCallbackContextKey{}, strings.TrimSpace(callbackID))
}

// HostCallbackID returns the CPA callback context selected for a request.
func HostCallbackID(contextValue context.Context) string {
	if contextValue == nil {
		return ""
	}
	callbackID, _ := contextValue.Value(hostCallbackContextKey{}).(string)
	return callbackID
}

func boundedContext(callbackID string) (context.Context, context.CancelFunc) {
	return context.WithTimeout(WithHostCallbackID(context.Background(), callbackID), 30*time.Second)
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

// Dispatcher is the in-memory RPC method dispatcher used by the c-shared shim.
type Dispatcher struct {
	configuration   configuration.Config
	scheduler       *routing.Scheduler
	poolManager     *keypool.PoolManager
	stateStore      *state.Store
	stateDiagnostic string
	mutex           sync.RWMutex
	clientFactory   func(string) *aihub.Client
	authSaver       func(context.Context, string, []byte) error
}

type authStorage struct {
	AccountID    string    `json:"account_id"`
	Label        string    `json:"label"`
	Plan         string    `json:"plan,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}
type streamResponse struct {
	Headers http.Header                     `json:"headers"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks"`
}

// NewDispatcher returns a dispatcher with safe defaults and no credentials.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		configuration:   configuration.Defaults(),
		scheduler:       routing.NewScheduler(routing.ModeBalanced, routing.PriceBand{Min: 0, Max: 1}, ""),
		poolManager:     keypool.New(keypool.Options{DefaultCapacity: configuration.DefaultPoolSize, LunaCapacity: configuration.DefaultPoolSize, EntryTTL: configuration.DefaultSessionTTL}),
		stateDiagnostic: "in-memory runtime state only",
		clientFactory: func(baseURL string) *aihub.Client {
			return aihub.NewClient(baseURL, nil)
		},
	}
}

// SetHostHTTPClient configures CPA's transport bridge for every AIHub call.
func (dispatcher *Dispatcher) SetHostHTTPClient(httpClient pluginapi.HostHTTPClient) {
	dispatcher.mutex.Lock()
	defer dispatcher.mutex.Unlock()
	dispatcher.clientFactory = func(baseURL string) *aihub.Client { return aihub.NewClientWithHostHTTPClient(baseURL, httpClient) }
}

// SetAuthSaver configures the CPA host callback used by credential import.
func (dispatcher *Dispatcher) SetAuthSaver(authSaver func(context.Context, string, []byte) error) {
	dispatcher.mutex.Lock()
	defer dispatcher.mutex.Unlock()
	dispatcher.authSaver = authSaver
}

// Handle always returns a CPA schema envelope, including malformed calls.
func (dispatcher *Dispatcher) Handle(method string, request []byte) []byte {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		dispatcher.mutex.Lock()
		defer dispatcher.mutex.Unlock()
		if errConfigure := dispatcher.configure(request); errConfigure != nil {
			return ErrorEnvelope("invalid_configuration", errConfigure.Error(), false, http.StatusBadRequest)
		}
		return SuccessEnvelope(dispatcher.registration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		dispatcher.saveState()
		return SuccessEnvelope(struct{}{})
	case pluginabi.MethodAuthIdentifier:
		dispatcher.mutex.RLock()
		defer dispatcher.mutex.RUnlock()
		return SuccessEnvelope(identifierResponse{Identifier: dispatcher.configuration.ProviderID})
	case pluginabi.MethodAuthParse:
		return dispatcher.parseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return dispatcher.startLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return dispatcher.pollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return dispatcher.refreshAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		dispatcher.mutex.RLock()
		defer dispatcher.mutex.RUnlock()
		return SuccessEnvelope(identifierResponse{Identifier: dispatcher.configuration.ProviderID})
	case pluginabi.MethodModelRegister:
		dispatcher.mutex.RLock()
		defer dispatcher.mutex.RUnlock()
		return SuccessEnvelope(pluginapi.ModelRegistrationResponse{Provider: dispatcher.configuration.ProviderID, Models: staticModels(dispatcher.configuration.ProviderID)})
	case pluginabi.MethodModelStatic:
		dispatcher.mutex.RLock()
		defer dispatcher.mutex.RUnlock()
		return SuccessEnvelope(pluginapi.ModelResponse{Provider: dispatcher.configuration.ProviderID, Models: staticModels(dispatcher.configuration.ProviderID)})
	case pluginabi.MethodModelForAuth:
		return dispatcher.modelsForAuth(request)
	case pluginabi.MethodModelRoute:
		return dispatcher.routeModel(request)
	case pluginabi.MethodSchedulerPick:
		return dispatcher.pickSchedulerCandidate(request)
	case pluginabi.MethodExecutorExecute:
		return dispatcher.execute(request, false)
	case pluginabi.MethodExecutorExecuteStream:
		return dispatcher.execute(request, true)
	case pluginabi.MethodExecutorCountTokens, pluginabi.MethodExecutorHTTPRequest:
		return ErrorEnvelope("executor_unavailable", "AIHub exposes no observed compatible endpoint for this executor method", false, http.StatusNotImplemented)
	case pluginabi.MethodManagementRegister:
		dispatcher.mutex.RLock()
		defer dispatcher.mutex.RUnlock()
		return SuccessEnvelope(dispatcher.managementRegistration())
	case pluginabi.MethodManagementHandle:
		return dispatcher.handleManagement(request)
	default:
		return ErrorEnvelope("unknown_method", "unknown method: "+method, false, http.StatusNotFound)
	}
}

func (dispatcher *Dispatcher) configure(rawRequest []byte) error {
	request := lifecycleRequest{}
	if len(rawRequest) > 0 {
		if errUnmarshal := json.Unmarshal(rawRequest, &request); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	parsedConfiguration, errParse := configuration.Parse(request.ConfigYAML)
	if errParse != nil {
		return errParse
	}
	scheduler, poolManager, stateStore, snapshot := buildRuntime(parsedConfiguration, state.EmptySnapshot())
	dispatcher.configuration = parsedConfiguration
	dispatcher.scheduler = scheduler
	dispatcher.poolManager = poolManager
	dispatcher.stateStore = stateStore
	if stateStore != nil {
		if errSave := stateStore.Save(snapshot); errSave != nil {
			return fmt.Errorf("initialize persistent state: %w", errSave)
		}
		dispatcher.stateDiagnostic = "persistent state configured and healthy"
	} else {
		dispatcher.stateDiagnostic = "in-memory runtime state only"
	}
	return nil
}

// saveState atomically writes only non-secret scheduler metadata when enabled.
func (dispatcher *Dispatcher) saveState() {
	dispatcher.mutex.RLock()
	store, scheduler, poolManager := dispatcher.stateStore, dispatcher.scheduler, dispatcher.poolManager
	dispatcher.mutex.RUnlock()
	if store == nil || scheduler == nil {
		return
	}
	routingSnapshot, sessionsSnapshot, aliasesSnapshot := scheduler.Snapshot()
	keysSnapshot := poolManager.Snapshot()
	if errSave := store.Save(state.Snapshot{Version: state.CurrentVersion, Routing: routingSnapshot, Sessions: sessionsSnapshot, Aliases: aliasesSnapshot, Keys: keysSnapshot}); errSave != nil {
		dispatcher.setStateDiagnostic("persistent state save failed")
	}
}

func (dispatcher *Dispatcher) setStateDiagnostic(diagnostic string) {
	dispatcher.mutex.Lock()
	defer dispatcher.mutex.Unlock()
	dispatcher.stateDiagnostic = diagnostic
}

func buildRuntime(parsedConfiguration configuration.Config, fallbackSnapshot state.Snapshot) (*routing.Scheduler, *keypool.PoolManager, *state.Store, state.Snapshot) {
	scheduler := routing.NewSchedulerWithSessionTTL(routing.Mode(parsedConfiguration.Mode), routing.PriceBand{Min: parsedConfiguration.PriceBandMin, Max: parsedConfiguration.PriceBandMax}, parsedConfiguration.ManualLockAuthID, parsedConfiguration.SessionTTL)
	poolManager := keypool.New(keypool.Options{DefaultCapacity: parsedConfiguration.DefaultPoolSize, LunaCapacity: parsedConfiguration.LunaPoolSize, EntryTTL: parsedConfiguration.SessionTTL})
	if parsedConfiguration.StateDir == "" {
		return scheduler, poolManager, nil, fallbackSnapshot
	}
	stateStore := state.NewStore(parsedConfiguration.StateDir)
	snapshot, loadResult := stateStore.Load()
	if loadResult != state.LoadOK && loadResult != state.LoadMigrated {
		snapshot = fallbackSnapshot
	}
	scheduler.Restore(snapshot.Routing, snapshot.Sessions, snapshot.Aliases)
	poolManager.Restore(snapshot.Keys)
	return scheduler, poolManager, stateStore, snapshot
}

func (dispatcher *Dispatcher) snapshotRuntimeState() state.Snapshot {
	dispatcher.mutex.RLock()
	scheduler, poolManager := dispatcher.scheduler, dispatcher.poolManager
	dispatcher.mutex.RUnlock()
	if scheduler == nil || poolManager == nil {
		return state.EmptySnapshot()
	}
	routingSnapshot, sessionsSnapshot, aliasesSnapshot := scheduler.Snapshot()
	return state.Snapshot{Version: state.CurrentVersion, Routing: routingSnapshot, Sessions: sessionsSnapshot, Aliases: aliasesSnapshot, Keys: poolManager.Snapshot()}
}

func (dispatcher *Dispatcher) routeModel(rawRequest []byte) []byte {
	var request rpcModelRouteRequest
	if err := json.Unmarshal(rawRequest, &request); err != nil {
		return ErrorEnvelope("invalid_request", "invalid model route request", false, http.StatusBadRequest)
	}
	dispatcher.mutex.RLock()
	providerID, modelPatterns := dispatcher.configuration.ProviderID, append([]string(nil), dispatcher.configuration.ModelPatterns...)
	dispatcher.mutex.RUnlock()
	if !routing.ModelMatches(modelPatterns, request.RequestedModel) {
		return SuccessEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	return SuccessEnvelope(pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetExecutor, Target: providerID, Reason: "configured AIHub model pattern"})
}

func (dispatcher *Dispatcher) pickSchedulerCandidate(rawRequest []byte) []byte {
	var request pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(rawRequest, &request); err != nil {
		return ErrorEnvelope("invalid_request", "invalid scheduler pick request", false, http.StatusBadRequest)
	}
	dispatcher.mutex.RLock()
	providerID, scheduler := dispatcher.configuration.ProviderID, dispatcher.scheduler
	dispatcher.mutex.RUnlock()
	if scheduler == nil {
		return SuccessEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	defer dispatcher.saveState()
	return SuccessEnvelope(scheduler.Pick(request, providerID))
}

func (dispatcher *Dispatcher) parseAuth(rawRequest []byte) []byte {
	var request pluginapi.AuthParseRequest
	if err := json.Unmarshal(rawRequest, &request); err != nil {
		return ErrorEnvelope("invalid_request", "invalid auth material", false, http.StatusBadRequest)
	}
	dispatcher.mutex.RLock()
	providerID := dispatcher.configuration.ProviderID
	dispatcher.mutex.RUnlock()
	if request.Provider != "" && request.Provider != providerID {
		return SuccessEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if request.Provider == "" && !isAIHubPersistedRecord(request.RawJSON, providerID) {
		return SuccessEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	storage, errParse := parseAuthStorage(request.RawJSON)
	if errParse != nil || storage.AccessToken == "" {
		return SuccessEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return SuccessEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: dispatcher.authData(storage, request.FileName)})
}

func (dispatcher *Dispatcher) startLogin(rawRequest []byte) []byte {
	var request pluginapi.AuthLoginStartRequest
	if err := json.Unmarshal(rawRequest, &request); err != nil {
		return ErrorEnvelope("invalid_request", "invalid login request", false, http.StatusBadRequest)
	}
	return ErrorEnvelope("login_unavailable", "AIHub credential import is available only through the authenticated management API", false, http.StatusNotImplemented)
}

func (dispatcher *Dispatcher) pollLogin(rawRequest []byte) []byte {
	var request pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(rawRequest, &request); err != nil {
		return ErrorEnvelope("invalid_request", "invalid login poll request", false, http.StatusBadRequest)
	}
	return SuccessEnvelope(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "AIHub credential import is available only through the authenticated management API"})
}

func (dispatcher *Dispatcher) refreshAuth(rawRequest []byte) []byte {
	var request rpcAuthRefreshRequest
	if err := json.Unmarshal(rawRequest, &request); err != nil {
		return ErrorEnvelope("invalid_request", "invalid refresh request", false, http.StatusBadRequest)
	}
	storage, errParse := parseAuthStorage(request.StorageJSON)
	if errParse != nil || storage.RefreshToken == "" {
		return ErrorEnvelope("invalid_auth", "AIHub refresh credentials are unavailable", false, http.StatusUnauthorized)
	}
	dispatcher.mutex.RLock()
	baseURL := dispatcher.configuration.BaseURL
	dispatcher.mutex.RUnlock()
	requestContext, cancelRequest := boundedContext(request.HostCallbackID)
	defer cancelRequest()
	session, err := dispatcher.clientFactory(baseURL).Refresh(requestContext, storage.RefreshToken)
	if err != nil {
		return ErrorEnvelope("refresh_failed", "AIHub token refresh failed", true, http.StatusBadGateway)
	}
	storage.AccessToken, storage.RefreshToken, storage.ExpiresAt = session.AccessToken, session.RefreshToken, session.ExpiresAt
	return SuccessEnvelope(pluginapi.AuthRefreshResponse{Auth: dispatcher.authData(storage, ""), NextRefreshAfter: session.ExpiresAt.Add(-5 * time.Minute)})
}

func (dispatcher *Dispatcher) modelsForAuth(rawRequest []byte) []byte {
	var request rpcAuthModelRequest
	if err := json.Unmarshal(rawRequest, &request); err != nil {
		return ErrorEnvelope("invalid_request", "invalid model request", false, http.StatusBadRequest)
	}
	storage, errParse := parseAuthStorage(request.StorageJSON)
	if errParse != nil || storage.AccessToken == "" {
		return ErrorEnvelope("invalid_auth", "AIHub credentials are unavailable", false, http.StatusUnauthorized)
	}
	dispatcher.mutex.RLock()
	baseURL, providerID := dispatcher.configuration.BaseURL, dispatcher.configuration.ProviderID
	dispatcher.mutex.RUnlock()
	requestContext, cancelRequest := boundedContext(request.HostCallbackID)
	defer cancelRequest()
	models, err := dispatcher.clientFactory(baseURL).Models(requestContext, storage.AccessToken)
	if err != nil {
		return ErrorEnvelope("model_discovery_failed", "AIHub model discovery failed", true, http.StatusBadGateway)
	}
	result := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		result = append(result, pluginapi.ModelInfo{ID: model.ID, Name: model.ID, Object: "model", OwnedBy: providerID, DisplayName: model.DisplayName, SupportedGenerationMethods: []string{"chat"}})
	}
	return SuccessEnvelope(pluginapi.ModelResponse{Provider: providerID, Models: result})
}

func (dispatcher *Dispatcher) execute(rawRequest []byte, stream bool) []byte {
	var request rpcExecutorRequest
	if err := json.Unmarshal(rawRequest, &request); err != nil {
		return ErrorEnvelope("invalid_request", "invalid executor request", false, http.StatusBadRequest)
	}
	if request.Format != "chat-completions" {
		return ErrorEnvelope("executor_unavailable", "AIHub pass-through supports only chat-completions", false, http.StatusNotImplemented)
	}
	storage, errParse := parseAuthStorage(request.StorageJSON)
	if errParse != nil || storage.AccessToken == "" {
		return ErrorEnvelope("invalid_auth", "AIHub credentials are unavailable", false, http.StatusUnauthorized)
	}
	dispatcher.mutex.RLock()
	baseURL, maxResponseBytes, maxRequestBytes := dispatcher.configuration.BaseURL, dispatcher.configuration.MaxResponseBytes, dispatcher.configuration.MaxRequestBytes
	dispatcher.mutex.RUnlock()
	if int64(len(request.Payload)) > maxRequestBytes {
		return ErrorEnvelope("request_too_large", "executor payload exceeds configured byte limit", false, http.StatusRequestEntityTooLarge)
	}
	requestContext, cancelRequest := boundedContext(request.HostCallbackID)
	defer cancelRequest()
	startedAt := time.Now()
	if stream {
		response, errExecute := dispatcher.clientFactory(baseURL).ExecuteStream(requestContext, "/v1/chat/completions", storage.AccessToken, request.Headers, request.Payload)
		if errExecute != nil {
			dispatcher.recordExecutionOutcome(storage.AccountID, false, startedAt)
			return ErrorEnvelope("upstream_error", "AIHub request failed", true, http.StatusBadGateway)
		}
		payload, errRead := aihub.CollectBoundedStream(response.Chunks, maxResponseBytes)
		if errRead != nil {
			dispatcher.recordExecutionOutcome(storage.AccountID, false, startedAt)
			return ErrorEnvelope("response_too_large", "AIHub stream response exceeds configured byte limit", false, http.StatusBadGateway)
		}
		dispatcher.recordExecutionOutcome(storage.AccountID, true, startedAt)
		return SuccessEnvelope(streamResponse{Headers: safeResponseHeaders(response.Headers), Chunks: []pluginapi.ExecutorStreamChunk{{Payload: payload}}})
	}
	response, errExecute := dispatcher.clientFactory(baseURL).ExecuteBounded(requestContext, "/v1/chat/completions", storage.AccessToken, request.Headers, request.Payload, maxResponseBytes)
	if errExecute != nil {
		dispatcher.recordExecutionOutcome(storage.AccountID, false, startedAt)
		return ErrorEnvelope("upstream_error", "AIHub request failed", true, http.StatusBadGateway)
	}
	dispatcher.recordExecutionOutcome(storage.AccountID, true, startedAt)
	return SuccessEnvelope(pluginapi.ExecutorResponse{Payload: response.Body, Headers: safeResponseHeaders(response.Headers), Metadata: map[string]any{"status_code": response.StatusCode}})
}

func (dispatcher *Dispatcher) recordExecutionOutcome(authID string, success bool, startedAt time.Time) {
	dispatcher.mutex.RLock()
	scheduler := dispatcher.scheduler
	dispatcher.mutex.RUnlock()
	if scheduler == nil || authID == "" {
		return
	}
	scheduler.RecordExecutionOutcome(authID, success, float64(time.Since(startedAt).Microseconds())/1000, time.Now())
	dispatcher.saveState()
}

func (dispatcher *Dispatcher) authData(storage authStorage, fileName string) pluginapi.AuthData {
	rawStorage, _ := json.Marshal(storage)
	dispatcher.mutex.RLock()
	providerID := dispatcher.configuration.ProviderID
	dispatcher.mutex.RUnlock()
	identifier := storage.AccountID
	if identifier == "" {
		identifier = storage.Label
	}
	if identifier == "" {
		identifier = "aihub-account"
	}
	plan := storage.Plan
	if plan == "" {
		plan = "unknown"
	}
	attributes := map[string]string{
		routing.AttributePlan:    plan,
		routing.AttributeGroupID: "1",
		routing.AttributeRate:    "1",
		routing.AttributeTTFT:    "1000",
	}
	if storage.AccountID != "" {
		attributes[routing.AttributeAccountID] = storage.AccountID
	}
	return pluginapi.AuthData{Provider: providerID, ID: identifier, FileName: fileName, Label: storage.Label, StorageJSON: rawStorage, Attributes: attributes, NextRefreshAfter: storage.ExpiresAt.Add(-5 * time.Minute)}
}
func staticModels(providerID string) []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{}
}
func safeResponseHeaders(source http.Header) http.Header {
	destination := make(http.Header)
	for name, values := range source {
		switch http.CanonicalHeaderKey(name) {
		case "Authorization", "Proxy-Authorization", "Set-Cookie", "Connection", "Transfer-Encoding", "Content-Length":
			continue
		}
		destination[name] = append([]string(nil), values...)
	}
	return destination
}

func (dispatcher *Dispatcher) registration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             PluginIdentifier,
			Version:          "0.1.0",
			Author:           "aihub-auto",
			GitHubRepository: "https://github.com/mimmer/aihub-auto",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "provider_id", Type: pluginapi.ConfigFieldTypeString, Description: "CPA provider identifier for this plugin."},
				{Name: "max_request_bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum accepted executor or management request body size in bytes."},
				{Name: "max_response_bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum buffered upstream response size in bytes."},
				{Name: "state_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Absolute directory for versioned non-secret routing state."},
				{Name: "default_pool_size", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum runtime-only managed key metadata entries per account/plan default pool."},
				{Name: "luna_pool_size", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum runtime-only managed key metadata entries per account/plan Luna pool."},
				{Name: "session_ttl", Type: pluginapi.ConfigFieldTypeString, Description: "Duration for scoped session and Responses alias affinity."},
				{Name: "management_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Exposes non-sensitive management status."},
			},
		},
		Capabilities: capabilities{
			ManagementAPI: true,
			AuthProvider:  true, ModelProvider: true, Executor: true, Scheduler: true, ModelRouter: true,
			ExecutorModelScope: pluginapi.ExecutorModelScopeOAuth, ExecutorInputFormats: []string{"chat-completions"}, ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

func (dispatcher *Dispatcher) managementRegistration() managementRegistrationResponse {
	if !dispatcher.configuration.ManagementEnabled {
		return managementRegistrationResponse{}
	}
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/plugins/aihub-auto/status", Description: "Returns non-secret plugin routing and runtime status."},
			{Method: http.MethodPost, Path: "/plugins/aihub-auto/credentials", Description: "Imports AIHub credentials through CPA authentication."},
			{Method: http.MethodPost, Path: "/plugins/aihub-auto/config", Description: "Safely updates non-secret plugin configuration."},
			{Method: http.MethodPost, Path: "/plugins/aihub-auto/lock", Description: "Sets an in-memory candidate preference."},
			{Method: http.MethodPost, Path: "/plugins/aihub-auto/unlock", Description: "Clears the in-memory candidate preference."},
			{Method: http.MethodPost, Path: "/plugins/aihub-auto/reconsider", Description: "Requests immediate in-memory candidate reconsideration."},
			{Method: http.MethodPost, Path: "/plugins/aihub-auto/sessions/clear", Description: "Clears in-memory session affinity."},
			{Method: http.MethodPost, Path: "/plugins/aihub-auto/aliases/clear", Description: "Clears in-memory Responses aliases."},
			{Method: http.MethodPost, Path: "/plugins/aihub-auto/pools/reconcile", Description: "Reconciles non-secret runtime pool metadata."},
		},
		Resources: []pluginapi.ResourceRoute{{Path: "/dashboard", Menu: "AIHub Auto", Description: "Shows the AIHub Auto management dashboard."}},
	}
}

func (dispatcher *Dispatcher) handleManagement(rawRequest []byte) []byte {
	var request rpcManagementRequest
	if errUnmarshal := json.Unmarshal(rawRequest, &request); errUnmarshal != nil {
		return ErrorEnvelope("invalid_request", "invalid management request", false, http.StatusBadRequest)
	}
	dispatcher.mutex.RLock()
	managementEnabled, maxRequestBytes := dispatcher.configuration.ManagementEnabled, dispatcher.configuration.MaxRequestBytes
	dispatcher.mutex.RUnlock()
	if int64(len(request.Body)) > maxRequestBytes {
		return SuccessEnvelope(managementJSONResponse(http.StatusRequestEntityTooLarge, map[string]string{"error": "request body exceeds configured byte limit"}))
	}
	if !managementEnabled {
		return SuccessEnvelope(managementJSONResponse(http.StatusNotFound, map[string]string{"error": "management disabled"}))
	}
	if request.Method == http.MethodGet && request.Path == "/v0/resource/plugins/aihub-auto/dashboard" {
		return SuccessEnvelope(managementHTMLResponse())
	}
	const basePath = "/v0/management/plugins/aihub-auto"
	if request.Path == basePath+"/status" {
		if request.Method != http.MethodGet {
			return SuccessEnvelope(managementJSONResponse(http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"}))
		}
		return SuccessEnvelope(dispatcher.managementStatus())
	}
	if request.Method != http.MethodPost {
		return SuccessEnvelope(managementJSONResponse(http.StatusNotFound, map[string]string{"error": "resource not found"}))
	}
	if !isJSONContentType(request.Headers) {
		return SuccessEnvelope(managementJSONResponse(http.StatusUnsupportedMediaType, map[string]string{"error": "application/json content type is required"}))
	}
	switch request.Path {
	case basePath + "/credentials":
		return dispatcher.importCredentials(request.Body, request.HostCallbackID)
	case basePath + "/config":
		return SuccessEnvelope(dispatcher.updateManagementConfiguration(request.Body))
	case basePath + "/lock":
		return SuccessEnvelope(dispatcher.setManagementLock(request.Body))
	case basePath + "/unlock":
		return SuccessEnvelope(dispatcher.clearManagementLock(request.Body))
	case basePath + "/reconsider":
		if !isEmptyJSONObject(request.Body) {
			return SuccessEnvelope(managementJSONResponse(http.StatusBadRequest, map[string]string{"error": "request must be an empty JSON object"}))
		}
		return SuccessEnvelope(managementJSONResponse(http.StatusOK, map[string]bool{"reconsidered": true}))
	case basePath + "/sessions/clear":
		return SuccessEnvelope(dispatcher.clearManagementSessions(request.Body, false))
	case basePath + "/aliases/clear":
		return SuccessEnvelope(dispatcher.clearManagementSessions(request.Body, true))
	case basePath + "/pools/reconcile":
		if !isEmptyJSONObject(request.Body) {
			return SuccessEnvelope(managementJSONResponse(http.StatusBadRequest, map[string]string{"error": "request must be an empty JSON object"}))
		}
		return SuccessEnvelope(dispatcher.reconcileManagementPools())
	default:
		return SuccessEnvelope(managementJSONResponse(http.StatusNotFound, map[string]string{"error": "resource not found"}))
	}
}

func (dispatcher *Dispatcher) importCredentials(rawRequest []byte, callbackID string) []byte {
	var request struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeStrictJSON(rawRequest, &request) {
		return SuccessEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"invalid credential import request"}`)})
	}
	request.Email = strings.TrimSpace(request.Email)
	if request.Email == "" || request.Password == "" {
		return SuccessEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"email and password are required"}`)})
	}
	dispatcher.mutex.RLock()
	baseURL, providerID, authSaver := dispatcher.configuration.BaseURL, dispatcher.configuration.ProviderID, dispatcher.authSaver
	dispatcher.mutex.RUnlock()
	if authSaver == nil {
		return SuccessEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusServiceUnavailable, Body: []byte(`{"error":"CPA auth persistence callback is unavailable"}`)})
	}
	client := dispatcher.clientFactory(baseURL)
	requestContext, cancelRequest := boundedContext(callbackID)
	defer cancelRequest()
	session, errLogin := client.Login(requestContext, request.Email, request.Password)
	if errLogin != nil {
		return SuccessEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusUnauthorized, Body: []byte(`{"error":"AIHub login failed"}`)})
	}
	account, _ := client.Me(requestContext, session.AccessToken)
	label := account.Label
	if label == "" {
		label = request.Email
	}
	storage := authStorage{AccountID: account.ID, Label: label, Plan: "unknown", AccessToken: session.AccessToken, RefreshToken: session.RefreshToken, ExpiresAt: session.ExpiresAt}
	persisted, _ := json.Marshal(struct {
		Type string `json:"type"`
		authStorage
	}{Type: providerID, authStorage: storage})
	fileName := fmt.Sprintf("aihub-auto-%d.json", time.Now().UnixNano())
	if errSave := authSaver(requestContext, fileName, persisted); errSave != nil {
		return SuccessEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusBadGateway, Body: []byte(`{"error":"could not save AIHub credentials"}`)})
	}
	return SuccessEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusCreated, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"status":"imported"}`)})
}

// parseAuthStorage accepts both the plugin-owned snake_case schema and the
// camelCase credentials persisted by aihub-auto's router application.
func parseAuthStorage(rawStorage []byte) (authStorage, error) {
	var raw map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(rawStorage, &raw); errUnmarshal != nil {
		return authStorage{}, errUnmarshal
	}
	readString := func(names ...string) string {
		for _, name := range names {
			var value string
			if json.Unmarshal(raw[name], &value) == nil && value != "" {
				return value
			}
		}
		return ""
	}
	storage := authStorage{
		AccountID:    readString("account_id", "accountId", "id"),
		Label:        readString("label", "email", "username"),
		Plan:         readString("plan", "plan_type", "planType"),
		AccessToken:  readString("access_token", "accessToken", "token"),
		RefreshToken: readString("refresh_token", "refreshToken"),
	}
	for _, name := range []string{"expires_at", "expiresAt"} {
		var timestamp time.Time
		if json.Unmarshal(raw[name], &timestamp) == nil && !timestamp.IsZero() {
			storage.ExpiresAt = timestamp
			break
		}
		var milliseconds int64
		if json.Unmarshal(raw[name], &milliseconds) == nil && milliseconds > 0 {
			storage.ExpiresAt = time.UnixMilli(milliseconds)
			break
		}
	}
	return storage, nil
}

func isAIHubPersistedRecord(rawStorage []byte, providerID string) bool {
	var persisted struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
	}
	if json.Unmarshal(rawStorage, &persisted) != nil {
		return false
	}
	return persisted.Type == providerID || persisted.Provider == providerID
}
