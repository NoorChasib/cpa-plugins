// Native ABI adapted from token-usage at b7683a1.
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

// No host API pointer is retained: model lists are fetched from CPA over
// plain HTTP, with no host callbacks or auth access.
//
// Calls are admitted through a gate instead of a read lock. A plugin route
// that fetches from CPA makes CPA run its response interceptors, which call
// back into this library while the outer call is still in flight. With a
// RWMutex, that nested read lock would block behind a waiting Shutdown until
// the outer fetch timed out. The gate never blocks: once Shutdown has begun,
// new calls are refused at once (an interceptor call passes the response
// through), and Shutdown waits only for calls already admitted.
var (
	lifecycleMu  sync.Mutex // serializes init and shutdown
	gateMu       sync.Mutex
	stopping     bool
	inFlight     sync.WaitGroup
	globalPlugin nativePlugin
)

type nativePlugin interface {
	Handle(string, []byte) (any, error)
	Shutdown()
}

// interceptPassThrough keeps CPA's response unchanged. It is the answer to
// every response.intercept_after call this library does not rewrite,
// including refused, oversized, and failed ones, so CPA never logs an
// interceptor error for them.
var interceptPassThrough = []byte(`{"ok":true,"result":{}}`)

// admit returns the plugin and a release function, or false when the plugin
// is not initialized or is shutting down.
func admit() (nativePlugin, func(), bool) {
	gateMu.Lock()
	defer gateMu.Unlock()
	if stopping || globalPlugin == nil {
		return nil, nil, false
	}
	inFlight.Add(1)
	return globalPlugin, inFlight.Done, true
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || uint32(host.abi_version) != protocol.ABIVersion {
		return 1
	}
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	gateMu.Lock()
	defer gateMu.Unlock()
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
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	if method == nil || (request == nil && requestLen != 0) || uint64(requestLen) > 1<<40 {
		writeResponse(response, errorEnvelope("invalid_request", "invalid native request"))
		return 1
	}
	// The request is read in place from CPA's buffer, valid only during this
	// call: every decoder copies what it keeps, so a large response body that
	// is not a model list is never copied.
	var raw []byte
	if requestLen > 0 {
		raw = unsafe.Slice((*byte)(unsafe.Pointer(request)), int(requestLen))
	}
	result, ok := handleCall(C.GoString(method), raw)
	if !writeResponse(response, result) || !ok {
		return 1
	}
	return 0
}

// handleCall admits and dispatches one call. response.intercept_after always
// succeeds: anything it does not rewrite passes CPA's response through.
func handleCall(method string, raw []byte) ([]byte, bool) {
	intercept := method == protocol.MethodResponseInterceptAfter
	limit := protocol.MaxRequestBytes
	if intercept {
		limit = protocol.MaxInterceptRequestBytes
	}
	if len(raw) > limit {
		if intercept {
			return interceptPassThrough, true
		}
		return errorEnvelope("invalid_request", "invalid native request"), false
	}
	p, release, ok := admit()
	if !ok {
		if intercept {
			return interceptPassThrough, true
		}
		return errorEnvelope("not_initialized", "plugin is not initialized"), false
	}
	defer release()
	encoded, ok := dispatch(p, method, raw)
	if !ok && intercept {
		return interceptPassThrough, true
	}
	return encoded, ok
}

// dispatch runs one admitted call, including response encoding.
func dispatch(p nativePlugin, method string, raw []byte) (encoded []byte, ok bool) {
	defer func() {
		if recover() != nil {
			encoded, ok = errorEnvelope("plugin_panic", "plugin request failed"), false
		}
	}()
	result, err := p.Handle(method, raw)
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
func cliproxyPluginShutdown() { shutdownNative() }

// shutdownNative refuses new calls, waits for admitted ones (a catalog fetch
// is bounded by its own timeout), stops the plugin, and clears it so init
// works again in the same process.
func shutdownNative() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	gateMu.Lock()
	p := globalPlugin
	stopping = true
	gateMu.Unlock()
	inFlight.Wait()
	if p != nil {
		func() {
			defer func() { _ = recover() }()
			p.Shutdown()
		}()
	}
	gateMu.Lock()
	globalPlugin, stopping = nil, false
	gateMu.Unlock()
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
