// Native ABI adapted from token-usage at 4286578.
// Copyright (c) 2026 NoorChasib. Distributed under the MIT license.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"sync"
	"unsafe"

	pluginimpl "github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

// No host API pointer is retained: the catalog is fetched from CPA over plain
// HTTP, with no host callbacks or auth access. The read lock spans each
// admitted Go call, and Shutdown takes the write lock so it drains calls
// already in flight; a catalog fetch is bounded by its own timeout.
var (
	globalMu     sync.RWMutex
	globalPlugin nativePlugin
)

type nativePlugin interface {
	Handle(string, []byte) (any, error)
	Shutdown()
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || uint32(host.abi_version) != protocol.ABIVersion {
		return 1
	}
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalPlugin != nil {
		return 1
	}
	globalPlugin = pluginimpl.New()
	api.abi_version = C.uint32_t(protocol.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	globalMu.RLock()
	defer globalMu.RUnlock()
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	if method == nil || uint64(requestLen) > protocol.MaxRequestBytes || (request == nil && requestLen != 0) {
		writeResponse(response, errorEnvelope("invalid_request", "invalid native request"))
		return 1
	}
	var raw []byte
	if requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, ok := dispatch(C.GoString(method), raw)
	if !writeResponse(response, result) || !ok {
		return 1
	}
	return 0
}

// dispatch runs under globalMu.RLock, including response encoding in its caller.
func dispatch(method string, raw []byte) (encoded []byte, ok bool) {
	defer func() {
		if recover() != nil {
			encoded, ok = errorEnvelope("plugin_panic", "plugin request failed"), false
		}
	}()
	if globalPlugin == nil {
		return errorEnvelope("not_initialized", "plugin is not initialized"), false
	}
	result, err := globalPlugin.Handle(method, raw)
	if err != nil {
		// Never echo an arbitrary decoder/config error: it can contain
		// untrusted wire values or request headers.
		return errorEnvelope("plugin_error", "plugin request rejected"), false
	}
	encoded, err = okEnvelope(result)
	if err != nil {
		return errorEnvelope("encoding_error", "plugin response encoding failed"), false
	}
	return encoded, true
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalPlugin != nil {
		globalPlugin.Shutdown()
		globalPlugin = nil
	}
}

func okEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(protocol.Envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(protocol.Envelope{Error: &protocol.EnvelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) bool {
	if response == nil || len(raw) == 0 {
		return false
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return false
	}
	response.ptr, response.len = ptr, C.size_t(len(raw))
	return true
}
