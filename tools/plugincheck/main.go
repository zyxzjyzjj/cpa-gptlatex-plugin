// Command plugincheck loads the built prism-provider shared library the same way
// CLIProxyAPI's plugin host does and exercises every RPC method it implements.
//
// This is the closest thing to an end-to-end test that does not need a CPA build
// or live prism.openai.com cookies: it proves the C ABI layout, the JSON envelope
// protocol, config parsing, and credential/model handling all line up with the
// host. The loader logic mirrors internal/pluginhost/loader_windows.go
// (struct layouts, return-code handling, buffer ownership); only calls that
// reach prism.openai.com are impossible here.
//
// Usage: plugincheck [path/to/prism-provider.dll]
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// ------------------------------------------------------- host ABI (mirrors CPA)

type cliproxyBuffer struct {
	ptr uintptr
	len uintptr
}

type cliproxyHostAPI struct {
	abiVersion uint32
	hostCtx    uintptr
	call       uintptr
	freeBuffer uintptr
}

type cliproxyPluginAPI struct {
	abiVersion uint32
	call       uintptr
	freeBuffer uintptr
	shutdown   uintptr
}

const hostABIVersion = 1

// validConfigFieldTypes mirrors pluginapi.ConfigFieldType.
var validConfigFieldTypes = map[string]bool{
	"string": true, "number": true, "integer": true, "boolean": true,
	"enum": true, "array": true, "object": true,
}

// ---------------------------------------------------------------- test harness

type check struct {
	passed  int
	failed  int
	hostLog []string
}

func (c *check) ok(name string) {
	c.passed++
	fmt.Printf("  PASS  %s\n", name)
}

func (c *check) fail(name, format string, args ...any) {
	c.failed++
	fmt.Printf("  FAIL  %s\n        %s\n", name, fmt.Sprintf(format, args...))
}

func (c *check) assert(name string, cond bool, format string, args ...any) {
	if cond {
		c.ok(name)
		return
	}
	c.fail(name, format, args...)
}

// ------------------------------------------------------------- host callbacks

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procLocalAlloc = kernel32.NewProc("LocalAlloc")
	procLocalFree  = kernel32.NewProc("LocalFree")
	harness        *check
)

// hostCall is the host-side callback the plugin reaches through
// cliproxy_host_api.call. Signature and buffer ownership match
// windowsHostCall in loader_windows.go.
func hostCall(hostCtx, methodPtr, requestPtr, requestLen, responsePtr uintptr) uintptr {
	if responsePtr != 0 {
		response := (*cliproxyBuffer)(unsafe.Pointer(responsePtr))
		response.ptr = 0
		response.len = 0
	}
	if methodPtr == 0 {
		return 1
	}
	method := goString(methodPtr)
	var request []byte
	if requestPtr != 0 && requestLen > 0 {
		request = unsafe.Slice((*byte)(unsafe.Pointer(requestPtr)), requestLen)
	}
	if harness != nil {
		harness.hostLog = append(harness.hostLog, fmt.Sprintf("%s %s", method, string(request)))
	}

	// The plugin never inspects host.log's result, so answer with an empty body
	// but stay allocation-compatible with the real host (LocalAlloc/LocalFree).
	resp := []byte(`{"ok":true}`)
	if len(resp) == 0 || responsePtr == 0 {
		return 0
	}
	mem, _, _ := procLocalAlloc.Call(0x0040 /* LPTR */, uintptr(len(resp)))
	if mem == 0 {
		return 1
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(mem)), len(resp)), resp)
	response := (*cliproxyBuffer)(unsafe.Pointer(responsePtr))
	response.ptr = mem
	response.len = uintptr(len(resp))
	return 0
}

func hostFree(ptr, length uintptr) uintptr {
	if ptr != 0 {
		procLocalFree.Call(ptr)
	}
	return 0
}

func goString(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	var out []byte
	for offset := uintptr(0); ; offset++ {
		b := *(*byte)(unsafe.Pointer(ptr + offset))
		if b == 0 {
			break
		}
		out = append(out, b)
	}
	return string(out)
}

// ------------------------------------------------------------------ RPC client

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error,omitempty"`
}

type client struct {
	api cliproxyPluginAPI
}

// call invokes cliproxyPluginCall and returns the parsed envelope plus the raw
// return code, mirroring dynamicLibraryClient.Call.
func (c *client) call(method string, request []byte) (envelope, int, error) {
	methodBytes, err := syscall.BytePtrFromString(method)
	if err != nil {
		return envelope{}, -1, err
	}
	var requestPtr uintptr
	if len(request) > 0 {
		requestPtr = uintptr(unsafe.Pointer(&request[0]))
	}
	var response cliproxyBuffer
	rc, _, _ := syscall.SyscallN(
		c.api.call,
		uintptr(unsafe.Pointer(methodBytes)),
		requestPtr,
		uintptr(len(request)),
		uintptr(unsafe.Pointer(&response)),
	)
	runtime.KeepAlive(methodBytes)
	runtime.KeepAlive(request)

	var out []byte
	if response.ptr != 0 && response.len > 0 {
		out = unsafe.Slice((*byte)(unsafe.Pointer(response.ptr)), response.len)
		out = append([]byte(nil), out...)
	}
	if response.ptr != 0 {
		syscall.SyscallN(c.api.freeBuffer, response.ptr, response.len)
	}
	if len(out) == 0 {
		return envelope{}, int(rc), fmt.Errorf("empty response body (rc=%d)", rc)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return envelope{}, int(rc), fmt.Errorf("decode envelope: %w (raw=%s)", err, out)
	}
	return env, int(rc), nil
}

// callNilMethod exercises the nil-method guard, which would segfault if the
// plugin passed the pointer straight to C.GoString.
func (c *client) callNilMethod() int {
	var response cliproxyBuffer
	rc, _, _ := syscall.SyscallN(c.api.call, 0, 0, 0, uintptr(unsafe.Pointer(&response)))
	if response.ptr != 0 {
		syscall.SyscallN(c.api.freeBuffer, response.ptr, response.len)
	}
	return int(rc)
}

func (c *client) mustResult(env envelope, rc int, err error) json.RawMessage {
	if err != nil {
		return nil
	}
	return env.Result
}

// ---------------------------------------------------------------------- main

func main() {
	dllPath := "prism-provider.dll"
	if len(os.Args) > 1 {
		dllPath = os.Args[1]
	}
	harness = &check{}

	fmt.Printf("loading %s (host ABI version %d)\n\n", dllPath, hostABIVersion)

	dll, err := syscall.LoadDLL(dllPath)
	if err != nil {
		fmt.Printf("FATAL: LoadDLL: %v\n", err)
		os.Exit(1)
	}
	defer dll.Release()

	proc, err := dll.FindProc("cliproxy_plugin_init")
	if err != nil {
		fmt.Printf("FATAL: FindProc(cliproxy_plugin_init): %v\n", err)
		os.Exit(1)
	}

	hostCtx := new(uintptr)
	*hostCtx = 1
	hostAPI := &cliproxyHostAPI{
		abiVersion: hostABIVersion,
		hostCtx:    uintptr(unsafe.Pointer(hostCtx)),
		call:       syscall.NewCallback(hostCall),
		freeBuffer: syscall.NewCallback(hostFree),
	}
	var pluginAPI cliproxyPluginAPI

	// ---- init -------------------------------------------------------------
	fmt.Println("plugin init")
	rc, _, _ := proc.Call(uintptr(unsafe.Pointer(hostAPI)), uintptr(unsafe.Pointer(&pluginAPI)))
	harness.assert("cliproxy_plugin_init returns 0", rc == 0, "got rc=%d", rc)
	harness.assert("plugin abi_version == host abi_version", pluginAPI.abiVersion == hostABIVersion,
		"plugin=%d host=%d", pluginAPI.abiVersion, hostABIVersion)
	harness.assert("function table complete",
		pluginAPI.call != 0 && pluginAPI.freeBuffer != 0 && pluginAPI.shutdown != 0,
		"call=%d free=%d shutdown=%d", pluginAPI.call, pluginAPI.freeBuffer, pluginAPI.shutdown)
	if rc != 0 || pluginAPI.call == 0 {
		fmt.Println("\ncannot continue without a working ABI")
		os.Exit(1)
	}

	cli := &client{api: pluginAPI}
	runProtocolChecks(cli)

	// ---- shutdown ---------------------------------------------------------
	fmt.Println("lifecycle")
	syscall.SyscallN(pluginAPI.shutdown)

	fmt.Printf("\n%d passed, %d failed\n", harness.passed, harness.failed)
	if len(harness.hostLog) > 0 {
		fmt.Printf("host callbacks observed: %d\n", len(harness.hostLog))
	}
	if harness.failed > 0 {
		os.Exit(1)
	}
}

func runProtocolChecks(cli *client) {
	// ---- plugin.register --------------------------------------------------
	fmt.Println("plugin.register (with config)")
	cfgYAML := []byte(`cookies: "prism_session_token=SESSION; prism_oai_access_token=ACCESS"
user_id: user-test123
project_title: PluginCheck
models: ["gpt-6-astra", "gpt-5-codex"]
default_model: gpt-6-astra
reasoning_effort: high
sandbox: true
system_prompt: "You are a test."
`)
	regReq, _ := json.Marshal(map[string]any{
		"config_yaml":    base64.StdEncoding.EncodeToString(cfgYAML),
		"schema_version": hostABIVersion,
	})
	env, rc, err := cli.call("plugin.register", regReq)
	harness.assert("register call succeeds", err == nil && rc == 0 && env.OK,
		"rc=%d err=%v ok=%v errEnv=%+v", rc, err, env.OK, env.Error)

	// schema_version is the one that silently bricks the plugin: the host
	// refuses any plugin advertising more than its own.
	var reg struct {
		SchemaVersion uint32 `json:"schema_version"`
		Metadata      struct {
			Name             string `json:"Name"`
			Version          string `json:"Version"`
			GitHubRepository string `json:"GitHubRepository"`
			ConfigFields     []struct {
				Name       string   `json:"Name"`
				Type       string   `json:"Type"`
				EnumValues []string `json:"EnumValues"`
			} `json:"ConfigFields"`
		} `json:"metadata"`
		Capabilities struct {
			ModelProvider         bool     `json:"model_provider"`
			AuthProvider          bool     `json:"auth_provider"`
			Executor              bool     `json:"executor"`
			ExecutorModelScope    string   `json:"executor_model_scope"`
			ExecutorInputFormats  []string `json:"executor_input_formats"`
			ExecutorOutputFormats []string `json:"executor_output_formats"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		harness.fail("register result decodes as rpcRegistration", "%v", err)
		return
	}
	harness.ok("register result decodes as rpcRegistration")
	harness.assert("schema_version <= host schema_version (1)",
		reg.SchemaVersion <= hostABIVersion,
		"plugin advertised %d; host accepts at most %d -> plugin would be rejected",
		reg.SchemaVersion, hostABIVersion)
	harness.assert("metadata.Name == prism-provider", reg.Metadata.Name == "prism-provider",
		"got %q", reg.Metadata.Name)
	harness.assert("capabilities: model_provider/auth_provider/executor",
		reg.Capabilities.ModelProvider && reg.Capabilities.AuthProvider && reg.Capabilities.Executor,
		"got %+v", reg.Capabilities)
	harness.assert("executor_model_scope is a pluginapi.ExecutorModelScope value",
		reg.Capabilities.ExecutorModelScope == "oauth" ||
			reg.Capabilities.ExecutorModelScope == "static" ||
			reg.Capabilities.ExecutorModelScope == "both",
		"got %q", reg.Capabilities.ExecutorModelScope)
	harness.assert("executor declares input+output formats",
		len(reg.Capabilities.ExecutorInputFormats) > 0 && len(reg.Capabilities.ExecutorOutputFormats) > 0,
		"in=%v out=%v", reg.Capabilities.ExecutorInputFormats, reg.Capabilities.ExecutorOutputFormats)

	badTypes := []string{}
	for _, f := range reg.Metadata.ConfigFields {
		if !validConfigFieldTypes[f.Type] {
			badTypes = append(badTypes, fmt.Sprintf("%s=%q", f.Name, f.Type))
		}
	}
	harness.assert("every ConfigField.Type is a pluginapi.ConfigFieldType",
		len(badTypes) == 0, "invalid: %v (valid: string|number|integer|boolean|enum|array|object)", badTypes)

	hasEnum := false
	for _, f := range reg.Metadata.ConfigFields {
		if f.Type == "enum" && len(f.EnumValues) == 0 {
			harness.fail("enum fields carry EnumValues", "%s has none", f.Name)
			return
		}
		if f.Type == "enum" {
			hasEnum = true
		}
	}
	if hasEnum {
		harness.ok("enum fields carry EnumValues")
	}

	// The config above must have been applied, which is only observable through
	// model.static and auth.refresh later. Verify parsing did not error out by
	// re-registering with no config at all.
	fmt.Println("plugin.register (no config block)")
	emptyReq, _ := json.Marshal(map[string]any{"schema_version": hostABIVersion})
	env, rc, err = cli.call("plugin.register", emptyReq)
	harness.assert("register with no config_yaml succeeds", err == nil && rc == 0 && env.OK,
		"rc=%d err=%v errEnv=%+v", rc, err, env.Error)

	fmt.Println("plugin.register (malformed config)")
	badReq, _ := json.Marshal(map[string]any{
		"config_yaml":    base64.StdEncoding.EncodeToString([]byte("models: [unclosed\n  - :\t\t")),
		"schema_version": hostABIVersion,
	})
	env, rc, err = cli.call("plugin.register", badReq)
	harness.assert("malformed config_yaml is reported as an error envelope",
		err == nil && rc == 1 && !env.OK && env.Error != nil,
		"rc=%d ok=%v err=%+v", rc, env.OK, env.Error)

	// restore good config for the remaining checks
	if _, _, err := cli.call("plugin.reconfigure", regReq); err != nil {
		fmt.Printf("  note: reconfigure failed: %v\n", err)
	}
	env, rc, err = cli.call("plugin.reconfigure", regReq)
	if err == nil && env.OK {
		var reg2 struct {
			SchemaVersion uint32 `json:"schema_version"`
		}
		if json.Unmarshal(env.Result, &reg2) == nil && reg2.SchemaVersion <= hostABIVersion {
			harness.ok("plugin.reconfigure returns a valid registration")
		} else {
			harness.fail("plugin.reconfigure returns a valid registration", "rc=%d result=%s", rc, env.Result)
		}
	} else {
		harness.fail("plugin.reconfigure returns a valid registration", "rc=%d err=%v errEnv=%+v", rc, err, env.Error)
	}

	// ---- identifiers ------------------------------------------------------
	fmt.Println("identifiers")
	for _, m := range []string{"auth.identifier", "executor.identifier"} {
		env, rc, err := cli.call(m, nil)
		var id struct {
			Identifier string `json:"identifier"`
		}
		if err == nil && rc == 0 && json.Unmarshal(env.Result, &id) == nil && id.Identifier == "prism-provider" {
			harness.ok(m + " -> prism-provider")
		} else {
			harness.fail(m+" -> prism-provider", "rc=%d err=%v result=%s", rc, err, env.Result)
		}
	}

	// ---- model.static / model.for_auth -----------------------------------
	fmt.Println("model listing")
	for _, m := range []string{"model.static", "model.for_auth"} {
		env, rc, err := cli.call(m, []byte(`{"Plugin":{"Name":"prism-provider"}}`))
		var mr struct {
			Provider string `json:"Provider"`
			Models   []struct {
				ID            string `json:"ID"`
				DisplayName   string `json:"DisplayName"`
				ContextLength int64  `json:"ContextLength"`
			} `json:"Models"`
		}
		if err != nil || rc != 0 || json.Unmarshal(env.Result, &mr) != nil {
			harness.fail(m+" returns a ModelResponse", "rc=%d err=%v result=%s", rc, err, env.Result)
			continue
		}
		ids := []string{}
		for _, x := range mr.Models {
			ids = append(ids, x.ID)
		}
		harness.assert(m+" returns configured models",
			mr.Provider == "prism-provider" && len(mr.Models) > 0 && mr.Models[0].ID != "",
			"provider=%q models=%v", mr.Provider, ids)
	}

	// ---- auth.parse -------------------------------------------------------
	fmt.Println("auth.parse")
	credJSON := []byte(`{"cookies":"prism_session_token=SESSION; prism_oai_access_token=ACCESS","userId":"user-abc","projectId":"11111111-2222-4333-8444-555555555555","label":"my prism"}`)
	parseReq, _ := json.Marshal(map[string]any{
		"Provider": "prism-provider",
		"FileName": "prism.json",
		"RawJSON":  base64.StdEncoding.EncodeToString(credJSON),
	})
	env, rc, err = cli.call("auth.parse", parseReq)
	var pr struct {
		Handled bool `json:"Handled"`
		Auth    *struct {
			Provider    string            `json:"Provider"`
			Label       string            `json:"Label"`
			Prefix      string            `json:"Prefix"`
			StorageJSON []byte            `json:"StorageJSON"`
			Metadata    map[string]any    `json:"Metadata"`
			Attributes  map[string]string `json:"Attributes"`
		} `json:"Auth"`
	}
	if err != nil || rc != 0 || json.Unmarshal(env.Result, &pr) != nil {
		harness.fail("auth.parse returns an AuthParseResponse", "rc=%d err=%v result=%s", rc, err, env.Result)
	} else {
		harness.assert("auth.parse handles a prism credential file", pr.Handled && pr.Auth != nil,
			"handled=%v auth=%+v", pr.Handled, pr.Auth)
		if pr.Auth != nil {
			var stored struct {
				Cookies   string `json:"cookies"`
				UserID    string `json:"userId"`
				ProjectID string `json:"projectId"`
			}
			errStored := json.Unmarshal(pr.Auth.StorageJSON, &stored)
			harness.assert("auth.parse round-trips the cookie into StorageJSON",
				errStored == nil && stored.Cookies != "" && stored.UserID == "user-abc" && stored.ProjectID != "",
				"err=%v stored=%+v", errStored, stored)
			harness.assert("auth.parse sets Provider/Label",
				pr.Auth.Provider == "prism-provider" && pr.Auth.Label == "my prism",
				"provider=%q label=%q", pr.Auth.Provider, pr.Auth.Label)
			// The host composes "<Prefix>/<modelID>", so a trailing separator
			// here would surface a malformed model alias to clients.
			harness.assert("auth.parse Prefix is a bare namespace segment",
				pr.Auth.Prefix != "" &&
					!strings.HasSuffix(pr.Auth.Prefix, "/") &&
					!strings.HasSuffix(pr.Auth.Prefix, "-"),
				"prefix=%q composes as %q", pr.Auth.Prefix, pr.Auth.Prefix+"/gpt-6-astra")
		}
	}

	notPrism, _ := json.Marshal(map[string]any{
		"Provider": "someone-else",
		"RawJSON":  base64.StdEncoding.EncodeToString([]byte(`{"access_token":"nope"}`)),
	})
	env, _, err = cli.call("auth.parse", notPrism)
	var pr2 struct {
		Handled bool `json:"Handled"`
	}
	if err == nil && json.Unmarshal(env.Result, &pr2) == nil && !pr2.Handled {
		harness.ok("auth.parse declines a foreign credential file")
	} else {
		harness.fail("auth.parse declines a foreign credential file", "err=%v result=%s", err, env.Result)
	}

	// ---- executor.count_tokens -------------------------------------------
	fmt.Println("executor.count_tokens")
	env, rc, err = cli.call("executor.count_tokens", []byte(`{"Model":"gpt-6-astra","Format":"chat-completions"}`))
	var er struct {
		Payload []byte `json:"Payload"`
	}
	if err != nil || rc != 0 || json.Unmarshal(env.Result, &er) != nil {
		harness.fail("count_tokens returns an ExecutorResponse", "rc=%d err=%v result=%s", rc, err, env.Result)
	} else {
		harness.assert("count_tokens Payload is a decodable JSON body",
			json.Valid(er.Payload) && len(er.Payload) > 0,
			"payload=%q (host injects this as the token-count response)", er.Payload)
	}

	// ---- error handling ---------------------------------------------------
	fmt.Println("error handling")
	env, rc, err = cli.call("does.not.exist", nil)
	harness.assert("unknown method yields an error envelope, not a transport fault",
		err == nil && !env.OK && env.Error != nil && env.Error.Code == "unknown_method",
		"rc=%d err=%v errEnv=%+v", rc, err, env.Error)

	rcNil := cli.callNilMethod()
	harness.assert("nil method pointer is handled without crashing", rcNil == 1, "rc=%d", rcNil)

	// ---- lifecycle --------------------------------------------------------
	fmt.Println("lifecycle")
	env, rc, err = cli.call("plugin.shutdown", nil)
	harness.assert("plugin.shutdown returns a success envelope", err == nil && rc == 0 && env.OK,
		"rc=%d err=%v errEnv=%+v", rc, err, env.Error)
}
