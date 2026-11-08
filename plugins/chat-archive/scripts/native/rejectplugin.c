/* Test-only CPA plugin for chat-archive native acceptance (the case "rejected by a
 * higher-priority test plugin"). It declares only request_interceptor and terminates a
 * before-auth request with 403 when the hook request mentions the model "reject-fixture".
 * Every other call passes through. It holds no state, reads no files and opens no sockets.
 * Built by scripts/native/run-smoke.sh with: gcc -shared -fPIC -O2 -o reject-fixture.so rejectplugin.c
 */
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct { void *ptr; size_t len; } cliproxy_buffer;
typedef struct {
	uint32_t abi_version;
	void *host_ctx;
	int (*call)(void *host_ctx, const char *method, const uint8_t *req, size_t len, cliproxy_buffer *resp);
	void (*free_buffer)(void *ptr, size_t len);
} cliproxy_host_api;
typedef struct {
	uint32_t abi_version;
	int (*call)(const char *method, const uint8_t *req, size_t len, cliproxy_buffer *resp);
	void (*free_buffer)(void *ptr, size_t len);
	void (*shutdown)(void);
} cliproxy_plugin_api;

static const char REGISTER[] =
	"{\"ok\":true,\"result\":{\"schema_version\":7,\"metadata\":{\"Name\":\"Reject Fixture\","
	"\"Version\":\"0.0.1\",\"Author\":\"native-acceptance\",\"GitHubRepository\":\"https://example.invalid/reject-fixture\","
	"\"Logo\":\"\",\"ConfigFields\":[]},\"capabilities\":{\"request_interceptor\":true}}}";
/* ResponseBody is base64 of {"error":"rejected_by_fixture"}. */
static const char REJECT[] =
	"{\"ok\":true,\"result\":{\"Terminate\":true,\"StatusCode\":403,"
	"\"ResponseHeaders\":{\"Content-Type\":[\"application/json\"]},"
	"\"ResponseBody\":\"eyJlcnJvciI6InJlamVjdGVkX2J5X2ZpeHR1cmUifQ==\"}}";
static const char PASS[] = "{\"ok\":true,\"result\":{}}";
static const char ROUTES[] = "{\"ok\":true,\"result\":{\"routes\":[],\"resources\":[]}}";

static int reply(const char *text, cliproxy_buffer *resp) {
	size_t n = strlen(text);
	void *p = malloc(n);
	if (p == NULL) return 1;
	memcpy(p, text, n);
	resp->ptr = p;
	resp->len = n;
	return 0;
}

static int mentions(const uint8_t *req, size_t len, const char *needle) {
	size_t m = strlen(needle);
	if (req == NULL || len < m) return 0;
	for (size_t i = 0; i + m <= len; i++)
		if (memcmp(req + i, needle, m) == 0) return 1;
	return 0;
}

static int plugin_call(const char *method, const uint8_t *req, size_t len, cliproxy_buffer *resp) {
	if (method == NULL || resp == NULL) return 1;
	if (strcmp(method, "plugin.register") == 0 || strcmp(method, "plugin.reconfigure") == 0)
		return reply(REGISTER, resp);
	if (strcmp(method, "management.register") == 0) return reply(ROUTES, resp);
	if (strcmp(method, "request.intercept_before") == 0 && mentions(req, len, "\"reject-fixture\""))
		return reply(REJECT, resp);
	return reply(PASS, resp);
}

static void plugin_free(void *ptr, size_t len) {
	(void)len;
	free(ptr);
}

static void plugin_shutdown(void) {}

int cliproxy_plugin_init(const cliproxy_host_api *host, cliproxy_plugin_api *api) {
	if (host == NULL || api == NULL || host->abi_version != 1) return 1;
	api->abi_version = 1;
	api->call = plugin_call;
	api->free_buffer = plugin_free;
	api->shutdown = plugin_shutdown;
	return 0;
}
