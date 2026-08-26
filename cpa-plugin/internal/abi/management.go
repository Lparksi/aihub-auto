package abi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/configuration"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (dispatcher *Dispatcher) managementStatus() pluginapi.ManagementResponse {
	dispatcher.mutex.RLock()
	currentConfiguration := dispatcher.configuration
	scheduler := dispatcher.scheduler
	poolManager := dispatcher.poolManager
	stateDiagnostic := dispatcher.stateDiagnostic
	dispatcher.mutex.RUnlock()

	sessionCount, aliasCount := 0, 0
	manualLockActive := false
	if scheduler != nil {
		sessionCount, aliasCount = scheduler.AffinityCounts()
		manualLockActive = scheduler.ManualLockActive()
	}
	defaultEntries, lunaEntries, defaultCapacity, lunaCapacity, lifecycleConfirmed := 0, 0, 0, 0, false
	if poolManager != nil {
		defaultEntries, lunaEntries, defaultCapacity, lunaCapacity, lifecycleConfirmed = poolManager.Summary()
	}
	return managementJSONResponse(http.StatusOK, map[string]any{
		"plugin": "aihub-auto",
		"configuration": map[string]any{
			"provider_id": currentConfiguration.ProviderID, "base_url": currentConfiguration.BaseURL,
			"mode": currentConfiguration.Mode, "price_band_min": currentConfiguration.PriceBandMin,
			"price_band_max": currentConfiguration.PriceBandMax, "model_patterns": currentConfiguration.ModelPatterns,
			"management_enabled": currentConfiguration.ManagementEnabled,
		},
		"routing": map[string]any{"candidate_summary": "candidate details are supplied only by each scheduler request", "manual_lock_active": manualLockActive, "breaker": "in-memory", "observations": "in-memory"},
		"pools": map[string]any{
			"default":             map[string]int{"entries": defaultEntries, "capacity": defaultCapacity},
			"luna":                map[string]int{"entries": lunaEntries, "capacity": lunaCapacity},
			"lifecycle_confirmed": lifecycleConfirmed,
		},
		"sessions":    map[string]int{"bindings": sessionCount, "aliases": aliasCount},
		"diagnostics": map[string]any{"key_operations": "not available: AIHub lifecycle endpoint is unconfirmed", "state": stateDiagnostic},
	})
}

func (dispatcher *Dispatcher) updateManagementConfiguration(rawRequest []byte) pluginapi.ManagementResponse {
	var request struct {
		ConfigYAML string `json:"config_yaml"`
	}
	if !decodeStrictJSON(rawRequest, &request) || request.ConfigYAML == "" {
		return managementJSONResponse(http.StatusBadRequest, map[string]string{"error": "config_yaml is required and must be the only field"})
	}
	parsedConfiguration, errParse := configuration.Parse([]byte(request.ConfigYAML))
	if errParse != nil {
		return managementJSONResponse(http.StatusBadRequest, map[string]string{"error": "invalid configuration"})
	}
	fallbackSnapshot := dispatcher.snapshotRuntimeState()
	newScheduler, newPoolManager, newStore, newSnapshot := buildRuntime(parsedConfiguration, fallbackSnapshot)

	dispatcher.mutex.Lock()
	previousConfiguration := dispatcher.configuration
	previousScheduler := dispatcher.scheduler
	previousPoolManager := dispatcher.poolManager
	previousStore := dispatcher.stateStore
	previousStateDiagnostic := dispatcher.stateDiagnostic
	dispatcher.configuration = parsedConfiguration
	dispatcher.scheduler = newScheduler
	dispatcher.poolManager = newPoolManager
	dispatcher.stateStore = newStore
	if newStore == nil {
		dispatcher.stateDiagnostic = "in-memory runtime state only"
	} else {
		dispatcher.stateDiagnostic = "persistent state configured and healthy"
	}
	dispatcher.mutex.Unlock()
	if newStore != nil && newStore.Save(newSnapshot) != nil {
		dispatcher.mutex.Lock()
		dispatcher.configuration = previousConfiguration
		dispatcher.scheduler = previousScheduler
		dispatcher.poolManager = previousPoolManager
		dispatcher.stateStore = previousStore
		dispatcher.stateDiagnostic = previousStateDiagnostic
		dispatcher.mutex.Unlock()
		return managementJSONResponse(http.StatusInternalServerError, map[string]string{"error": "state reconfiguration failed"})
	}
	return managementJSONResponse(http.StatusOK, map[string]bool{"reconfigured": true})
}

func (dispatcher *Dispatcher) setManagementLock(rawRequest []byte) pluginapi.ManagementResponse {
	var request struct {
		AuthID string `json:"auth_id"`
	}
	if !decodeStrictJSON(rawRequest, &request) || strings.TrimSpace(request.AuthID) == "" {
		return managementJSONResponse(http.StatusBadRequest, map[string]string{"error": "auth_id is required and must be the only field"})
	}
	dispatcher.mutex.RLock()
	scheduler := dispatcher.scheduler
	dispatcher.mutex.RUnlock()
	if scheduler == nil {
		return managementJSONResponse(http.StatusServiceUnavailable, map[string]string{"error": "scheduler is unavailable"})
	}
	scheduler.SetManualLock(request.AuthID)
	dispatcher.saveState()
	return managementJSONResponse(http.StatusOK, map[string]bool{"locked": true})
}

func (dispatcher *Dispatcher) clearManagementLock(rawRequest []byte) pluginapi.ManagementResponse {
	if !isEmptyJSONObject(rawRequest) {
		return managementJSONResponse(http.StatusBadRequest, map[string]string{"error": "request must be an empty JSON object"})
	}
	dispatcher.mutex.RLock()
	scheduler := dispatcher.scheduler
	dispatcher.mutex.RUnlock()
	if scheduler != nil {
		scheduler.SetManualLock("")
	}
	dispatcher.saveState()
	return managementJSONResponse(http.StatusOK, map[string]bool{"locked": false})
}

func (dispatcher *Dispatcher) clearManagementSessions(rawRequest []byte, aliases bool) pluginapi.ManagementResponse {
	if !isEmptyJSONObject(rawRequest) {
		return managementJSONResponse(http.StatusBadRequest, map[string]string{"error": "request must be an empty JSON object"})
	}
	dispatcher.mutex.RLock()
	scheduler := dispatcher.scheduler
	dispatcher.mutex.RUnlock()
	cleared := 0
	if scheduler != nil {
		if aliases {
			cleared = scheduler.ClearAliases()
		} else {
			cleared = scheduler.ClearSessions()
		}
	}
	dispatcher.saveState()
	return managementJSONResponse(http.StatusOK, map[string]int{"cleared": cleared})
}

func (dispatcher *Dispatcher) reconcileManagementPools() pluginapi.ManagementResponse {
	dispatcher.mutex.RLock()
	poolManager := dispatcher.poolManager
	dispatcher.mutex.RUnlock()
	if poolManager == nil {
		return managementJSONResponse(http.StatusServiceUnavailable, map[string]string{"error": "pool manager is unavailable"})
	}
	removed, errReconcile := poolManager.Reconcile(context.Background())
	if errReconcile != nil {
		return managementJSONResponse(http.StatusBadGateway, map[string]string{"error": "pool reconciliation failed"})
	}
	return managementJSONResponse(http.StatusOK, map[string]any{"removed": removed, "lifecycle_confirmed": false})
}

func isJSONContentType(headers http.Header) bool {
	contentType := strings.ToLower(strings.TrimSpace(headers.Get("Content-Type")))
	return contentType == "application/json" || strings.HasPrefix(contentType, "application/json;")
}

func isEmptyJSONObject(rawRequest []byte) bool {
	var request map[string]json.RawMessage
	return decodeStrictJSON(rawRequest, &request) && len(request) == 0
}

func decodeStrictJSON(rawRequest []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(rawRequest))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func managementJSONResponse(statusCode int, payload any) pluginapi.ManagementResponse {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		body = []byte(`{"error":"response encoding failed"}`)
		statusCode = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{StatusCode: statusCode, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: body}
}

func managementHTMLResponse() pluginapi.ManagementResponse {
	const dashboard = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>AIHub Auto</title></head><body><main><h1>AIHub Auto</h1><p id="notice">Loading authenticated status...</p><pre id="status"></pre><button id="refresh" type="button">Refresh</button></main><script>(function(){"use strict";var status=document.getElementById("status"),notice=document.getElementById("notice");function show(value){status.textContent=JSON.stringify(value,null,2);notice.textContent="Authenticated status loaded."}function load(){notice.textContent="Loading authenticated status...";fetch("/v0/management/plugins/aihub-auto/status",{credentials:"same-origin",headers:{"Accept":"application/json"}}).then(function(response){if(!response.ok){throw new Error("status request failed")};return response.json()}).then(show).catch(function(){notice.textContent="Unable to load authenticated status.";status.textContent=""})}document.getElementById("refresh").addEventListener("click",load);load()}());</script></body></html>`
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}, "Content-Security-Policy": []string{"default-src 'none'; script-src 'unsafe-inline'; connect-src 'self'; style-src 'none'; base-uri 'none'; form-action 'none'"}}, Body: []byte(dashboard)}
}
