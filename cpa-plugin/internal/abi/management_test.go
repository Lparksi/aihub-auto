package abi

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/state"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const managementBasePath = "/v0/management/plugins/aihub-auto"

func TestManagementRegistersExactRoutesAndDashboardResource(t *testing.T) {
	dispatcher := NewDispatcher()
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			Routes    []pluginapi.ManagementRoute `json:"routes"`
			Resources []pluginapi.ResourceRoute   `json:"resources"`
		} `json:"result"`
	}
	decodeEnvelope(t, dispatcher.Handle(pluginabi.MethodManagementRegister, nil), &response)
	if !response.OK || len(response.Result.Routes) != 9 || len(response.Result.Resources) != 1 {
		t.Fatalf("management registration = %#v, want nine API routes and one dashboard resource", response)
	}
	if response.Result.Resources[0].Path != "/dashboard" {
		t.Fatalf("resource route = %#v, want /dashboard", response.Result.Resources[0])
	}
}

func TestManagementRejectsNearMissMethodsPathsContentTypesAndUnknownFields(t *testing.T) {
	dispatcher := NewDispatcher()
	for testName, request := range map[string]pluginapi.ManagementRequest{
		"wrong method":       {Method: http.MethodGet, Path: managementBasePath + "/lock"},
		"near miss path":     {Method: http.MethodPost, Path: managementBasePath + "/status/", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{}`)},
		"wrong content type": {Method: http.MethodPost, Path: managementBasePath + "/lock", Headers: http.Header{"Content-Type": []string{"text/plain"}}, Body: []byte(`{"auth_id":"candidate"}`)},
		"unknown field":      {Method: http.MethodPost, Path: managementBasePath + "/lock", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"auth_id":"candidate","extra":true}`)},
	} {
		t.Run(testName, func(testingHandle *testing.T) {
			managementResponse := callManagement(testingHandle, dispatcher, request)
			if managementResponse.StatusCode != http.StatusBadRequest && managementResponse.StatusCode != http.StatusNotFound && managementResponse.StatusCode != http.StatusMethodNotAllowed && managementResponse.StatusCode != http.StatusUnsupportedMediaType {
				testingHandle.Fatalf("status = %d, want strict request rejection", managementResponse.StatusCode)
			}
		})
	}
}

func TestManagementDashboardAndStatusNeverExposeSecrets(t *testing.T) {
	dispatcher := configuredDispatcher(t, "https://aihub.example", "manual_lock_auth_id: safe-lock\n")
	for _, request := range []pluginapi.ManagementRequest{
		{Method: http.MethodGet, Path: managementBasePath + "/status"},
		{Method: http.MethodGet, Path: "/v0/resource/plugins/aihub-auto/dashboard"},
	} {
		managementResponse := callManagement(t, dispatcher, request)
		if managementResponse.StatusCode != http.StatusOK {
			t.Fatalf("%s response status = %d, want 200", request.Path, managementResponse.StatusCode)
		}
		body := string(managementResponse.Body)
		for _, forbidden := range []string{"access_token", "refresh_token", "password", "api_key", "safe-lock"} {
			if strings.Contains(strings.ToLower(body), forbidden) {
				t.Fatalf("%s response leaked %q: %s", request.Path, forbidden, body)
			}
		}
	}
}

func TestManagementStatusReportsPersistentStateWithoutExposingDirectory(t *testing.T) {
	dispatcher := configuredDispatcher(t, "https://aihub.example", "state_dir: "+t.TempDir()+"\n")
	managementResponse := callManagement(t, dispatcher, pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementBasePath + "/status"})
	if managementResponse.StatusCode != http.StatusOK || !strings.Contains(string(managementResponse.Body), `"state":"persistent state configured and healthy"`) {
		t.Fatalf("status = %s, want healthy persistent state diagnostic", managementResponse.Body)
	}
}

func TestManagementManualLockAndResetsChangeOnlyInMemoryState(t *testing.T) {
	dispatcher := NewDispatcher()
	lockResponse := callManagement(t, dispatcher, pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + "/lock", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"auth_id":"locked-candidate"}`)})
	if lockResponse.StatusCode != http.StatusOK || !strings.Contains(string(lockResponse.Body), `"locked":true`) {
		t.Fatalf("lock response = %#v, want successful in-memory lock", lockResponse)
	}
	statusResponse := callManagement(t, dispatcher, pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementBasePath + "/status"})
	if strings.Contains(string(statusResponse.Body), "locked-candidate") || !strings.Contains(string(statusResponse.Body), `"manual_lock_active":true`) {
		t.Fatalf("status = %s, want redacted active manual lock", statusResponse.Body)
	}
	for _, path := range []string{"/sessions/clear", "/aliases/clear", "/unlock"} {
		resetResponse := callManagement(t, dispatcher, pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + path, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{}`)})
		if resetResponse.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, resetResponse.StatusCode)
		}
	}
}

func TestManagementDisabledRejectsAPIAndResource(t *testing.T) {
	dispatcher := configuredDispatcher(t, "https://aihub.example", "management_enabled: false\n")
	for _, request := range []pluginapi.ManagementRequest{{Method: http.MethodGet, Path: managementBasePath + "/status"}, {Method: http.MethodGet, Path: "/v0/resource/plugins/aihub-auto/dashboard"}} {
		managementResponse := callManagement(t, dispatcher, request)
		if managementResponse.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404 while disabled", request.Path, managementResponse.StatusCode)
		}
	}
}

func TestManagementStateDirectoryReconfigurationMigratesRuntimeState(t *testing.T) {
	originalDirectory := t.TempDir()
	targetDirectory := t.TempDir()
	dispatcher := configuredDispatcher(t, "https://aihub.example", "state_dir: "+originalDirectory+"\n")
	dispatcher.scheduler.Restore(state.RoutingSnapshot{Breaker: map[string]state.BreakerRecord{"account\u0000pro\u00007": {State: "open", Failures: 3, Opens: 1, OpenedAt: time.Unix(100, 0)}}, Observations: map[string]state.ObservationRecord{"account\u0000pro\u00007": {Samples: 4, Successes: 3, EWMA: 250}}}, map[string]state.SessionRecord{"session": {AccountID: "account", Plan: "pro", Pool: state.PoolDefault, GroupID: 7, LastUsedAt: time.Unix(100, 0)}}, map[string]state.AliasRecord{"alias": {SessionKey: "session", AccountID: "account", Plan: "pro", Pool: state.PoolDefault, GroupID: 7, LastUsedAt: time.Unix(100, 0)}})
	dispatcher.poolManager.Restore(map[string]state.KeyMetadata{"key": {AccountID: "account", Plan: "pro", Pool: state.PoolDefault, GroupID: 7, KeyID: "remote-id", LastUsedAt: time.Unix(100, 0)}})

	response := callManagement(t, dispatcher, pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + "/config", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"config_yaml":"state_dir: ` + targetDirectory + `\n"}`)})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reconfigure status = %d, body = %s", response.StatusCode, response.Body)
	}
	loadedSnapshot, loadResult := state.NewStore(targetDirectory).Load()
	if loadResult != state.LoadOK || loadedSnapshot.Routing.Breaker["account\x00pro\x007"].Failures != 3 || loadedSnapshot.Routing.Observations["account\x00pro\x007"].Successes != 3 || len(loadedSnapshot.Sessions) != 1 || len(loadedSnapshot.Aliases) != 1 || loadedSnapshot.Keys["account\x00pro\x00default\x007"].KeyID != "remote-id" {
		t.Fatalf("migrated snapshot = %#v, %s; want complete persisted runtime state", loadedSnapshot, loadResult)
	}
}

func TestManagementStateDirectoryReconfigurationRollsBackOnSaveFailure(t *testing.T) {
	dispatcher := configuredDispatcher(t, "https://aihub.example")
	originalStore := dispatcher.stateStore
	originalScheduler := dispatcher.scheduler
	blockedPath := t.TempDir() + "/blocked"
	if errWrite := os.WriteFile(blockedPath, []byte("not a directory"), 0o600); errWrite != nil {
		t.Fatalf("create blocked path: %v", errWrite)
	}
	response := callManagement(t, dispatcher, pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementBasePath + "/config", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"config_yaml":"state_dir: ` + blockedPath + `\n"}`)})
	if response.StatusCode != http.StatusInternalServerError || dispatcher.stateStore != originalStore || dispatcher.scheduler != originalScheduler {
		t.Fatalf("failed reconfigure = status %d store %#v scheduler %#v, want preserved runtime", response.StatusCode, dispatcher.stateStore, dispatcher.scheduler)
	}
}

func callManagement(testingHandle *testing.T, dispatcher *Dispatcher, request pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	testingHandle.Helper()
	rawRequest, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		testingHandle.Fatalf("marshal management request: %v", errMarshal)
	}
	var response struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.ManagementResponse `json:"result"`
	}
	decodeEnvelope(testingHandle, dispatcher.Handle(pluginabi.MethodManagementHandle, rawRequest), &response)
	if !response.OK {
		testingHandle.Fatalf("management envelope = %#v, want success envelope", response)
	}
	return response.Result
}
