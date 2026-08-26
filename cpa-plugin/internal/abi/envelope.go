package abi

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// DecodeHostEnvelope extracts a successful CPA host-callback result. Host
// callbacks use the same envelope format as plugin RPC calls, so a callback
// error must never be treated as a successful raw payload.
func DecodeHostEnvelope(rawResponse []byte) ([]byte, error) {
	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &envelope); errUnmarshal != nil {
		return nil, fmt.Errorf("decode CPA host callback envelope: %w", errUnmarshal)
	}
	if envelope.OK {
		return envelope.Result, nil
	}
	if envelope.Error == nil {
		return nil, errors.New("CPA host callback failed without an error payload")
	}
	callbackError := envelope.Error
	message := callbackError.Message
	if message == "" {
		message = "CPA host callback failed"
	}
	if callbackError.Code == "" {
		return nil, errors.New(message)
	}
	return nil, fmt.Errorf("CPA host callback %s: %s", callbackError.Code, message)
}

// SuccessEnvelope serializes a successful CPA RPC result.
func SuccessEnvelope(result any) []byte {
	rawResult, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return ErrorEnvelope("encoding_error", errMarshal.Error(), false, 0)
	}
	rawEnvelope, errMarshal := json.Marshal(pluginabi.Envelope{OK: true, Result: rawResult})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"encoding_error","message":"unable to encode response"}}`)
	}
	return rawEnvelope
}

// ErrorEnvelope serializes a typed CPA RPC error.
func ErrorEnvelope(code, message string, retryable bool, httpStatus int) []byte {
	rawEnvelope, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{
		Code:       code,
		Message:    message,
		Retryable:  retryable,
		HTTPStatus: httpStatus,
	}})
	return rawEnvelope
}
