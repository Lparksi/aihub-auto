package aihub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestClientRejectsRedirectsAndDoesNotFollowThem(t *testing.T) {
	redirectTargetReached := false
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect-target" {
			redirectTargetReached = true
		}
		http.Redirect(responseWriter, request, "/redirect-target", http.StatusFound)
	}))
	defer server.Close()

	_, errExecute := NewClient(server.URL, nil).Execute(context.Background(), "/v1/chat/completions", "secret-token", nil, nil)
	var clientError *Error
	if !errors.As(errExecute, &clientError) || clientError.StatusCode != http.StatusFound {
		t.Fatalf("Execute() error = %#v, want sanitized redirect error", errExecute)
	}
	if redirectTargetReached {
		t.Fatal("client followed a redirect")
	}
}

func TestClientErrorsRedactTokensAndUpstreamBodies(t *testing.T) {
	const secretToken = "very-secret-access-token"
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		http.Error(responseWriter, "upstream body includes "+secretToken, http.StatusBadGateway)
	}))
	defer server.Close()

	_, errExecute := NewClient(server.URL, nil).Execute(context.Background(), "/v1/chat/completions", secretToken, nil, nil)
	if errExecute == nil {
		t.Fatal("Execute() error = nil, want upstream error")
	}
	if strings.Contains(errExecute.Error(), secretToken) || strings.Contains(errExecute.Error(), "upstream body") {
		t.Fatalf("error leaked sensitive upstream detail: %q", errExecute)
	}
}

func TestClientTimeoutIsConfigurable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	_, errExecute := NewClientWithTimeout(server.URL, nil, 10*time.Millisecond).Execute(context.Background(), "/v1/chat/completions", "secret-token", nil, nil)
	if errExecute == nil {
		t.Fatal("Execute() error = nil, want timeout")
	}
	if strings.Contains(errExecute.Error(), "secret-token") {
		t.Fatalf("timeout error leaked token: %q", errExecute)
	}
}

func TestModelsSendsAuthorizationAndParsesCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer access-token" {
			t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
		}
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"data":[{"id":"model-a","display_name":"Model A"},{"id":"model-b"}]}`))
	}))
	defer server.Close()

	models, errModels := NewClient(server.URL, nil).Models(context.Background(), "access-token")
	if errModels != nil {
		t.Fatalf("Models() error = %v", errModels)
	}
	if len(models) != 2 || models[0].DisplayName != "Model A" || models[1].DisplayName != "model-b" {
		t.Fatalf("Models() = %#v, want parsed catalog", models)
	}
}

func TestClientUsesProvidedHostHTTPClient(t *testing.T) {
	hostClient := &recordingHostHTTPClient{response: pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"data":[{"id":"model-a"}]}`)}}
	models, errModels := NewClientWithHostHTTPClient("https://aihub.example", hostClient).Models(context.Background(), "access-token")
	if errModels != nil || len(models) != 1 || hostClient.request.URL != "https://aihub.example/v1/models" {
		t.Fatalf("models = %#v, error = %v, host request = %#v", models, errModels, hostClient.request)
	}
	if hostClient.request.Headers.Get("Authorization") != "Bearer access-token" {
		t.Fatalf("host Authorization = %q, want bearer token", hostClient.request.Headers.Get("Authorization"))
	}
}

type recordingHostHTTPClient struct {
	request  pluginapi.HTTPRequest
	response pluginapi.HTTPResponse
}

func (client *recordingHostHTTPClient) Do(_ context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	client.request = request
	return client.response, nil
}

func (client *recordingHostHTTPClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("unexpected stream request")
}
