package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	clientToolCallOpen    = "<tool_call>"
	clientToolCallClose   = "</tool_call>"
	clientToolResultOpen  = "<tool_result>"
	clientToolResultClose = "</tool_result>"
)

type clientTool struct {
	Name        string
	Kind        string // function | custom
	Namespace   string
	Description string
	Parameters  any
	Format      any
}

type clientToolCall struct {
	ID        string
	Name      string
	Kind      string // function | custom
	Arguments string // function: JSON object string; custom: raw input
}

type parsedClientTools struct {
	Calls     []clientToolCall
	Clean     string
	Malformed []string
}

var fencedToolCallRE = regexp.MustCompile(`(?s)^\x60\x60\x60[[:alnum:]_-]*[ \t]*\r?\n(.*?)\r?\n?\x60\x60\x60$`)

// normalizeClientTools accepts Chat Completions, flat Responses and Codex's
// additional_tools/namespace shapes. Unsupported server-side tools (MCP,
// web_search, file_search...) are deliberately ignored: the plugin cannot
// execute them, while function/custom calls are handed back to the client.
func normalizeClientTools(raw []any) []clientTool {
	out := make([]clientTool, 0)
	used := map[string]struct{}{}
	var walk func([]any, string)
	walk = func(list []any, namespace string) {
		for _, value := range list {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			typeName, _ := item["type"].(string)
			if typeName == "namespace" {
				ns, _ := item["name"].(string)
				if nested, ok := item["tools"].([]any); ok {
					walk(nested, firstNonEmpty(ns, namespace))
				}
				continue
			}
			if typeName != "function" && typeName != "custom" {
				continue
			}

			definition := item
			if nested, ok := item["function"].(map[string]any); ok {
				definition = nested
			}
			name, _ := definition["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, exists := used[name]; exists && namespace != "" {
				name = namespace + "__" + name
			}
			if _, exists := used[name]; exists {
				continue
			}
			used[name] = struct{}{}
			description, _ := definition["description"].(string)
			kind := "function"
			if typeName == "custom" {
				kind = "custom"
			}
			parameters := definition["parameters"]
			if parameters == nil && kind == "function" {
				parameters = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out = append(out, clientTool{
				Name: name, Kind: kind, Namespace: namespace,
				Description: description, Parameters: parameters, Format: definition["format"],
			})
		}
	}
	walk(raw, "")
	return out
}

func rawToolValues(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}
	var values []any
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

func buildClientToolsPreamble(tools []clientTool, toolChoice any) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【客户端工具调用约定】\n\n")
	b.WriteString("你不能直接执行下面这些工具。需要使用时，只输出工具调用标记，由用户本机的客户端执行；结果会在下一轮以 <tool_result> 返回。\n\n")
	b.WriteString("函数工具格式：\n  <tool_call>{\"name\":\"工具名\",\"arguments\":{...}}</tool_call>\n")
	b.WriteString("自由文本工具格式：\n  <tool_call>{\"name\":\"工具名\",\"input\":\"原始文本\"}</tool_call>\n\n")
	b.WriteString("规则：\n1. 需要工具时只输出调用标记，不要用代码块包裹。\n2. 不要声称无法访问本地文件；应请求客户端工具。\n3. 看到 <tool_result> 后直接使用结果，不要重复请求同一工具。\n4. 一轮可以请求多个工具，每个标记独占一行。\n")
	if force := toolChoiceInstruction(toolChoice); force != "" {
		b.WriteString("5. ")
		b.WriteString(force)
		b.WriteByte('\n')
	}
	b.WriteString("\n可用工具：\n")
	for _, tool := range tools {
		b.WriteString("- ")
		b.WriteString(tool.Name)
		if tool.Namespace != "" {
			b.WriteString("（分组 ")
			b.WriteString(tool.Namespace)
			b.WriteString("）")
		}
		if tool.Kind == "custom" {
			b.WriteString("：参数是自由文本，必须使用 input")
			if tool.Description != "" {
				b.WriteString("；")
				b.WriteString(tool.Description)
			}
			if tool.Format != nil {
				encoded, _ := json.Marshal(tool.Format)
				b.WriteString("；格式=")
				b.Write(encoded)
			}
			b.WriteByte('\n')
			continue
		}
		if tool.Description != "" {
			b.WriteString("：")
			b.WriteString(tool.Description)
		}
		encoded, _ := json.Marshal(tool.Parameters)
		b.WriteString("；参数 JSON Schema=")
		b.Write(encoded)
		b.WriteByte('\n')
	}
	return b.String()
}

func toolChoiceInstruction(choice any) string {
	switch value := choice.(type) {
	case string:
		if value == "required" || value == "any" {
			return "本轮必须调用至少一个工具，不要直接回答。"
		}
	case map[string]any:
		name, _ := value["name"].(string)
		if fn, ok := value["function"].(map[string]any); ok {
			if nested, _ := fn["name"].(string); nested != "" {
				name = nested
			}
		}
		if name != "" {
			return fmt.Sprintf("本轮必须调用 %q，不要调用其他工具，也不要直接回答。", name)
		}
	}
	return ""
}

// buildClientToolInput flattens history into the final user item because the
// prism agent only reliably reads that item. Oldest history is trimmed first
// when the prompt approaches maxInputBytes; the preamble and current request
// always remain.
func buildClientToolInput(messages []chatMessage, tools []clientTool, choice any) ([]map[string]any, error) {
	preamble := buildClientToolsPreamble(tools, choice)
	turns := make([]string, 0, len(messages))
	lastUser := -1
	for _, message := range messages {
		text := strings.TrimSpace(flattenContent(message.Content))
		if message.Role == "assistant" && len(message.ToolCalls) > 0 {
			markers := make([]string, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				kind := call.Kind
				if kind == "" {
					kind = "function"
				}
				markers = append(markers, formatClientToolCallMarker(call.Function.Name, kind, call.Function.Arguments))
			}
			if text != "" {
				text += "\n"
			}
			text += strings.Join(markers, "\n")
		}
		if message.Role == "tool" {
			if !strings.Contains(text, clientToolResultOpen) {
				text = formatClientToolResult(message.ToolCallID, text)
			}
		}
		if text == "" {
			continue
		}
		label := "用户"
		switch message.Role {
		case "system", "developer":
			label = "系统"
		case "assistant":
			label = "助手"
		case "tool":
			label = "工具结果"
			lastUser = len(turns)
		case "user":
			lastUser = len(turns)
		}
		turns = append(turns, label+"："+text)
	}
	if lastUser < 0 && len(turns) > 0 {
		lastUser = len(turns) - 1
	}
	current := "请根据以上工具约定继续处理当前任务。"
	if lastUser >= 0 {
		current = strings.TrimPrefix(turns[lastUser], "用户：")
		current = strings.TrimPrefix(current, "工具结果：")
		turns = append(turns[:lastUser], turns[lastUser+1:]...)
	}

	prefix := preamble + "\n\n"
	const header = "以下是此前上下文（较旧内容可能已裁剪）：\n\n"
	const currentHeader = "\n\n当前请求：\n"
	budget := maxInputBytes - len(prefix) - len(header) - len(currentHeader) - len(current) - 512
	if budget < 0 {
		return nil, fmt.Errorf("工具定义和当前请求共 %d 字节，超过 prism 上限", len(prefix)+len(current))
	}
	selected := make([]string, 0, len(turns))
	used := 0
	for i := len(turns) - 1; i >= 0; i-- {
		cost := len(turns[i]) + 2
		if used+cost > budget {
			continue
		}
		selected = append(selected, turns[i])
		used += cost
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	text := prefix
	if len(selected) > 0 {
		text += header + strings.Join(selected, "\n\n")
	}
	text += currentHeader + current
	return []map[string]any{{
		"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": text}},
	}}, nil
}

func parseClientToolCalls(text string, allowed []clientTool) parsedClientTools {
	allowedByName := make(map[string]clientTool, len(allowed))
	for _, tool := range allowed {
		allowedByName[tool.Name] = tool
	}
	result := parsedClientTools{}
	closedRE := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(clientToolCallOpen) + `\s*(.*?)\s*` + regexp.QuoteMeta(clientToolCallClose))
	for _, match := range closedRE.FindAllStringSubmatch(text, -1) {
		call, ok := parseOneClientToolCall(match[1], allowedByName)
		if !ok {
			result.Malformed = append(result.Malformed, truncateString(match[1], 240))
			continue
		}
		result.Calls = append(result.Calls, call)
	}

	// Accept one parseable unclosed marker at the end; truncated JSON remains
	// malformed so a bounded correction round can ask the model to rewrite it.
	if index := strings.LastIndex(text, clientToolCallOpen); index >= 0 &&
		!strings.Contains(text[index+len(clientToolCallOpen):], clientToolCallClose) {
		tail := text[index+len(clientToolCallOpen):]
		if call, ok := parseOneClientToolCall(tail, allowedByName); ok {
			result.Calls = append(result.Calls, call)
		} else if strings.TrimSpace(tail) != "" {
			result.Malformed = append(result.Malformed, truncateString(tail, 240))
		}
	}

	clean := closedRE.ReplaceAllString(text, "")
	if index := strings.LastIndex(clean, clientToolCallOpen); index >= 0 {
		tail := clean[index+len(clientToolCallOpen):]
		if _, ok := parseOneClientToolCall(tail, allowedByName); ok || len(result.Malformed) > 0 {
			clean = clean[:index]
		}
	}
	result.Clean = strings.TrimSpace(regexp.MustCompile(`\n{3,}`).ReplaceAllString(clean, "\n\n"))
	return result
}

func parseOneClientToolCall(body string, allowed map[string]clientTool) (clientToolCall, bool) {
	body = strings.TrimSpace(body)
	if match := fencedToolCallRE.FindStringSubmatch(body); len(match) == 2 {
		body = strings.TrimSpace(match[1])
	}
	var object map[string]any
	if json.Unmarshal([]byte(body), &object) != nil {
		return clientToolCall{}, false
	}
	definition := object
	if nested, ok := object["function"].(map[string]any); ok {
		definition = nested
	}
	name, _ := definition["name"].(string)
	if name == "" {
		name, _ = object["tool"].(string)
	}
	tool, exists := allowed[name]
	if !exists {
		return clientToolCall{}, false
	}

	if input, exists := object["input"]; exists {
		return clientToolCall{Name: name, Kind: "custom", Arguments: stringifyToolInput(input)}, true
	}
	arguments := definition["arguments"]
	if arguments == nil {
		arguments = object["arguments"]
	}
	if arguments == nil {
		arguments = object["args"]
	}
	if arguments == nil {
		arguments = object["parameters"]
	}
	if raw, ok := arguments.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(raw), &decoded) == nil {
			encoded, _ := json.Marshal(decoded)
			return clientToolCall{Name: name, Kind: "function", Arguments: string(encoded)}, true
		}
		// Models frequently put custom free-form input in `arguments` despite
		// being told to use `input`.
		if tool.Kind == "custom" {
			return clientToolCall{Name: name, Kind: "custom", Arguments: raw}, true
		}
		return clientToolCall{}, false
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return clientToolCall{}, false
	}
	kind := tool.Kind
	if kind == "custom" {
		return clientToolCall{Name: name, Kind: kind, Arguments: stringifyToolInput(arguments)}, true
	}
	return clientToolCall{Name: name, Kind: "function", Arguments: string(encoded)}, true
}

func assignClientToolCallIDs(calls []clientToolCall, responseID string) []clientToolCall {
	out := make([]clientToolCall, len(calls))
	for i, call := range calls {
		call.ID = stableToolCallID(responseID, call, i)
		out[i] = call
	}
	return out
}

func stableToolCallID(responseID string, call clientToolCall, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", responseID, index, call.Name, call.Arguments)))
	return "call_" + hex.EncodeToString(sum[:12])
}

func formatClientToolCallMarker(name, kind, payload string) string {
	object := map[string]any{"name": name}
	if kind == "custom" {
		object["input"] = payload
	} else {
		var arguments any
		if json.Unmarshal([]byte(payload), &arguments) != nil {
			arguments = map[string]any{"__raw": payload}
		}
		object["arguments"] = arguments
	}
	raw, _ := json.Marshal(object)
	return clientToolCallOpen + string(raw) + clientToolCallClose
}

func formatClientToolResult(callID, output string) string {
	raw, _ := json.Marshal(map[string]any{"call_id": callID, "content": output})
	return clientToolResultOpen + string(raw) + clientToolResultClose
}

func stringifyToolInput(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func truncateString(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "…"
}

func sortedToolNames(tools []clientTool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}
