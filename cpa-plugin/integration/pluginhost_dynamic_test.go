//go:build cpa_dynamic_loader && cgo && (darwin || linux)

package integration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginhost"
	"gopkg.in/yaml.v3"
)

// TestPluginLoadsThroughCPAPluginHost builds the native library and lets CPA's
// public plugin host dlopen it. It deliberately makes no fake loader claim.
func TestPluginLoadsThroughCPAPluginHost(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("CPA public loader exposes dlopen only on darwin/linux, not %s", runtime.GOOS)
	}
	pluginRoot := pluginRootFromTestFile(t)
	pluginDirectory := t.TempDir()
	pluginPath := filepath.Join(pluginDirectory, pluginLibraryName(runtime.GOOS))
	buildCommand := exec.Command("go", "build", "-buildmode=c-shared", "-o", pluginPath, "./cmd/aihub-auto-plugin")
	buildCommand.Dir = pluginRoot
	buildCommand.Env = append(os.Environ(), "CGO_ENABLED=1")
	if output, errBuild := buildCommand.CombinedOutput(); errBuild != nil {
		t.Fatalf("build native plugin: %v\n%s", errBuild, output)
	}

	enabled := true
	host := pluginhost.New()
	t.Cleanup(host.ShutdownAll)
	host.ApplyConfig(context.Background(), pluginhost.RuntimeConfig{
		Enabled: true,
		Dir:     pluginDirectory,
		Configs: map[string]pluginhost.PluginInstanceConfig{
			"aihub-auto": {Enabled: &enabled, Raw: yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "base_url"},
				{Kind: yaml.ScalarNode, Value: "http://127.0.0.1:1"},
			}}},
		},
	})
	registered := host.RegisteredPlugins()
	if len(registered) != 1 || registered[0].ID != "aihub-auto" {
		t.Fatalf("registered plugins = %#v, want loaded aihub-auto dynamic library", registered)
	}

	response, handled, errPick := host.PickAuth(context.Background(), pluginapi.SchedulerPickRequest{
		Model: "gpt-4",
		Options: pluginapi.SchedulerOptions{Metadata: map[string]any{
			"aihub_auto_account_id": "account-a",
			"aihub_auto_plan":       "pro",
		}},
		Candidates: []pluginapi.SchedulerAuthCandidate{{
			ID:       "account-a-auth",
			Provider: "aihub-auto",
			Attributes: map[string]string{
				"aihub_auto_account_id":      "account-a",
				"aihub_auto_plan":            "pro",
				"aihub_auto_group_id":        "1",
				"aihub_auto_rate_multiplier": "0.1",
				"aihub_auto_ttft_ms":         "100",
			},
		}},
	})
	if errPick != nil || !handled || !response.Handled || response.AuthID != "account-a-auth" {
		t.Fatalf("CPA host scheduler result = %#v, handled=%t, error=%v", response, handled, errPick)
	}

	// This reaches the loaded C shared library, which invokes CPA's native host
	// HTTP callback. The unavailable local endpoint makes CPA return its error
	// envelope; the plugin must propagate it rather than decode it as HTTP JSON.
	auth := host.AuthDataToCoreAuth(pluginapi.AuthData{
		Provider:    "aihub-auto",
		ID:          "account-a",
		StorageJSON: []byte(`{"access_token":"access-token"}`),
	}, "", "aihub-auto-account.json")
	modelResult := host.ModelsForAuth(context.Background(), auth)
	if !modelResult.Handled || modelResult.Err == nil {
		t.Fatalf("CPA host callback result = %#v, want propagated host HTTP failure", modelResult)
	}
}

func pluginRootFromTestFile(testingHandle *testing.T) string {
	testingHandle.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		testingHandle.Fatal("locate integration test source")
	}
	return filepath.Dir(filepath.Dir(currentFile))
}

func pluginLibraryName(goos string) string {
	switch goos {
	case "darwin":
		return "aihub-auto.dylib"
	case "linux":
		return "aihub-auto.so"
	default:
		return ""
	}
}
