package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// withConfig runs fn against a known config and restores the global afterwards.
func withConfig(t *testing.T, cfg config, fn func()) {
	t.Helper()
	cfgMu.Lock()
	previous := cfgState
	cfgState = cfg
	cfgMu.Unlock()
	defer func() {
		cfgMu.Lock()
		cfgState = previous
		cfgMu.Unlock()
	}()
	fn()
}

// -------------------------------------------------------------- config parsing

func lifecycleJSON(t *testing.T, yaml string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"config_yaml":    base64.StdEncoding.EncodeToString([]byte(yaml)),
		"schema_version": 1,
	})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	return raw
}

func TestConfigureFromLifecycleRequest(t *testing.T) {
	withConfig(t, defaultConfig(), func() {
		req := lifecycleJSON(t, `
cookies: "prism_session_token=S; prism_oai_access_token=A"
user_id: user-abc
project_uuid: 11111111-2222-4333-8444-555555555555
project_title: My Title
models: ["gpt-6-astra", "gpt-5-codex"]
default_model: gpt-5-codex
reasoning_effort: HIGH
sandbox: true
system_prompt: "be terse"
`)
		if err := configure(req); err != nil {
			t.Fatalf("configure: %v", err)
		}
		got := currentConfig()
		if got.Cookies == "" || !strings.Contains(got.Cookies, "prism_session_token") {
			t.Errorf("Cookies = %q, want the cookie header", got.Cookies)
		}
		if got.UserID != "user-abc" {
			t.Errorf("UserID = %q, want user-abc", got.UserID)
		}
		if got.ProjectUUID != "11111111-2222-4333-8444-555555555555" {
			t.Errorf("ProjectUUID = %q", got.ProjectUUID)
		}
		if got.ProjectTitle != "My Title" {
			t.Errorf("ProjectTitle = %q, want My Title", got.ProjectTitle)
		}
		if len(got.Models) != 2 || got.Models[0] != "gpt-6-astra" || got.Models[1] != "gpt-5-codex" {
			t.Errorf("Models = %v, want both configured models", got.Models)
		}
		if got.DefaultModel != "gpt-5-codex" {
			t.Errorf("DefaultModel = %q", got.DefaultModel)
		}
		// normalizeEffort lowercases before matching, so HIGH must land on high.
		if got.ReasoningEffort != "high" {
			t.Errorf("ReasoningEffort = %q, want high", got.ReasoningEffort)
		}
		if !got.EnableSandbox {
			t.Error("EnableSandbox = false, want true")
		}
		if got.SystemPrompt != "be terse" {
			t.Errorf("SystemPrompt = %q", got.SystemPrompt)
		}
	})
}

// A single-line JSON array in a string is the form the docs ask operators to
// paste, so it has to keep working alongside real YAML sequences.
func TestConfigureAcceptsJSONArrayModelsString(t *testing.T) {
	withConfig(t, defaultConfig(), func() {
		req := lifecycleJSON(t, `models: '["alpha", "beta"]'`)
		if err := configure(req); err != nil {
			t.Fatalf("configure: %v", err)
		}
		got := currentConfig()
		if len(got.Models) != 2 || got.Models[0] != "alpha" || got.Models[1] != "beta" {
			t.Errorf("Models = %v, want [alpha beta]", got.Models)
		}
	})
}

// An unconfigured plugin must still load: CPA sends no config_yaml at all when
// the operator has not written the block yet.
func TestConfigureWithoutConfigKeepsDefaults(t *testing.T) {
	withConfig(t, defaultConfig(), func() {
		if err := configure([]byte(`{"schema_version":1}`)); err != nil {
			t.Fatalf("configure with no config_yaml: %v", err)
		}
		if got := currentConfig(); got.DefaultModel != "gpt-6-astra" {
			t.Errorf("DefaultModel = %q, want the default", got.DefaultModel)
		}
		if err := configure(nil); err != nil {
			t.Fatalf("configure(nil): %v", err)
		}
	})
}

func TestConfigureRejectsMalformedYAML(t *testing.T) {
	withConfig(t, defaultConfig(), func() {
		req := lifecycleJSON(t, "models: [unclosed\n  - :\t\t")
		if err := configure(req); err == nil {
			t.Fatal("configure accepted malformed YAML")
		}
	})
}

func TestNormalizeEffort(t *testing.T) {
	cases := map[string]string{
		"low": "low", "LOW": "low", " high ": "high",
		"xhigh": "xhigh", "medium": "medium", "nonsense": "medium", "": "medium",
	}
	for in, want := range cases {
		if got := normalizeEffort(in); got != want {
			t.Errorf("normalizeEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

// ------------------------------------------------------------- chat -> prism

func TestBuildInputInjectsSystemPromptAndMapsRoles(t *testing.T) {
	cfg := defaultConfig()
	cfg.SystemPrompt = "be terse"
	withConfig(t, cfg, func() {
		input, err := buildInput([]chatMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
			{Role: "assistant", Content: json.RawMessage(`"hi"`)},
			{Role: "user", Content: json.RawMessage(`"bye"`)},
		})
		if err != nil {
			t.Fatalf("buildInput: %v", err)
		}
		if len(input) != 4 {
			t.Fatalf("len(input) = %d, want 4 (injected system + 3 messages)", len(input))
		}
		if input[0]["role"] != "system" {
			t.Errorf("input[0].role = %v, want system", input[0]["role"])
		}
		content := input[0]["content"].([]map[string]any)
		if content[0]["text"] != "be terse" {
			t.Errorf("system text = %v, want the configured prompt", content[0]["text"])
		}
		// The assistant turn must come back as output_text, not input_text.
		assistantContent := input[2]["content"].([]map[string]any)
		if assistantContent[0]["type"] != "output_text" {
			t.Errorf("assistant content type = %v, want output_text", assistantContent[0]["type"])
		}
		if input[3]["content"].([]map[string]any)[0]["text"] != "bye" {
			t.Error("last user message text was not carried through")
		}
	})
}

func TestBuildInputDoesNotDuplicateExistingSystemMessage(t *testing.T) {
	withConfig(t, defaultConfig(), func() {
		input, err := buildInput([]chatMessage{
			{Role: "system", Content: json.RawMessage(`"from client"`)},
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		})
		if err != nil {
			t.Fatalf("buildInput: %v", err)
		}
		if len(input) != 2 {
			t.Fatalf("len(input) = %d, want 2 (no injection)", len(input))
		}
		if input[0]["content"].([]map[string]any)[0]["text"] != "from client" {
			t.Error("client system message was not preserved")
		}
	})
}

func TestBuildInputFlattensMultimodalContent(t *testing.T) {
	withConfig(t, defaultConfig(), func() {
		content := json.RawMessage(`[{"type":"text","text":"look at this"},{"type":"image_url","image_url":{"url":"data:..."}}]`)
		input, err := buildInput([]chatMessage{{Role: "user", Content: content}})
		if err != nil {
			t.Fatalf("buildInput: %v", err)
		}
		// input[0] is the injected system message; the user turn is input[1].
		got := input[1]["content"].([]map[string]any)[0]["text"]
		if got != "look at this" {
			t.Errorf("flattened text = %q, want only the text part", got)
		}
	})
}

func TestBuildInputRejectsOversizedHistory(t *testing.T) {
	withConfig(t, defaultConfig(), func() {
		huge := strings.Repeat("x", maxInputBytes+1)
		_, err := buildInput([]chatMessage{{Role: "user", Content: json.RawMessage(jsonString(huge))}})
		if err == nil {
			t.Fatal("buildInput accepted a history past maxInputBytes")
		}
		if !strings.Contains(err.Error(), "上限") {
			t.Errorf("error = %v, want the size-limit message", err)
		}
	})
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func TestResolveModel(t *testing.T) {
	cfg := defaultConfig()
	cfg.Models = []string{"gpt-6-astra", "gpt-5-codex"}
	withConfig(t, cfg, func() {
		cases := []struct{ requested, body, want string }{
			{"gpt-6-astra", "", "gpt-6-astra"},
			{"", "gpt-5-codex", "gpt-5-codex"},
			// Provider prefixes are stripped before matching.
			{"prism-provider/gpt-5-codex", "", "gpt-5-codex"},
			// Case-insensitive match returns the configured spelling.
			{"GPT-6-ASTRA", "", "gpt-6-astra"},
			// Unknown models pass through rather than being silently rewritten.
			{"something-else", "", "something-else"},
			// Nothing requested at all falls back to the default.
			{"", "", "gpt-6-astra"},
		}
		for _, c := range cases {
			if got := resolveModel(c.requested, c.body); got != c.want {
				t.Errorf("resolveModel(%q, %q) = %q, want %q", c.requested, c.body, got, c.want)
			}
		}
	})
}

// --------------------------------------------------------- prism -> client

func TestTurnPayloadTextSkipsNonMessageItems(t *testing.T) {
	payload := turnPayload{Output: []outputItem{
		{Type: "reasoning", Content: []contentPart{{Type: "output_text", Text: "thinking"}}},
		{Type: "function_call", Content: []contentPart{{Type: "output_text", Text: "tool"}}},
		{Type: "message", Role: "user", Content: []contentPart{{Type: "output_text", Text: "echo"}}},
		{Type: "message", Role: "assistant", Content: []contentPart{
			{Type: "output_text", Text: "the "},
			{Type: "output_text", Text: "answer"},
		}},
		{Type: "message", Role: "assistant", Content: []contentPart{{Type: "refusal", Refusal: "no"}}},
	}}
	if got := payload.Text(); got != "the answerno" {
		t.Errorf("Text() = %q, want %q", got, "the answerno")
	}
}

func TestCompletionIsValidChatCompletion(t *testing.T) {
	raw := completion("gpt-6-astra", "resp_123", "hello")
	var body struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("completion is not valid JSON: %v", err)
	}
	if body.Object != "chat.completion" {
		t.Errorf("object = %q", body.Object)
	}
	if body.ID != "chatcmpl-resp_123" {
		t.Errorf("id = %q, want chatcmpl-resp_123", body.ID)
	}
	if body.Model != "gpt-6-astra" {
		t.Errorf("model = %q", body.Model)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "hello" ||
		body.Choices[0].Message.Role != "assistant" || body.Choices[0].FinishReason != "stop" {
		t.Errorf("choices = %+v", body.Choices)
	}
}

func TestSSEChunksAreWellFormed(t *testing.T) {
	chunks := sseChunks("gpt-6-astra", "resp_123", "hi")
	if len(chunks) != 4 {
		t.Fatalf("len(chunks) = %d, want 4", len(chunks))
	}
	for i, chunk := range chunks[:3] {
		text := string(chunk.Payload)
		if !strings.HasPrefix(text, "data: ") || !strings.HasSuffix(text, "\n\n") {
			t.Fatalf("chunk %d is not an SSE frame: %q", i, text)
		}
		var frame struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta        map[string]any `json:"delta"`
				FinishReason any            `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(text, "data: "), "\n\n")), &frame); err != nil {
			t.Fatalf("chunk %d payload is not valid JSON: %v", i, err)
		}
		if frame.Object != "chat.completion.chunk" {
			t.Errorf("chunk %d object = %q", i, frame.Object)
		}
		if len(frame.Choices) != 1 {
			t.Errorf("chunk %d has %d choices", i, len(frame.Choices))
		}
	}
	if string(chunks[3].Payload) != "data: [DONE]\n\n" {
		t.Errorf("last chunk = %q, want the [DONE] terminator", chunks[3].Payload)
	}
}

// ------------------------------------------------------------------ credentials

func TestParseStoredFallsBackToConfigCookie(t *testing.T) {
	cfg := defaultConfig()
	cfg.Cookies = "prism_session_token=FROM_CONFIG"
	withConfig(t, cfg, func() {
		// A credential file with no cookie should inherit the configured one.
		sa, err := parseStored([]byte(`{"userId":"user-1"}`))
		if err != nil {
			t.Fatalf("parseStored: %v", err)
		}
		if sa.Cookies != "prism_session_token=FROM_CONFIG" {
			t.Errorf("Cookies = %q, want the config fallback", sa.Cookies)
		}
		if sa.UserID != "user-1" {
			t.Errorf("UserID = %q, want user-1", sa.UserID)
		}
	})
}

// With no cookie in either the credential file or the config there is nothing
// to authenticate with, and that must fail loudly rather than start an
// anonymous session.
func TestParseStoredRequiresCookieSomewhere(t *testing.T) {
	cfg := defaultConfig()
	cfg.Cookies = ""
	withConfig(t, cfg, func() {
		for _, raw := range []string{`{}`, `{"userId":"user-1"}`, `{"cookies":"   "}`, ``} {
			if _, err := parseStored([]byte(raw)); err == nil {
				t.Errorf("parseStored(%q) accepted a credential with no cookie", raw)
			}
		}
	})
}

func TestStoredAuthRoundTrip(t *testing.T) {
	original := &storedAuth{Cookies: "c=1", UserID: "user-1", ProjectID: "p-1"}
	var decoded storedAuth
	if err := json.Unmarshal(original.encode(), &decoded); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if decoded != *original {
		t.Errorf("round trip = %+v, want %+v", decoded, *original)
	}
}

func TestNewUUIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := newUUID()
		if len(id) != 36 {
			t.Fatalf("newUUID() = %q, want 36 chars", id)
		}
		if id[14] != '4' {
			t.Errorf("newUUID() = %q, want version nibble 4", id)
		}
		switch id[19] {
		case '8', '9', 'a', 'b':
		default:
			t.Errorf("newUUID() = %q, want RFC 4122 variant nibble", id)
		}
		if seen[id] {
			t.Fatalf("newUUID() repeated %q", id)
		}
		seen[id] = true
	}
}

func TestIsCloudflareChallenge(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"cloudflare html page", "<!DOCTYPE html>\n<!--[if lt IE 7]>...<title>Attention Required! | Cloudflare</title>", true},
		{"generic html", "<html><body>blocked</body></html>", true},
		{"cloudflare named in text", "error: see Cloudflare docs", true},
		{"prism json error", `{"error":{"message":"ConversationTooLarge"}}`, false},
		{"empty body", "", false},
		// Only the first 2 KiB are inspected; a challenge page starts with the
		// doctype, so a long body must still be recognised.
		{"long challenge page", "<!DOCTYPE html>" + strings.Repeat("x", 8192), true},
	}
	for _, c := range cases {
		if got := isCloudflareChallenge([]byte(c.body)); got != c.want {
			t.Errorf("%s: isCloudflareChallenge = %v, want %v", c.name, got, c.want)
		}
	}
}

// Observed live: HTTP 500 {"status":"error","message":"auth-session-policy-unavailable"}.
// It must surface that message rather than "no request_id", and it must not be
// mistaken for a poll-forever state.
func TestTerminalTurnError(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantNil    bool
		wantStatus int
		wantText   string
	}{
		{"auth policy unavailable", `{"status":"error","message":"auth-session-policy-unavailable"}`, false, 401, "auth-session-policy-unavailable"},
		{"failure without message", `{"status":"failed"}`, false, 502, "failed"},
		{"other terminal error", `{"status":"error","message":"boom"}`, false, 502, "boom"},
		{"conversation too large", `{"status":"error","message":"ConversationTooLarge"}`, false, 502, "ConversationTooLarge"},
		{"still running", `{"status":"started","request_id":"r1"}`, true, 0, ""},
		{"pending", `{"status":"pending","turn_state":{}}`, true, 0, ""},
		{"completed", `{"status":"completed"}`, true, 0, ""},
		{"no status at all", `{}`, true, 0, ""},
	}
	for _, c := range cases {
		var resp turnResponse
		if err := json.Unmarshal([]byte(c.body), &resp); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		got := terminalTurnError(&resp)
		if c.wantNil {
			if got != nil {
				t.Errorf("%s: terminalTurnError = %v, want nil", c.name, got)
			}
			continue
		}
		if got == nil {
			t.Fatalf("%s: terminalTurnError = nil, want a prismError", c.name)
		}
		if got.HTTPStatus() != c.wantStatus {
			t.Errorf("%s: HTTPStatus() = %d, want %d", c.name, got.HTTPStatus(), c.wantStatus)
		}
		if !strings.Contains(got.Error(), c.wantText) {
			t.Errorf("%s: Error() = %q, want it to contain %q", c.name, got.Error(), c.wantText)
		}
	}
}

// TestRealCompletedStatusResponse pins the parser to bytes captured from real
// traffic (prism/testdata/status_completed.json, lifted verbatim from
// POST /api/llm/response_with_tools_status, HTTP 200). Hand-written fixtures
// drift from reality; this one cannot.
func TestRealCompletedStatusResponse(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "status_completed.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var resp turnResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("real response does not decode into turnResponse: %v", err)
	}

	// The outer envelope reports completion...
	if resp.Status != "completed" {
		t.Fatalf("Status = %q, want completed", resp.Status)
	}
	if resp.RequestID == "" {
		t.Error("RequestID is empty; the poll loop needs it")
	}
	if err := terminalTurnError(&resp); err != nil {
		t.Errorf("a successful turn must not look terminal-failed: %v", err)
	}

	// ...and the inner result carries the assistant text.
	payload, err := interpret(resp.Response)
	if err != nil {
		t.Fatalf("interpret: %v", err)
	}
	if got, want := payload.Text(), "What would you like me to do with `main.tex`?"; got != want {
		t.Errorf("Text() = %q, want %q", got, want)
	}
	if payload.ID != "resp_mu44y9j3_g17m9dtz" {
		t.Errorf("payload.ID = %q", payload.ID)
	}
	if payload.ConversationID == "" {
		t.Error("ConversationID was not carried through")
	}
}

// A real start response returns status "started" plus a turn_state the client
// must echo back, which is why polling exists at all.
func TestRealStartedResponseRequiresPolling(t *testing.T) {
	raw := []byte(`{"status":"started","request_id":"16f9426b-6de9-44c5-8886-b552037caec5",` +
		`"conversation_id":"cdx1_aa3f4d64-5035-4acc-88be-2bacb4bc7177",` +
		`"turn_state":{"version":1,"conversation_id":"cdx1_aa3f4d64-5035-4acc-88be-2bacb4bc7177",` +
		`"sandbox_url":"http://crixet-backend.oai-science.svc.cluster.local:8081/sandboxes/proxy/"}}`)
	var resp turnResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != "started" {
		t.Fatalf("Status = %q, want started", resp.Status)
	}
	if len(resp.TurnState) == 0 {
		t.Error("turn_state must be echoed back on the next poll")
	}
	// "started" is not terminal, so the loop keeps polling rather than failing.
	if pe := terminalTurnError(&resp); pe != nil {
		t.Errorf("started must not be terminal: %v", pe)
	}
}

// Measured live 2026-09-17: the wire spells reasons snake_case while the
// frontend enum spells them CamelCase. Comparing literally meant a retryable
// turn was reported as a hard failure.
func TestSameReasonMatchesWireSpelling(t *testing.T) {
	cases := []struct {
		wire, enum string
		want       bool
	}{
		{"sandbox_reconnecting", "SandboxReconnecting", true},
		{"SandboxReconnecting", "SandboxReconnecting", true},
		{"SANDBOX_RECONNECTING", "SandboxReconnecting", true},
		{"conversation_too_large", "ConversationTooLarge", true},
		{"conversation-too-large", "ConversationTooLarge", true},
		{"unknown", "SandboxReconnecting", false},
		{"", "SandboxReconnecting", false},
	}
	for _, c := range cases {
		if got := sameReason(c.wire, c.enum); got != c.want {
			t.Errorf("sameReason(%q, %q) = %v, want %v", c.wire, c.enum, got, c.want)
		}
	}
}

// The verbatim live payload for a missing sandbox must be classified as
// retryable, and must not be mistaken for a terminal turn failure.
func TestRealSandboxReconnectingPayloadIsRetryable(t *testing.T) {
	raw := []byte(`{"status":"completed","request_id":"b058d305-6593-4617-ac1c-534ebcccf821",` +
		`"conversation_id":"cdx1_ce256438-6ba2-4851-9490-b7a2b9217535",` +
		`"response":{"status":"error","payload":{"reason":"sandbox_reconnecting",` +
		`"message":"Reconnecting to sandbox. Your request will resume automatically once the sandbox is ready.",` +
		`"codexRequestDebug":{"sandbox_url_input":null,"sandbox_url_resolved":null}}}}`)
	var resp turnResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pe := terminalTurnError(&resp); pe != nil {
		t.Fatalf("outer status is completed, so this is not terminal: %v", pe)
	}
	if _, err := interpret(resp.Response); !errors.Is(err, errSandboxReconnecting) {
		t.Fatalf("interpret = %v, want errSandboxReconnecting so the caller retries", err)
	}
}

// ------------------------------------------------------- y-sweet handshake

// These exact frames were sent to the live provider on 2026-09-17 and got the
// document back, so they are pinned rather than derived.
func TestYjsFrameEncodings(t *testing.T) {
	if got, want := ySyncStep1Frame(), []byte{0x00, 0x00, 0x01, 0x00}; !bytes.Equal(got, want) {
		t.Errorf("SyncStep1 = % x, want % x", got, want)
	}
	if got, want := ySyncStep2Frame(emptyUpdate), []byte{0x00, 0x01, 0x01, 0x00}; !bytes.Equal(got, want) {
		t.Errorf("empty SyncStep2 = % x, want % x", got, want)
	}
}

func TestAppendVarUint(t *testing.T) {
	cases := []struct {
		in   uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xac, 0x02}},
		{16384, []byte{0x80, 0x80, 0x01}},
	}
	for _, c := range cases {
		got := appendVarUint(nil, c.in)
		if !bytes.Equal(got, c.want) {
			t.Errorf("appendVarUint(%d) = % x, want % x", c.in, got, c.want)
		}
		n, i, ok := readVarUint(got, 0)
		if !ok || n != c.in || i != len(got) {
			t.Errorf("readVarUint(% x) = (%d, %d, %v), want (%d, %d, true)", got, n, i, ok, c.in, len(got))
		}
	}
}

func TestReadVarUintRejectsTruncatedInput(t *testing.T) {
	for _, buf := range [][]byte{{}, {0x80}, {0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80}} {
		if _, _, ok := readVarUint(buf, 0); ok {
			t.Errorf("readVarUint(% x) reported success on truncated input", buf)
		}
	}
}

// A stale cached sandbox must not be handed out; the y-sweet socket dies with it.
func TestEnsureSandboxDropsStaleEntry(t *testing.T) {
	const project = "11111111-2222-4333-8444-555555555555"
	closed := false
	sandboxMu.Lock()
	sandboxCache[project] = &sandboxSession{
		URL:     "https://prism.openai.com/s/sandboxes/proxy/",
		Token:   "stale",
		bust:    "1",
		expires: time.Now().Add(-time.Minute),
		yjs:     nil,
	}
	sandboxMu.Unlock()
	defer func() {
		sandboxMu.Lock()
		delete(sandboxCache, project)
		sandboxMu.Unlock()
		_ = closed
	}()
	// A nil session must not panic when it is evicted.
	dropSandbox(project)
	sandboxMu.Lock()
	_, still := sandboxCache[project]
	sandboxMu.Unlock()
	if still {
		t.Error("dropSandbox left the entry in the cache")
	}
}

func TestSandboxURLWithBust(t *testing.T) {
	s := &sandboxSession{URL: "https://prism.openai.com/s/sandboxes/proxy/", bust: "12345"}
	got := s.urlWithBust("wait-for-sync")
	want := "https://prism.openai.com/s/sandboxes/proxy/wait-for-sync?prism_cache_bust=12345"
	if got != want {
		t.Errorf("urlWithBust = %q, want %q", got, want)
	}
}

func TestNewCacheBustIsNumeric(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		bust := newCacheBust()
		if bust == "" {
			t.Fatal("newCacheBust returned empty")
		}
		if _, err := strconv.ParseUint(bust, 10, 64); err != nil {
			t.Fatalf("newCacheBust returned non-numeric %q", bust)
		}
		seen[bust] = true
	}
	if len(seen) < 2 {
		t.Error("newCacheBust is not varying")
	}
}

// The response path must carry non-ASCII through unchanged: prism answers in the
// user's language, so any re-encoding here would corrupt every reply. (The build
// machine's console mangles Chinese on display, which is why this asserts bytes.)
func TestCompletionPreservesNonASCII(t *testing.T) {
	const answer = `LaTeX 中，\label{名称} 用于设置引用标记，\ref{名称} 用于显示编号。`
	raw := completion("gpt-6-astra", "resp_x", answer)

	if !utf8.Valid(raw) {
		t.Fatal("completion produced invalid UTF-8")
	}
	// Check a run with no characters JSON has to escape, so this compares the
	// encoding rather than JSON escaping rules.
	if !bytes.Contains(raw, []byte("中，")) || !bytes.Contains(raw, []byte("设置引用标记")) {
		t.Errorf("Chinese text did not survive the response path verbatim:\n got %s", raw)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("completion is not valid JSON: %v", err)
	}
	if got := decoded.Choices[0].Message.Content; got != answer {
		t.Errorf("round trip = %q, want %q", got, answer)
	}

	// The envelope must not escape or corrupt it either.
	wrapped, err := okEnvelope(executorResponse{Payload: raw})
	if err != nil {
		t.Fatalf("okEnvelope: %v", err)
	}
	var env struct {
		Result struct {
			Payload []byte `json:"Payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(wrapped, &env); err != nil {
		t.Fatalf("envelope decode: %v", err)
	}
	var throughEnvelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(env.Result.Payload, &throughEnvelope); err != nil {
		t.Fatalf("payload did not survive the envelope: %v", err)
	}
	if got := throughEnvelope.Choices[0].Message.Content; got != answer {
		t.Errorf("envelope round trip = %q, want %q", got, answer)
	}
}

// A credential-less-to-prism request must not mint a new project every time:
// the client is rebuilt per request, so the project has to be remembered
// out-of-band. This is the regression guard for that.
func TestProjectCacheReusesPerCredential(t *testing.T) {
	a := credentialKey("prism_session_token=AAA; prism_oai_access_token=BBB")
	b := credentialKey("prism_session_token=CCC; prism_oai_access_token=DDD")
	if a == b {
		t.Fatal("different credentials produced the same cache key")
	}
	if a == "" || len(a) != 32 {
		t.Fatalf("credentialKey = %q, want 32 hex chars", a)
	}
	if credentialKey("prism_session_token=AAA; prism_oai_access_token=BBB") != a {
		t.Error("credentialKey is not stable for the same cookie")
	}

	dropProject(a)
	dropProject(b)
	defer func() { dropProject(a); dropProject(b) }()

	if _, ok := lookupProject(a); ok {
		t.Fatal("cache hit before anything was remembered")
	}
	rememberProject(a, "11111111-2222-4333-8444-555555555555")

	got, ok := lookupProject(a)
	if !ok || got != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("lookupProject = (%q, %v)", got, ok)
	}
	// Another credential must not inherit it.
	if _, ok := lookupProject(b); ok {
		t.Error("a different credential saw another credential's project")
	}
	// An empty key must never match, or every anonymous caller would share one.
	if _, ok := lookupProject(""); ok {
		t.Error("empty credential key matched a cached project")
	}
	rememberProject("", "should-be-ignored")
	if _, ok := lookupProject(""); ok {
		t.Error("an empty credential key was cached")
	}
}

func TestProjectCacheExpires(t *testing.T) {
	key := "expiry-test-key"
	defer dropProject(key)
	projectMu.Lock()
	projectCache[key] = projectEntry{uuid: "deadbeef", expires: time.Now().Add(-time.Second)}
	projectMu.Unlock()
	if _, ok := lookupProject(key); ok {
		t.Error("an expired project entry was returned")
	}
	projectMu.Lock()
	_, lingering := projectCache[key]
	projectMu.Unlock()
	if lingering {
		t.Error("an expired entry was left in the cache")
	}
}

// ensureProject must prefer an explicitly configured UUID over creating one.
func TestEnsureProjectPrefersConfigUUID(t *testing.T) {
	cfg := defaultConfig()
	cfg.ProjectUUID = "22222222-3333-4444-8555-666666666666"
	withConfig(t, cfg, func() {
		client := &prismClient{credKey: "config-precedence-test"}
		defer dropProject(client.credKey)
		got, err := client.ensureProject(context.Background())
		if err != nil {
			t.Fatalf("ensureProject: %v", err)
		}
		if got != cfg.ProjectUUID {
			t.Errorf("ensureProject = %q, want the configured %q", got, cfg.ProjectUUID)
		}
	})
}

// ------------------------------------------------------------- login flow

func TestCallbackCodeParsing(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"full callback url", "https://prism.openai.com/auth/popup-callback?code=ac_ABC.def-123", "ac_ABC.def-123", true},
		{"url with framing quotes", `"https://prism.openai.com/auth/popup-callback?code=ac_XYZ"`, "ac_XYZ", true},
		{"bare code fragment", "code=ac_QQQ&state=ignored", "ac_QQQ", true},
		{"bare code only", "code=ac_SOLO", "ac_SOLO", true},
		{"popup-callback without code", "https://prism.openai.com/auth/popup-callback", "", false},
		{"unrelated url", "https://prism.openai.com/?u=x", "", false},
		// A cookie header must never be read as a callback, even though it can
		// contain "code=" inside an unrelated value.
		{"cookie header", "prism_session_token=abc; oai-sc=0gAAAAABqqcode=QQ", "", false},
		{"empty", "", "", false},
		{"whitespace", "   \n\t ", "", false},
	}
	for _, c := range cases {
		got, ok := callbackCode(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: callbackCode(%q) = (%q, %v), want (%q, %v)", c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLoginStateIsHostSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		state := newLoginState()
		if state == "" {
			t.Fatal("newLoginState returned empty")
		}
		// Mirrors the host's ValidateOAuthState.
		if strings.ContainsAny(state, `/\`) || strings.Contains(state, "..") {
			t.Fatalf("newLoginState produced %q, which the host rejects", state)
		}
		for _, r := range state {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				t.Fatalf("newLoginState produced %q with a disallowed character %q", state, r)
			}
		}
		if seen[state] {
			t.Fatalf("newLoginState repeated %q", state)
		}
		seen[state] = true
	}
}

func TestCookieHeaderRoundTrip(t *testing.T) {
	header := "prism_session_token=AAA; prism_oai_access_token=BBB; cf_clearance=CCC"
	cookies := parseCookieHeader(header)
	if len(cookies) != 3 {
		t.Fatalf("parseCookieHeader returned %d cookies, want 3", len(cookies))
	}
	if got := joinCookieHeader(cookies); got != header {
		t.Errorf("round trip = %q, want %q", got, header)
	}
	// Empty and malformed fragments must be skipped, not emitted as bare names.
	got := joinCookieHeader(parseCookieHeader("a=1; broken; =noname; b=2"))
	if got != "a=1; b=2" {
		t.Errorf("malformed fragments leaked into %q", got)
	}
}

func TestPollLoginWithoutFlow(t *testing.T) {
	loginMu.Lock()
	previous := activeLogin
	activeLogin = nil
	loginMu.Unlock()
	defer func() { loginMu.Lock(); activeLogin = previous; loginMu.Unlock() }()

	status, message, auth := pollLogin("prismdeadbeef")
	if status != "error" || auth != nil || message == "" {
		t.Errorf("pollLogin with no flow = (%q, %q, %v), want an error with a message", status, message, auth)
	}
}

func TestPollLoginPendingThenSuccess(t *testing.T) {
	flow := &loginFlow{state: "prismtestflow", created: time.Now()}
	loginMu.Lock()
	previous := activeLogin
	activeLogin = flow
	loginMu.Unlock()
	defer func() { loginMu.Lock(); activeLogin = previous; loginMu.Unlock() }()

	status, message, auth := pollLogin(flow.state)
	if status != "pending" || auth != nil {
		t.Fatalf("fresh flow = (%q, %v), want pending", status, auth)
	}
	// The operator has to be told what to paste, and where.
	// Without a browser to drive, the fallback has to name the file the operator
	// pastes into.
	if !strings.Contains(message, callbackFileHint) {
		t.Errorf("pending fallback does not name the paste target: %q", message)
	}

	// A mismatched state must not expose another flow's progress.
	if status, _, _ := pollLogin("prismsomeoneelse"); status != "error" {
		t.Errorf("mismatched state returned %q, want error", status)
	}

	want := &authData{Provider: providerName, Label: "prism (browser sign-in)"}
	flow.result = want
	status, _, auth = pollLogin(flow.state)
	if status != "success" || auth != want {
		t.Errorf("completed flow = (%q, %v), want success with the auth", status, auth)
	}

	// An expired flow must not be reported as still pending.
	flow.result = nil
	flow.created = time.Now().Add(-2 * loginFlowTTL)
	if status, _, _ := pollLogin(flow.state); status != "error" {
		t.Errorf("expired flow returned %q, want error", status)
	}
	if pendingLogin() != nil {
		t.Error("pendingLogin returned an expired flow")
	}
}

// The callback page closes itself, so the operator cannot copy the address bar.
// Whatever DevTools hands them -- a cURL line, a request line, a header dump --
// has to be parseable.
func TestCallbackCodeFromDevToolsPaste(t *testing.T) {
	const urlPart = "https://prism.openai.com/auth/popup-callback?code=ac_CMD123&state=prism_openai_oauth_state.v1.abc"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"copy as curl, multiple headers", "curl '" + urlPart + "' -H 'accept: */*' -H 'cookie: x=y'", "ac_CMD123"},
		{"curl with double quotes", `curl "` + urlPart + `" --compressed`, "ac_CMD123"},
		{"request line", "GET " + urlPart + " HTTP/2", "ac_CMD123"},
		{"hAR style json", `"url": "` + urlPart + `",`, "ac_CMD123"},
		{"markdown link", "[here](" + urlPart + ")", "ac_CMD123"},
		{"bare url still works", urlPart, "ac_CMD123"},
		{"url with no code", "curl 'https://prism.openai.com/auth/popup-callback?state=x'", ""},
		{"unrelated url in text", "curl 'https://prism.openai.com/api/llm/response_with_tools_start?code=zzz'", ""},
	}
	for _, c := range cases {
		got, ok := callbackCode(c.in)
		if c.want == "" {
			if ok {
				t.Errorf("%s: callbackCode = (%q, true), want no match", c.name, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("%s: callbackCode = (%q, %v), want (%q, true)", c.name, got, ok, c.want)
		}
	}
}

// The host's callback box writes .oauth-<provider>-<state>.oauth containing
// {"code":…,"state":…} and hands it to auth.parse. It must be recognised as a
// sign-in completion, not shrugged off as an unknown credential file. With no
// flow in flight the completion fails, but the error must be about the flow --
// that is what proves the file was routed down the callback path.
func TestParseAuthAcceptsHostCallbackFile(t *testing.T) {
	loginMu.Lock()
	previous := activeLogin
	activeLogin = nil
	loginMu.Unlock()
	defer func() { loginMu.Lock(); activeLogin = previous; loginMu.Unlock() }()

	payload := base64.StdEncoding.EncodeToString([]byte(
		`{"code":"ac_TESTCODE","state":"prism_openai_oauth_state.v1.abc.def"}`))
	request := []byte(`{"Provider":"prism-provider","FileName":".oauth-prism-provider-x.oauth","RawJSON":"` + payload + `"}`)

	out, err := handleParseAuth(request)
	if err != nil {
		t.Fatalf("handleParseAuth returned a Go error: %v", err)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("envelope decode: %v", err)
	}
	if env.OK {
		t.Fatal("a callback with no flow in flight should not report success")
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "登录流程") {
		t.Fatalf("message = %+v, want it to be about the pending flow", env.Error)
	}
}

// ------------------------------------------------------------------ management

// The plugin registers its own sign-in endpoint because the host's callback box
// rejects prism's state (484 chars vs the host's 128-char limit).
func TestManagementRegisterExposesCallbackRoute(t *testing.T) {
	out, err := handleManagementRegister()
	if err != nil {
		t.Fatalf("handleManagementRegister: %v", err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Routes []struct {
				Method string `json:"Method"`
				Path   string `json:"Path"`
			} `json:"Routes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("envelope decode: %v", err)
	}
	if !env.OK || len(env.Result.Routes) == 0 {
		t.Fatalf("registration = %s", out)
	}
	methods := map[string]bool{}
	for _, route := range env.Result.Routes {
		if route.Path != callbackPath {
			t.Errorf("route path = %q, want %q", route.Path, callbackPath)
		}
		methods[route.Method] = true
	}
	if !methods["POST"] || !methods["GET"] {
		t.Errorf("routes must cover POST and GET, got %v", methods)
	}
}

// Both accepted shapes -- query parameters and a JSON body -- must reach the
// completion path. With no flow in flight that path fails, and the failure has
// to be about the flow, which is what proves the callback was parsed.
func TestManagementHandleAcceptsBothShapes(t *testing.T) {
	loginMu.Lock()
	previous := activeLogin
	activeLogin = nil
	loginMu.Unlock()
	defer func() { loginMu.Lock(); activeLogin = previous; loginMu.Unlock() }()

	const cb = "https://prism.openai.com/auth/popup-callback?code=ac_MGMT&state=prism_openai_oauth_state.v1.x.y"
	requests := []string{
		`{"Method":"POST","Path":"` + callbackPath + `","Query":{"redirect_url":["` + cb + `"]}}`,
		`{"Method":"POST","Path":"` + callbackPath + `","Body":"` + base64.StdEncoding.EncodeToString([]byte(`{"redirect_url":"`+cb+`"}`)) + `"}`,
	}
	for i, request := range requests {
		out, err := handleManagementHandle([]byte(request))
		if err != nil {
			t.Fatalf("shape %d: %v", i, err)
		}
		var env struct {
			OK     bool `json:"ok"`
			Result struct {
				StatusCode int    `json:"StatusCode"`
				Body       []byte `json:"Body"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("shape %d: envelope decode: %v", i, err)
		}
		if env.Result.StatusCode != 400 {
			t.Errorf("shape %d: StatusCode = %d, want 400", i, env.Result.StatusCode)
		}
		if !strings.Contains(string(env.Result.Body), "登录流程") {
			t.Errorf("shape %d: body = %s, want it to be about the pending flow", i, env.Result.Body)
		}
	}

	// A request with no callback at all must be rejected with guidance.
	out, err := handleManagementHandle([]byte(`{"Method":"GET","Query":{}}`))
	if err != nil {
		t.Fatalf("empty request: %v", err)
	}
	var emptyEnv struct {
		OK     bool `json:"ok"`
		Result struct {
			Body []byte `json:"Body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &emptyEnv); err != nil {
		t.Fatalf("empty request: envelope decode: %v", err)
	}
	if !strings.Contains(string(emptyEnv.Result.Body), "redirect_url") {
		t.Errorf("empty request should say what to send: %s", emptyEnv.Result.Body)
	}
}

func TestPanelServesFormAndReportsOutcome(t *testing.T) {
	loginMu.Lock()
	previous := activeLogin
	activeLogin = nil
	loginMu.Unlock()
	defer func() { loginMu.Lock(); activeLogin = previous; loginMu.Unlock() }()

	decode := func(t *testing.T, out []byte) (int, string) {
		t.Helper()
		var env struct {
			Result struct {
				StatusCode int    `json:"StatusCode"`
				Body       []byte `json:"Body"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("envelope decode: %v", err)
		}
		return env.Result.StatusCode, string(env.Result.Body)
	}

	// A plain visit renders the form.
	// The host reaches the panel through management.handle, so that is the entry
	// point the test exercises.
	out, err := handleManagementHandle([]byte(`{"Method":"GET","Path":"/panel"}`))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	status, page := decode(t, out)
	if status != 200 || !strings.Contains(page, "redirect_url") || !strings.Contains(page, "完成登录") {
		t.Fatalf("GET panel = %d, page missing the form: %s", status, page[:min(200, len(page))])
	}

	// A form submission reaches the completion path; with no flow in flight the
	// error has to be about the flow, which proves the paste was parsed.
	body := []byte("redirect_url=" + url.QueryEscape("https://prism.openai.com/auth/popup-callback?code=ac_PANEL&state=prism_openai_oauth_state.v1.x.y"))
	out, err = handlePanel([]byte(`{"Method":"POST","Body":"` + base64.StdEncoding.EncodeToString(body) + `","Headers":{"Content-Type":["application/x-www-form-urlencoded"]}}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	status, page = decode(t, out)
	// The outcome is rendered in the page with a 200; the host turns a plugin 4xx
	// into a bare 502 and drops the body, which would hide the reason.
	if status != 200 || !strings.Contains(page, "登录流程") {
		t.Fatalf("POST panel = %d, want a 400 about the pending flow: %s", status, page)
	}

	// Submitting nothing explains what is missing instead of failing silently.
	out, _ = handlePanel([]byte(`{"Method":"POST","Body":""}`))
	status, page = decode(t, out)
	if status != 200 || !strings.Contains(page, "粘贴") {
		t.Fatalf("empty POST panel = %d, want guidance: %s", status, page)
	}
}
