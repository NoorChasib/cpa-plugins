// Native ABI adapted from token-usage (recover-wrapped dispatch, RW lock, sanitized errors) and
// quota-cache (host API pointer behind a host-call gate). Never bridges host.auth.get.
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

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) { stored_host = host; }
static void clear_host_api(void) { stored_host = NULL; }

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	pluginimpl "github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
)

// nativePlugin is what the ABI dispatches to.
type nativePlugin interface {
	Hook(method string, raw []byte) []byte
	HookPanicked(method, typ string)
	Handle(method string, raw []byte) (any, error)
	Shutdown()
}

// Calls hold globalMu.RLock for their duration; shutdown stops the plugin first (hooks pass
// through by admission state meanwhile), then takes the write lock only to drain in-flight calls
// and clear the instance.
var (
	nativeLifecycleMu  sync.Mutex
	globalMu           sync.RWMutex
	globalPlugin       nativePlugin
	hostCalls          hostCallGate
	clearStoredHostAPI = func() { C.clear_host_api() }
	newPlugin          = func() nativePlugin { return pluginimpl.New(hostBridge{}) }
)

type hostCallGate struct {
	mu       sync.Mutex
	stopping bool
	calls    sync.WaitGroup
}

func (g *hostCallGate) reset() {
	g.mu.Lock()
	g.stopping = false
	g.mu.Unlock()
}

func (g *hostCallGate) begin() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return nil, false
	}
	g.calls.Add(1)
	var once sync.Once
	return func() { once.Do(g.calls.Done) }, true
}

func (g *hostCallGate) stopAccepting() {
	g.mu.Lock()
	g.stopping = true
	g.mu.Unlock()
}

func (g *hostCallGate) wait() { g.calls.Wait() }

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	nativeLifecycleMu.Lock()
	defer nativeLifecycleMu.Unlock()
	if host == nil || api == nil || uint32(host.abi_version) != protocol.ABIVersion {
		return 1
	}
	globalMu.RLock()
	initialized := globalPlugin != nil
	globalMu.RUnlock()
	if initialized {
		return 1
	}
	C.store_host_api(host)
	hostCalls.reset()
	api.abi_version = C.uint32_t(protocol.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	globalMu.Lock()
	globalPlugin = newPlugin()
	globalMu.Unlock()
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
	// Decode in place from the C buffer, valid only during this call: the raw RPC never enters
	// the Go heap. Decoders copy every value they keep.
	var raw []byte
	if requestLen > 0 {
		raw = unsafe.Slice((*byte)(unsafe.Pointer(request)), int(requestLen))
	}
	out, rc := handleCall(C.GoString(method), raw)
	if !writeResponse(response, out) {
		return 1
	}
	return C.int(rc)
}

// handleCall dispatches one RPC. Hooks always answer the pre-encoded OK with rc 0, even when the
// handler panics; only lifecycle (config errors), management and unknown methods can fail.
func handleCall(method string, raw []byte) ([]byte, int) {
	globalMu.RLock()
	defer globalMu.RUnlock()
	p := globalPlugin
	if p == nil {
		if pluginimpl.IsHook(method) {
			return hooks.OK, 0
		}
		return errorEnvelope("not_initialized", "plugin is not initialized"), 1
	}
	if pluginimpl.IsHook(method) {
		return hookGuarded(p, method, raw), 0
	}
	switch method {
	case protocol.MethodPluginRegister, protocol.MethodPluginReconfigure:
		if len(raw) > protocol.MaxLifecycle {
			return errorEnvelope("invalid_request", "request too large"), 1
		}
	case protocol.MethodManagementHandle:
		if len(raw) > protocol.MaxSmallHook {
			b, _ := okEnvelope(protocol.ManagementResponse{StatusCode: 413, Body: []byte(`{"error":"too_large"}`)})
			return b, 0
		}
	default:
		if len(raw) > protocol.MaxOtherMethod {
			return errorEnvelope("invalid_request", "request too large"), 1
		}
	}
	result, err := handleGuarded(p, method, raw)
	if err != nil {
		return callErrorEnvelope(method, err), 1
	}
	encoded, err := okEnvelope(result)
	if err != nil {
		return errorEnvelope("encoding_error", "plugin response encoding failed"), 1
	}
	return encoded, 0
}

func hookGuarded(p nativePlugin, method string, raw []byte) (out []byte) {
	defer func() {
		if v := recover(); v != nil {
			out = hooks.OK
			func() {
				defer func() { _ = recover() }()
				p.HookPanicked(method, fmt.Sprintf("%T", v))
			}()
		}
	}()
	return p.Hook(method, raw)
}

var errPluginPanic = errors.New("plugin request failed")

func handleGuarded(p nativePlugin, method string, raw []byte) (result any, err error) {
	defer func() {
		if v := recover(); v != nil {
			result, err = nil, errPluginPanic
			func() {
				defer func() { _ = recover() }()
				p.HookPanicked(method, fmt.Sprintf("%T", v))
			}()
		}
	}()
	return p.Handle(method, raw)
}

// callErrorEnvelope never echoes arbitrary error text. Lifecycle errors are the plugin's own
// constant messages naming a config key (never its value), so they are passed on.
func callErrorEnvelope(method string, err error) []byte {
	switch {
	case errors.Is(err, errPluginPanic):
		return errorEnvelope("plugin_panic", "plugin request failed")
	case method == protocol.MethodPluginRegister || method == protocol.MethodPluginReconfigure:
		return errorEnvelope("config_error", err.Error())
	}
	return errorEnvelope("plugin_error", "plugin request rejected")
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { shutdownNative() }

// shutdownNative: stop accepting host calls, stop the plugin (bounded), wait for in-flight host
// calls, drain in-flight RPCs and clear the instance, then clear the host pointer. Init works
// again in the same process afterwards.
func shutdownNative() {
	nativeLifecycleMu.Lock()
	defer nativeLifecycleMu.Unlock()
	globalMu.RLock()
	p := globalPlugin
	globalMu.RUnlock()
	hostCalls.stopAccepting()
	if p != nil {
		func() {
			defer func() { _ = recover() }()
			p.Shutdown()
		}()
	}
	hostCalls.wait()
	globalMu.Lock()
	globalPlugin = nil
	globalMu.Unlock()
	clearStoredHostAPI()
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

// hostBridge implements labels.Host through the gate. host.auth.get is never bridged.
type hostBridge struct{}

func (hostBridge) ListAuth(ctx context.Context) ([]protocol.HostAuthFileEntry, error) {
	raw, err := callHost(ctx, protocol.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var r protocol.HostAuthListResponse
	if json.Unmarshal(raw, &r) != nil {
		return nil, errors.New("decode host.auth.list response")
	}
	return r.Files, nil
}

func (hostBridge) GetRuntime(ctx context.Context, authIndex string) (protocol.HostAuthFileEntry, error) {
	raw, err := callHost(ctx, protocol.MethodHostAuthGetRuntime, protocol.HostAuthGetRuntimeRequest{AuthIndex: authIndex})
	if err != nil {
		return protocol.HostAuthFileEntry{}, err
	}
	var r protocol.HostAuthGetRuntimeResponse
	if json.Unmarshal(raw, &r) != nil {
		return protocol.HostAuthFileEntry{}, errors.New("decode host.auth.get_runtime response")
	}
	return r.Auth, nil
}

func callHost(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	finish, ok := hostCalls.begin()
	if !ok {
		return nil, errors.New("host callbacks are shutting down")
	}
	defer finish()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("encode host callback request")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var resp C.cliproxy_buffer
	var reqPtr *C.uint8_t
	if len(body) > 0 {
		mem := C.CBytes(body)
		if mem == nil {
			return nil, errors.New("allocate host callback request")
		}
		defer C.free(mem)
		reqPtr = (*C.uint8_t)(mem)
	}
	code := C.call_host_api(cMethod, reqPtr, C.size_t(len(body)), &resp)
	if resp.len > protocol.MaxHostResponse {
		C.free_host_buffer(resp.ptr, resp.len)
		return nil, errors.New("host response too large")
	}
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil {
		C.free_host_buffer(resp.ptr, resp.len)
	}
	if code != 0 || len(out) == 0 {
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	var env protocol.Envelope
	if json.Unmarshal(out, &env) != nil || !env.OK {
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	return env.Result, nil
}
