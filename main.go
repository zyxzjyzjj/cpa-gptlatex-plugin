// Command prism builds a CLIProxyAPI (CPA) provider plugin that exposes the
// prism.openai.com ChatGPT web subscription as an OpenAI-compatible
// chat-completions provider.
//
// Build (Git Bash, no make required):
//
//	./build.sh
//
// or directly:
//
//	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o prism-provider.dll .
//
// then drop the artifact into CPA's plugins/ directory and enable it in
// config.yaml under plugins.configs.prism-provider.
//
// The C preamble below is a verbatim copy of CPA's own plugin examples
// (examples/plugin/*/go/main.go). The struct layouts and exported symbol names
// are an ABI contract with the host loader (internal/pluginhost/loader_windows.go)
// and must not be reshaped.
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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"unsafe"
)

const (
	providerName = "prism-provider"
	repoURL      = "https://github.com/zyxzjyzjj/cpa-gptlatex-plugin"
)

// version is a var, not a const, so the build scripts can inject it with
// -ldflags "-X main.version=...".
var version = "0.1.8"

// ABI and protocol versions, mirrored from CPA's sdk/pluginabi rather than
// imported so the plugin builds against no CLIProxyAPI release in particular.
//
// abiVersion must equal the host's pluginabi.ABIVersion or loader.Open refuses
// the library. schemaVersion must be <= the host's pluginabi.SchemaVersion:
// internal/pluginhost/rpc_client.go rejects any plugin that answers
// plugin.register with a higher value ("plugin schema version %d is not
// supported"). v7.2.50 reports 1, v7.3.4 reports 6 — so 1 is the only value
// accepted by every host, and it is the contract this plugin actually speaks.
const (
	abiVersion    uint32 = 1
	schemaVersion uint32 = 1
)

// RPC method names, mirrored from sdk/pluginabi.
const (
	methodPluginRegister    = "plugin.register"
	methodPluginReconfigure = "plugin.reconfigure"
	methodPluginShutdown    = "plugin.shutdown"

	methodModelStatic  = "model.static"
	methodModelForAuth = "model.for_auth"

	methodAuthIdentifier = "auth.identifier"
	methodAuthParse      = "auth.parse"
	methodAuthRefresh    = "auth.refresh"
	methodAuthLoginStart = "auth.login.start"
	methodAuthLoginPoll  = "auth.login.poll"

	methodExecutorIdentifier    = "executor.identifier"
	methodExecutorExecute       = "executor.execute"
	methodExecutorExecuteStream = "executor.execute_stream"
	methodExecutorCountTokens   = "executor.count_tokens"

	methodManagementRegister = "management.register"
	methodManagementHandle   = "management.handle"
	methodManagementResource = "management.resource"

	methodHostLog = "host.log"
)

var (
	cfgMu    sync.RWMutex
	cfgState = defaultConfig()
)

// ---------------------------------------------------------------- wire types
//
// These mirror the JSON contract the host decodes in
// internal/pluginhost/rpc_schema.go and sdk/pluginapi. Field names are the Go
// names on the host side: those structs carry no json tags, so they travel
// capitalised (e.g. "Payload", "StorageJSON"), except where noted.

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []configField
}

// configField mirrors pluginapi.ConfigField. Type must be one of the
// pluginapi.ConfigFieldType values ("string", "number", "integer", "boolean",
// "enum", "array", "object").
type configField struct {
	Name        string
	Type        string
	EnumValues  []string
	Description string
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type capabilities struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
	ManagementAPI         bool     `json:"management_api,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

// authParseRequest / authParseResponse drive credential import.
type authParseRequest struct {
	Provider string          `json:"Provider"`
	Path     string          `json:"Path"`
	FileName string          `json:"FileName"`
	RawJSON  []byte          `json:"RawJSON"`
	Host     json.RawMessage `json:"Host"`
}

type authData struct {
	Provider    string            `json:"Provider"`
	ID          string            `json:"ID"`
	FileName    string            `json:"FileName,omitempty"`
	Label       string            `json:"Label,omitempty"`
	Prefix      string            `json:"Prefix,omitempty"`
	Disabled    bool              `json:"Disabled,omitempty"`
	StorageJSON []byte            `json:"StorageJSON,omitempty"`
	Metadata    map[string]any    `json:"Metadata,omitempty"`
	Attributes  map[string]string `json:"Attributes,omitempty"`
	// NextRefreshAfter is a time.Time on the host side; time.Time unmarshals
	// RFC 3339, so the string form is accepted.
	NextRefreshAfter string `json:"NextRefreshAfter,omitempty"`
}

type authParseResponse struct {
	Handled bool       `json:"Handled"`
	Auth    *authData  `json:"Auth,omitempty"`
	Auths   []authData `json:"Auths,omitempty"`
}

type authRefreshRequest struct {
	AuthID       string            `json:"AuthID"`
	AuthProvider string            `json:"AuthProvider"`
	StorageJSON  []byte            `json:"StorageJSON"`
	Metadata     map[string]any    `json:"Metadata"`
	Attributes   map[string]string `json:"Attributes"`
}

type authRefreshResponse struct {
	Auth             authData `json:"Auth"`
	NextRefreshAfter string   `json:"NextRefreshAfter,omitempty"`
}

type staticModelRequest struct {
	Plugin metadata        `json:"Plugin"`
	Host   json.RawMessage `json:"Host"`
}

type authModelRequest struct {
	Plugin       metadata          `json:"Plugin"`
	AuthID       string            `json:"AuthID"`
	AuthProvider string            `json:"AuthProvider"`
	StorageJSON  []byte            `json:"StorageJSON"`
	Metadata     map[string]any    `json:"Metadata"`
	Attributes   map[string]string `json:"Attributes"`
	Host         json.RawMessage   `json:"Host"`
}

type modelInfo struct {
	ID               string `json:"ID"`
	Object           string `json:"Object,omitempty"`
	OwnedBy          string `json:"OwnedBy,omitempty"`
	Type             string `json:"Type,omitempty"`
	DisplayName      string `json:"DisplayName,omitempty"`
	Description      string `json:"Description,omitempty"`
	ContextLength    int64  `json:"ContextLength,omitempty"`
	InputTokenLimit  int64  `json:"InputTokenLimit,omitempty"`
	OutputTokenLimit int64  `json:"OutputTokenLimit,omitempty"`
	UserDefined      bool   `json:"UserDefined,omitempty"`
}

type modelResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

// executorRequest mirrors rpcExecutorRequest, which is pluginapi.ExecutorRequest
// plus the two callback fields. Capitalised keys throughout.
type executorRequest struct {
	AuthID          string              `json:"AuthID"`
	AuthProvider    string              `json:"AuthProvider"`
	Model           string              `json:"Model"`
	Format          string              `json:"Format"`
	Stream          bool                `json:"Stream"`
	Alt             string              `json:"Alt"`
	Headers         http.Header         `json:"Headers"`
	Query           map[string][]string `json:"Query"`
	OriginalRequest []byte              `json:"OriginalRequest"`
	SourceFormat    string              `json:"SourceFormat"`
	Payload         []byte              `json:"Payload"`
	Metadata        map[string]any      `json:"Metadata"`
	StorageJSON     []byte              `json:"StorageJSON"`
	AuthMetadata    map[string]any      `json:"AuthMetadata"`
	AuthAttributes  map[string]string   `json:"AuthAttributes"`
	StreamID        string              `json:"stream_id,omitempty"`
	HostCallbackID  string              `json:"host_callback_id,omitempty"`
}

// executorResponse mirrors pluginapi.ExecutorResponse.
type executorResponse struct {
	Payload  []byte         `json:"Payload,omitempty"`
	Headers  http.Header    `json:"Headers,omitempty"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

// streamResponse mirrors rpcExecutorStreamResponse: the whole stream is handed
// over as a finished slice of chunks, which the host replays to the client.
type streamResponse struct {
	Headers http.Header   `json:"headers,omitempty"`
	Chunks  []streamChunk `json:"chunks,omitempty"`
}

// streamChunk mirrors pluginapi.ExecutorStreamChunk. Err is host-to-plugin
// only and is never produced here.
type streamChunk struct {
	Payload []byte `json:"Payload,omitempty"`
}

// countTokensBody is the decoder the host applies to executor.count_tokens
// Payload. Shape taken from CPA's own reference executor plugin.
type countTokensBody struct {
	TotalTokens int64 `json:"total_tokens"`
}

// ------------------------------------------------------------------ C ABI

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	if host != nil {
		C.store_host_api(host)
	}
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 0))
		return 1
	}

	var raw []byte
	if request != nil && requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	out, err := handleMethod(C.GoString(method), raw)
	if err != nil {
		// The host accepts a non-zero return code as long as the body is a
		// well-formed error envelope (loader_windows.go: isPluginErrorEnvelope).
		hostLog("error", "method failed: "+err.Error(), map[string]any{"method": C.GoString(method)})
		writeResponse(response, errorEnvelope("internal_error", err.Error(), http.StatusInternalServerError))
		return 1
	}
	writeResponse(response, out)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	// CBytes allocates with malloc, which is what cliproxyPluginFree releases.
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// callHost invokes a host callback (loading/calling convention identical to
// CPA's reference plugins). The response buffer is owned by the host and must
// be handed back through its own free function, not C.free.
func callHost(method string, payload []byte) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}
	if C.call_host_api(cMethod, req, C.size_t(len(payload)), &response) == 0 && response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
}

// hostLog reports through the host's logger. Failure is not fatal: logging must
// never take a request down.
func hostLog(level, message string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin"] = providerName
	payload, err := json.Marshal(map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	})
	if err != nil {
		return
	}
	callHost(methodHostLog, payload)
}

// -------------------------------------------------------------- dispatching

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(reg())
	case methodPluginShutdown:
		return okEnvelope(map[string]any{})
	case methodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case methodAuthParse:
		return handleParseAuth(request)
	case methodAuthRefresh:
		return handleRefreshAuth(request)
	case methodAuthLoginStart:
		return handleLoginStart(context.Background())
	case methodAuthLoginPoll:
		return handleLoginPoll(request)
	case methodModelStatic:
		return handleModelStatic(request)
	case methodModelForAuth:
		return handleModelForAuth(request)
	case methodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case methodExecutorExecute:
		return handleExecute(request, false)
	case methodExecutorExecuteStream:
		return handleExecute(request, true)
	case methodExecutorCountTokens:
		return handleCountTokens(request)
	case methodManagementRegister:
		return handleManagementRegister()
	case methodManagementHandle:
		return handleManagementHandle(request)
	case methodManagementResource:
		return handlePanel(request)
	default:
		// Unknown methods are reported as a failed call rather than a Go error
		// so the host surfaces a clean envelope instead of a transport fault.
		return errorEnvelope("unknown_method", fmt.Sprintf("unsupported method: %s", method), http.StatusBadRequest), nil
	}
}

func reg() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: metadata{
			Name:             providerName,
			Version:          version,
			Author:           "zjj",
			GitHubRepository: repoURL,
			Logo:             "",
			ConfigFields: []configField{
				{Name: "cookies", Type: "string", Description: "prism.openai.com 的 Cookie 请求头原文（必填）"},
				{Name: "user_id", Type: "string", Description: "OpenAI user id，形如 user-xxxx；留空则自动从 /auth/session 探测"},
				{Name: "project_uuid", Type: "string", Description: "复用的项目 UUID；留空则首次调用时自动创建"},
				{Name: "project_title", Type: "string", Description: "自动创建项目时的标题"},
				{Name: "models", Type: "array", Description: "可用模型，单行 JSON 数组"},
				{Name: "default_model", Type: "string", Description: "客户端未指定模型时使用的模型"},
				{Name: "reasoning_effort", Type: "enum", EnumValues: []string{"low", "medium", "high", "xhigh"}, Description: "思考档位"},
				{Name: "sandbox", Type: "boolean", Description: "领取 LaTeX 沙箱并完成工作区同步（默认开；关掉则服务端只会返回 sandbox_reconnecting，拿不到答案）"},
				{Name: "system_prompt", Type: "string", Description: "覆盖默认 system prompt"},
			},
		},
		Capabilities: capabilities{
			ModelProvider: true,
			AuthProvider:  true,
			Executor:      true,
			// prism 的模型只能挂在导入的 cookie 凭据上（model.for_auth）。
			ExecutorModelScope:    "oauth",
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
			// Exposes the plugin's own sign-in completion endpoint. The host's
			// own callback box cannot be used because prism's OAuth state (~484
			// chars) is far longer than the host's 128-character limit, so it
			// rejects the callback with "invalid state" before looking for us.
			ManagementAPI: true,
		},
	}
}

// ------------------------------------------------------------- envelope I/O

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{
		OK:    false,
		Error: &envelopeError{Code: code, Message: message, HTTPStatus: httpStatus},
	})
	return raw
}
