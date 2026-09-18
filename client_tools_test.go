package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func fixtureClientTools() []clientTool {
	return []clientTool{
		{Name: "read_file", Kind: "function", Parameters: map[string]any{"type": "object"}},
		{Name: "exec", Kind: "custom"},
	}
}

func TestNormalizeClientToolsSupportsChatResponsesAndNamespaces(t *testing.T) {
	raw := []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "description": "read", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "name": "write_file", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "namespace", "name": "functions", "tools": []any{
			map[string]any{"type": "custom", "name": "exec", "description": "run"},
			map[string]any{"type": "function", "name": "wait", "parameters": map[string]any{"type": "object"}},
		}},
		map[string]any{"type": "mcp", "name": "not-supported"},
	}
	tools := normalizeClientTools(raw)
	if got := strings.Join(sortedToolNames(tools), ","); got != "exec,read_file,wait,write_file" {
		t.Fatalf("tool names = %q", got)
	}
	var exec clientTool
	for _, tool := range tools {
		if tool.Name == "exec" {
			exec = tool
		}
	}
	if exec.Kind != "custom" || exec.Namespace != "functions" {
		t.Fatalf("exec = %#v", exec)
	}
}

func TestBuildClientToolsPreambleNamesToolsAndChoice(t *testing.T) {
	preamble := buildClientToolsPreamble(fixtureClientTools(), "required")
	for _, want := range []string{"read_file", "exec", "必须调用至少一个工具", clientToolCallOpen, clientToolResultOpen} {
		if !strings.Contains(preamble, want) {
			t.Errorf("preamble missing %q", want)
		}
	}
}

func TestParseClientToolCallsAcceptsKnownVariants(t *testing.T) {
	text := strings.Join([]string{
		`before`,
		`<tool_call>{"name":"read_file","arguments":{"path":"a.txt"}}</tool_call>`,
		"<tool_call>\n```json\n{\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"b.txt\\\"}\"}}\n```\n</tool_call>",
		`<tool_call>{"name":"exec","arguments":"const x = 1;"}</tool_call>`,
	}, "\n")
	parsed := parseClientToolCalls(text, fixtureClientTools())
	if len(parsed.Calls) != 3 || len(parsed.Malformed) != 0 {
		t.Fatalf("parsed = %#v", parsed)
	}
	if parsed.Calls[0].Kind != "function" || parsed.Calls[0].Arguments != `{"path":"a.txt"}` {
		t.Errorf("first = %#v", parsed.Calls[0])
	}
	if parsed.Calls[2].Kind != "custom" || parsed.Calls[2].Arguments != "const x = 1;" {
		t.Errorf("custom = %#v", parsed.Calls[2])
	}
	if parsed.Clean != "before" {
		t.Errorf("clean = %q", parsed.Clean)
	}
}

func TestParseClientToolCallsRejectsUndeclaredToolAndMalformedJSON(t *testing.T) {
	parsed := parseClientToolCalls(
		`<tool_call>{"name":"delete_everything","arguments":{}}</tool_call>`+"\n"+
			`<tool_call>{bad json}</tool_call>`,
		fixtureClientTools(),
	)
	if len(parsed.Calls) != 0 || len(parsed.Malformed) != 2 {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestAssignClientToolCallIDsIsStable(t *testing.T) {
	calls := []clientToolCall{{Name: "read_file", Kind: "function", Arguments: `{"path":"a"}`}}
	a := assignClientToolCallIDs(calls, "resp-1")
	b := assignClientToolCallIDs(calls, "resp-1")
	if a[0].ID == "" || a[0].ID != b[0].ID {
		t.Fatalf("ids = %q / %q", a[0].ID, b[0].ID)
	}
}

func TestBuildClientToolInputRewritesToolRoundtrip(t *testing.T) {
	var assistant chatMessage
	assistant.Role = "assistant"
	assistant.ToolCalls = append(assistant.ToolCalls, struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Kind     string `json:"kind,omitempty"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}{ID: "call-1", Type: "function"})
	assistant.ToolCalls[0].Function.Name = "read_file"
	assistant.ToolCalls[0].Function.Arguments = `{"path":"a.txt"}`
	toolContent, _ := json.Marshal("hello from local file")
	input, err := buildClientToolInput([]chatMessage{
		{Role: "user", Content: json.RawMessage(`"read a.txt"`)},
		assistant,
		{Role: "tool", ToolCallID: "call-1", Content: toolContent},
	}, fixtureClientTools(), nil)
	if err != nil {
		t.Fatal(err)
	}
	content := input[0]["content"].([]map[string]any)[0]["text"].(string)
	for _, want := range []string{clientToolCallOpen, clientToolResultOpen, "hello from local file", "read_file"} {
		if !strings.Contains(content, want) {
			t.Errorf("input missing %q: %s", want, content)
		}
	}
}

func TestResponsesToolCallOutputLifecycle(t *testing.T) {
	calls := assignClientToolCallIDs([]clientToolCall{
		{Name: "read_file", Kind: "function", Arguments: `{"path":"a.txt"}`},
		{Name: "exec", Kind: "custom", Arguments: `await tools.exec_command({cmd:"pwd"})`},
	}, "resp-tool")
	body := responsesCompletion("gpt-5.6-sol", "resp-tool", "", calls)
	for _, want := range [][]byte{[]byte(`"type":"function_call"`), []byte(`"type":"custom_tool_call"`), []byte(`"call_id"`)} {
		if !bytes.Contains(body, want) {
			t.Errorf("completion missing %q: %s", want, body)
		}
	}
	chunks := responsesSSEChunks("gpt-5.6-sol", "resp-tool", "", calls)
	joined := bytes.Join(func() [][]byte {
		out := make([][]byte, len(chunks))
		for i, chunk := range chunks {
			out[i] = chunk.Payload
		}
		return out
	}(), nil)
	for _, event := range []string{
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.output_item.done",
		"response.completed",
	} {
		if !bytes.Contains(joined, []byte("event: "+event)) {
			t.Errorf("missing event %s", event)
		}
	}
	if bytes.Contains(joined, []byte("response.output_text.delta")) {
		t.Error("pure tool response unexpectedly emitted an output_text item")
	}
}

func TestChatToolCallOutput(t *testing.T) {
	calls := assignClientToolCallIDs([]clientToolCall{{Name: "read_file", Kind: "function", Arguments: `{"path":"a.txt"}`}}, "chat-tool")
	body := completion("gpt-5.6-sol", "chat-tool", "", calls)
	if !bytes.Contains(body, []byte(`"finish_reason":"tool_calls"`)) || !bytes.Contains(body, []byte(`"tool_calls"`)) {
		t.Fatalf("completion = %s", body)
	}
	chunks := sseChunks("gpt-5.6-sol", "chat-tool", "", calls)
	joined := bytes.Join(func() [][]byte {
		out := make([][]byte, len(chunks))
		for i, chunk := range chunks {
			out[i] = chunk.Payload
		}
		return out
	}(), nil)
	if !bytes.Contains(joined, []byte(`"finish_reason":"tool_calls"`)) || !bytes.Contains(joined, []byte(`"tool_calls"`)) {
		t.Fatalf("chunks = %s", joined)
	}
}
