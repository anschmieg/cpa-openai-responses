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

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

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
	"encoding/json"
	"errors"
	"unsafe"
)

var running = plugin{call: callHost}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, out *C.cliproxy_plugin_api) C.int {
	if host == nil || host.abi_version != 1 || out == nil {
		return 1
	}
	C.store_host_api(host)
	out.abi_version = 1
	out.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	out.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	out.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, size C.size_t, response *C.cliproxy_buffer) C.int {
	if method == nil || response == nil || size > 1<<31-1 {
		return 1
	}
	response.ptr, response.len = nil, 0
	result, err := running.dispatch(C.GoString(method), C.GoBytes(unsafe.Pointer(request), C.int(size)))
	reply := envelope{OK: err == nil}
	if err != nil {
		if !errors.As(err, &reply.Error) {
			reply.Error = &rpcError{Code: "plugin_error", Message: err.Error()}
		}
	} else {
		reply.Result, err = json.Marshal(result)
		if err != nil {
			return 1
		}
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return 1
	}
	response.ptr, response.len = C.CBytes(raw), C.size_t(len(raw))
	// Even error envelopes are successful ABI calls. The host decodes status and code.
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	C.free(ptr)
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { running.shutdown() }

func callHost(method string, data []byte) ([]byte, error) {
	name := C.CString(method)
	input := C.CBytes(data)
	defer C.free(unsafe.Pointer(name))
	defer C.free(input)
	var out C.cliproxy_buffer
	status := C.call_host_api(name, (*C.uint8_t)(input), C.size_t(len(data)), &out)
	if out.ptr != nil {
		defer C.free_host_buffer(out.ptr, out.len)
	}
	if status != 0 || out.ptr == nil || out.len > 1<<31-1 {
		return nil, errors.New("host callback failed")
	}
	var reply envelope
	if err := json.Unmarshal(C.GoBytes(out.ptr, C.int(out.len)), &reply); err != nil {
		return nil, err
	}
	if !reply.OK {
		return nil, errors.New("host callback failed")
	}
	return reply.Result, nil
}
