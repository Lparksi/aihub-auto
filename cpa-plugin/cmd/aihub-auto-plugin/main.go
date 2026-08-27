package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

static const cliproxy_host_api* cliproxyHostAPI = NULL;

static void cliproxySetHostAPI(const cliproxy_host_api* host) {
	cliproxyHostAPI = host;
}

// cgo-generated export declarations cannot preserve const qualifiers. Keep
// those qualifiers on the CPA-facing function pointer with this adapter.
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static int cliproxyPluginCallAdapter(const char* method, const uint8_t* request, size_t requestLength, cliproxy_buffer* response) {
	return cliproxyPluginCall((char*)method, (uint8_t*)request, requestLength, response);
}

static void cliproxySetPluginAPI(cliproxy_plugin_api* plugin) {
	plugin->call = cliproxyPluginCallAdapter;
	plugin->free_buffer = cliproxyPluginFree;
	plugin->shutdown = cliproxyPluginShutdown;
}

static int cliproxyHostAPIIsValid(const cliproxy_host_api* host, uint32_t expectedABIVersion) {
	return host != NULL && host->abi_version == expectedABIVersion && host->call != NULL && host->free_buffer != NULL;
}

static int cliproxyCallHost(const char* method, const uint8_t* request, size_t requestLength, cliproxy_buffer* response) {
	if (response != NULL) { response->ptr = NULL; response->len = 0; }
	if (cliproxyHostAPI == NULL || cliproxyHostAPI->call == NULL) { return 1; }
	return cliproxyHostAPI->call(cliproxyHostAPI->host_ctx, method, request, requestLength, response);
}

static void cliproxyFreeHostResponse(void* pointer, size_t length) {
	if (cliproxyHostAPI != NULL && cliproxyHostAPI->free_buffer != NULL && pointer != NULL) {
		cliproxyHostAPI->free_buffer(pointer, length);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unsafe"

	"github.com/mimmer/aihub-auto/cpa-plugin/internal/abi"
	"github.com/mimmer/aihub-auto/cpa-plugin/internal/configuration"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var dispatcher = abi.NewDispatcher()

const maxGoInt = int(^uint(0) >> 1)
const maxCGoBytesLength = int(^uint32(0) >> 1)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil || C.cliproxyHostAPIIsValid(host, C.uint32_t(pluginabi.ABIVersion)) == 0 {
		return 1
	}
	C.cliproxySetHostAPI(host)
	dispatcher.SetHostHTTPClient(hostHTTPClient{})
	dispatcher.SetAuthSaver(saveHostAuth)
	dispatcher.SetStreamEmitter(emitHostStream, closeHostStream)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	C.cliproxySetPluginAPI(plugin)
	return 0
}

// hostHTTPClient is the native ABI adapter to CPA's mandatory host transport.
type hostHTTPClient struct{}

func (hostHTTPClient) Do(contextValue context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	rawResponse, errCall := callHost(contextValue, pluginabi.MethodHostHTTPDo, hostHTTPRequest{HTTPRequest: request, HostCallbackID: abi.HostCallbackID(contextValue)})
	if errCall != nil {
		return pluginapi.HTTPResponse{}, errCall
	}
	var response pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(rawResponse, &response); errUnmarshal != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host HTTP response: %w", errUnmarshal)
	}
	return response, nil
}

func (hostHTTPClient) DoStream(contextValue context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	rawResponse, errCall := callHost(contextValue, pluginabi.MethodHostHTTPDoStream, hostHTTPRequest{HTTPRequest: request, HostCallbackID: abi.HostCallbackID(contextValue)})
	if errCall != nil {
		return pluginapi.HTTPStreamResponse{}, errCall
	}
	var response struct {
		StatusCode int         `json:"status_code"`
		Headers    http.Header `json:"headers"`
		StreamID   string      `json:"stream_id"`
	}
	if errUnmarshal := json.Unmarshal(rawResponse, &response); errUnmarshal != nil {
		return pluginapi.HTTPStreamResponse{}, fmt.Errorf("decode host HTTP stream response: %w", errUnmarshal)
	}
	if response.StreamID == "" {
		return pluginapi.HTTPStreamResponse{}, errors.New("host HTTP stream identifier is missing")
	}
	chunks := make(chan pluginapi.HTTPStreamChunk)
	go readHostStream(contextValue, response.StreamID, chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: response.StatusCode, Headers: response.Headers, Chunks: chunks}, nil
}

// hostHTTPRequest mirrors CPA's rpcHostHTTPRequest wrapper. CPA resolves the
// callback context from this field before enforcing its HTTP policy.
type hostHTTPRequest struct {
	pluginapi.HTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func readHostStream(contextValue context.Context, streamID string, chunks chan<- pluginapi.HTTPStreamChunk) {
	defer close(chunks)
	defer func() {
		_, _ = callHost(contextValue, pluginabi.MethodHostHTTPStreamClose, struct {
			StreamID string `json:"stream_id"`
		}{StreamID: streamID})
	}()
	for {
		var response struct {
			Payload []byte `json:"payload"`
			Error   string `json:"error"`
			Done    bool   `json:"done"`
		}
		rawResponse, errCall := callHost(contextValue, pluginabi.MethodHostHTTPStreamRead, struct {
			StreamID string `json:"stream_id"`
		}{StreamID: streamID})
		if errCall != nil {
			chunks <- pluginapi.HTTPStreamChunk{Err: errCall}
			return
		}
		if errUnmarshal := json.Unmarshal(rawResponse, &response); errUnmarshal != nil {
			chunks <- pluginapi.HTTPStreamChunk{Err: errUnmarshal}
			return
		}
		if response.Error != "" {
			chunks <- pluginapi.HTTPStreamChunk{Err: errors.New("host HTTP stream read failed")}
			return
		}
		if len(response.Payload) > 0 {
			chunks <- pluginapi.HTTPStreamChunk{Payload: response.Payload}
		}
		if response.Done {
			return
		}
	}
}

func saveHostAuth(contextValue context.Context, name string, authJSON []byte) error {
	_, errCall := callHost(contextValue, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: authJSON})
	return errCall
}

// emitHostStream pushes one executor stream chunk to CPA's host stream bridge.
func emitHostStream(contextValue context.Context, streamID string, payload []byte, errorMessage string) error {
	_, errCall := callHost(contextValue, pluginabi.MethodHostStreamEmit, struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload,omitempty"`
		Error    string `json:"error,omitempty"`
	}{StreamID: streamID, Payload: payload, Error: errorMessage})
	return errCall
}

// closeHostStream terminates CPA's host stream bridge for one executor stream.
func closeHostStream(contextValue context.Context, streamID string, errorMessage string) error {
	_, errCall := callHost(contextValue, pluginabi.MethodHostStreamClose, struct {
		StreamID string `json:"stream_id"`
		Error    string `json:"error,omitempty"`
	}{StreamID: streamID, Error: errorMessage})
	return errCall
}

func callHost(contextValue context.Context, method string, request any) ([]byte, error) {
	if callbackID := abi.HostCallbackID(contextValue); callbackID != "" {
		switch typedRequest := request.(type) {
		case pluginapi.HostAuthSaveRequest:
			request = struct {
				pluginapi.HostAuthSaveRequest
				HostCallbackID string `json:"host_callback_id,omitempty"`
			}{HostAuthSaveRequest: typedRequest, HostCallbackID: callbackID}
		}
	}
	rawRequest, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return nil, errMarshal
	}
	methodValue := C.CString(method)
	defer C.free(unsafe.Pointer(methodValue))
	var response C.cliproxy_buffer
	result := C.cliproxyCallHost(methodValue, (*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(rawRequest))), C.size_t(len(rawRequest)), &response)
	if result != 0 {
		return nil, errors.New("CPA host callback failed")
	}
	defer C.cliproxyFreeHostResponse(response.ptr, response.len)
	if response.ptr == nil {
		return nil, errors.New("CPA host callback returned no response")
	}
	rawResponse := C.GoBytes(response.ptr, C.int(response.len))
	return abi.DecodeHostEnvelope(rawResponse)
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLength C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, abi.ErrorEnvelope("invalid_method", "method is required", false, 0))
		return 1
	}
	if requestLength > C.size_t(maxGoInt) || requestLength > C.size_t(maxCGoBytesLength) || requestLength > C.size_t(configuration.DefaultMaxRequestBytes) {
		writeResponse(response, abi.ErrorEnvelope("invalid_request", "request length exceeds supported maximum", false, 0))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLength > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLength))
	}
	writeResponse(response, dispatcher.Handle(C.GoString(method), requestBytes))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(pointer unsafe.Pointer, _ C.size_t) {
	if pointer != nil {
		C.free(pointer)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	_ = dispatcher.Handle(pluginabi.MethodPluginShutdown, nil)
}

func writeResponse(response *C.cliproxy_buffer, rawResponse []byte) {
	if response == nil || len(rawResponse) == 0 {
		return
	}
	pointer := C.CBytes(rawResponse)
	if pointer == nil {
		return
	}
	response.ptr = pointer
	response.len = C.size_t(len(rawResponse))
}
