package abi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestDecodeHostEnvelopeReturnsResultOnlyForSuccessfulCallback(t *testing.T) {
	rawResponse := SuccessEnvelope(map[string]string{"saved": "credential.json"})
	result, errDecode := DecodeHostEnvelope(rawResponse)
	if errDecode != nil || string(result) != `{"saved":"credential.json"}` {
		t.Fatalf("DecodeHostEnvelope() = %s, %v; want callback result", result, errDecode)
	}
}

func TestDecodeHostEnvelopePropagatesCallbackError(t *testing.T) {
	rawResponse := ErrorEnvelope("auth_save_denied", "host refused credential storage", false, 403)
	_, errDecode := DecodeHostEnvelope(rawResponse)
	if errDecode == nil || !strings.Contains(errDecode.Error(), "auth_save_denied") || !strings.Contains(errDecode.Error(), "host refused credential storage") {
		t.Fatalf("DecodeHostEnvelope() error = %v, want callback envelope error", errDecode)
	}

	_, errDecode = DecodeHostEnvelope([]byte(`{"ok":false,"error":null}`))
	if errDecode == nil || !strings.Contains(errDecode.Error(), "without an error payload") {
		t.Fatalf("DecodeHostEnvelope() missing error = %v, want explicit failure", errDecode)
	}

	_, errDecode = DecodeHostEnvelope([]byte(`not JSON`))
	if errDecode == nil {
		t.Fatal("DecodeHostEnvelope() accepted malformed callback response")
	}
}

func TestDecodeHostEnvelopeRecognizesNativeHostErrorEnvelope(t *testing.T) {
	rawResponse, errMarshal := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: "host_call_failed", Message: "simulated C host response"}})
	if errMarshal != nil {
		t.Fatalf("marshal simulated C host envelope: %v", errMarshal)
	}
	_, errDecode := DecodeHostEnvelope(rawResponse)
	if errDecode == nil || !strings.Contains(errDecode.Error(), "simulated C host response") {
		t.Fatalf("DecodeHostEnvelope() error = %v, want simulated C host error", errDecode)
	}
}
