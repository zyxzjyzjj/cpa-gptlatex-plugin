package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// handleExecute services executor.execute / executor.execute_stream. Prism has
// no upstream SSE: a turn is start + poll. A streaming call uses CPA's async
// stream bridge so a valid first event is returned immediately instead of
// making Codex wait 30+ seconds with no bytes and retry the same turn.
func handleExecute(raw []byte, stream bool) ([]byte, error) {
	if !stream {
		return handleExecuteSync(raw, false)
	}
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", "无法解析 executor 请求: "+err.Error(), http.StatusBadRequest), nil
	}
	if strings.TrimSpace(req.StreamID) == "" {
		// Compatibility fallback for older hosts/harnesses without the bridge.
		return handleExecuteSync(raw, true)
	}
	return startAsyncExecute(raw, req)
}

// handleExecuteSync is the complete turn implementation shared by ordinary
// calls and the background worker behind async streaming.
func handleExecuteSync(raw []byte, stream bool) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", "无法解析 executor 请求: "+err.Error(), http.StatusBadRequest), nil
	}

	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}
	client, err := newPrismClient(sa)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := client.hydrate(ctx, sa); err != nil {
		return errorEnvelope("authentication_error", err.Error(), http.StatusUnauthorized), nil
	}

	var chat *chatRequest
	if isResponsesFormat(req.Format) {
		chat, err = decodeResponsesRequest(req.Payload)
	} else {
		chat, err = decodeChatRequest(req.Payload)
	}
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}
	model, err := resolveModel(ctx, client, req.Model, chat.Model)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}

	cfg := currentConfig()
	clientToolMode := cfg.ClientTools && len(chat.ClientTools) > 0
	var input []map[string]any
	if clientToolMode {
		input, err = buildClientToolInput(chat.Messages, chat.ClientTools, chat.ToolChoice)
	} else {
		input, err = buildInput(chat.Messages)
	}
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}

	// The host log is the only place an operator can see which model prism was
	// actually asked for; the client-facing error cannot say it.
	started := time.Now()
	hostLog("info", fmt.Sprintf("开始 prism 回合 model=%s 消息=%d 输入项=%d stream=%t 客户端工具=%d 请求模型=%s",
		model, len(chat.Messages), len(input), stream, len(chat.ClientTools), req.Model), map[string]any{"model": model})

	projectID, err := client.ensureProject(ctx)
	if err != nil {
		hostLog("error", "创建/复用 prism 项目失败", map[string]any{"error": err.Error()})
		return errorEnvelope("server_error", err.Error(), http.StatusBadGateway), nil
	}

	// 冷启动要付沙箱握手（实测 42s：资源令牌 → y-sweet → Yjs 同步），
	// 缓存命中后同一轮只要几秒。所以这里的耗时按阶段打点，便于判断慢在哪。
	sandboxMS := int64(0)
	metadata := map[string]any{
		"projectId":        projectID,
		"userId":           client.userID,
		"model":            model,
		"reasoning_effort": cfg.ReasoningEffort,
		"frontend_origin":  baseURL,
	}
	// 沙箱凭证必须放进 metadata，服务端 Agent 才有工具执行环境（读写 .tex、编译 PDF）。
	// 实测 metadata.sandbox_url 用公开地址即可，服务端在 turn_state 里会换成内部集群地址。
	attachSandbox := func() error {
		mark := time.Now()
		sb, reused, sbErr := client.ensureSandbox(ctx, projectID)
		if sbErr != nil {
			hostLog("error", fmt.Sprintf("prism 沙箱领取失败 已耗时=%dms", time.Since(started).Milliseconds()),
				map[string]any{"error": sbErr.Error()})
			return sbErr
		}
		metadata["sandbox_url"] = sb.URL
		metadata["sandbox_token"] = sb.Token
		took := time.Since(mark).Milliseconds()
		sandboxMS += took
		// The host renders only known field names, so the numbers go in the
		// message; 复用=true means no handshake was paid this time.
		hostLog("info", fmt.Sprintf("prism 沙箱就绪 复用=%t 本次=%dms 累计=%dms",
			reused, took, sandboxMS), nil)
		return nil
	}
	if cfg.EnableSandbox {
		if err := attachSandbox(); err != nil {
			return errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway), nil
		}
	}

	turn := turnRequest{Input: input, Metadata: metadata}
	maxToolRounds := 1
	if clientToolMode && cfg.ClientToolsMaxRounds > 1 {
		maxToolRounds = cfg.ClientToolsMaxRounds
	}
	var payload *turnPayload
	var toolResult parsedClientTools
	for round := 1; round <= maxToolRounds; round++ {
		payload, err = client.runTurn(ctx, turn, 5*time.Second, func() {
			// 服务端要求重连时缓存的沙箱已经没用了。丢掉之后必须重新领一个并
			// 覆盖 metadata，否则重试还是带着同一个死沙箱，只会再失败一次。
			if !cfg.EnableSandbox {
				return
			}
			dropSandbox(projectID)
			if err := attachSandbox(); err != nil {
				hostLog("error", "重连时重新领取 prism 沙箱失败", map[string]any{"error": err.Error()})
			}
		})
		if err != nil || !clientToolMode {
			break
		}
		toolResult = parseClientToolCalls(payload.Text(), chat.ClientTools)
		if len(toolResult.Calls) > 0 || len(toolResult.Malformed) == 0 || round >= maxToolRounds {
			break
		}
		// Model emitted a marker but malformed its JSON. Give it one bounded
		// correction round; no tool is executed here. Repeat the full preamble:
		// prism reliably reads only the final user item.
		correction := fmt.Sprintf("%s\n\n上一次工具调用不是合法 JSON：%s\n请只重新输出一行格式正确的 %s{\\\"name\\\":...,\\\"arguments\\\":{...}}%s",
			buildClientToolsPreamble(chat.ClientTools, chat.ToolChoice),
			truncateString(toolResult.Malformed[0], 240), clientToolCallOpen, clientToolCallClose)
		turn.Input = append(turn.Input,
			map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": payload.Text()}}},
			map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": correction}}},
		)
		hostLog("warn", fmt.Sprintf("客户端工具标记 JSON 非法，纠正重试 round=%d", round), nil)
	}
	if err != nil {
		total := time.Since(started).Milliseconds()
		// Only `model`/`reason`/`error` survive the host's field filter, so the
		// model name, the phase timings and prism's own reason/messageKey all go
		// into the message.
		detail := fmt.Sprintf("prism 回合失败 model=%s 沙箱=%dms 回合=%dms 合计=%dms",
			model, sandboxMS, total-sandboxMS, total)
		if pErr, ok := err.(*prismError); ok {
			detail += fmt.Sprintf(" reason=%s messageKey=%s rootCause=%s upstreamStatus=%d",
				pErr.Reason, pErr.MessageKey, pErr.RootCause, pErr.Status)
		}
		hostLog("error", detail+" error="+err.Error(), map[string]any{
			"model": model,
			"error": err.Error(),
		})

		// 传输层/沙箱类失败（而非服务端明确返回的业务错误）说明缓存的沙箱不可用，
		// 丢掉它，下次请求重新领取。
		if cfg.EnableSandbox {
			if _, isBusiness := err.(*prismError); !isBusiness {
				dropSandbox(projectID)
			}
		}
		if pErr, ok := err.(*prismError); ok {
			return errorEnvelope("upstream_error", pErr.Error(), pErr.HTTPStatus()), nil
		}
		return errorEnvelope("upstream_error", err.Error(), http.StatusBadGateway), nil
	}

	text := payload.Text()
	var clientCalls []clientToolCall
	if clientToolMode {
		if len(toolResult.Calls) == 0 && len(toolResult.Malformed) == 0 {
			toolResult = parseClientToolCalls(text, chat.ClientTools)
		}
		text = toolResult.Clean
		clientCalls = assignClientToolCallIDs(toolResult.Calls, firstNonEmpty(payload.ID, streamRequestID(req)))
		if len(toolResult.Malformed) > 0 && len(clientCalls) == 0 {
			hostLog("warn", "客户端工具标记无法解析，已从正文移除", nil)
		}
	}
	if strings.TrimSpace(text) == "" && len(clientCalls) == 0 {
		text = "（prism 未返回可显示的文本）"
	}

	// The host only renders a fixed set of field names (model, error, reason…),
	// so anything an operator needs to see has to be in the message itself —
	// fields such as elapsed_ms are dropped on the way to the log file.
	total := time.Since(started).Milliseconds()
	hostLog("info", fmt.Sprintf("prism 回合完成 model=%s 沙箱=%dms 回合=%dms 合计=%dms 字数=%d",
		model, sandboxMS, total-sandboxMS, total, len(text)), map[string]any{
		"model": model,
	})

	outputID := payload.ID
	if value, ok := req.Metadata["prism_output_id"].(string); ok && strings.TrimSpace(value) != "" {
		outputID = value
	}
	if isResponsesFormat(req.Format) {
		if stream {
			chunks := responsesSSEChunks(model, outputID, text, clientCalls)
			started, _ := req.Metadata["prism_started_emitted"].(bool)
			if !started {
				created := time.Now().Unix()
				chunks = append([]streamChunk{{Payload: responsesStartedFrame(model, outputID, created)}}, chunks...)
			}
			return okEnvelope(streamResponse{
				Headers: http.Header{
					"Content-Type":  []string{"text/event-stream"},
					"Cache-Control": []string{"no-cache"},
				},
				Chunks: chunks,
			})
		}
		return okEnvelope(executorResponse{
			Payload: responsesCompletion(model, outputID, text, clientCalls),
			Headers: http.Header{"Content-Type": []string{"application/json"}},
		})
	}

	// Chat Completions stays available for /v1/chat/completions clients.
	if stream {
		return okEnvelope(streamResponse{
			Headers: http.Header{
				"Content-Type":  []string{"text/event-stream"},
				"Cache-Control": []string{"no-cache"},
			},
			Chunks: sseChunks(model, outputID, text, clientCalls),
		})
	}
	return okEnvelope(executorResponse{
		Payload: completion(model, outputID, text, clientCalls),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// sharedExecution is one upstream turn shared by identical downstream retries.
// Codex retries after a 30s idle wait; without this, four byte-identical 1.5MB
// requests observed in production started four separate prism turns.
type sharedExecution struct {
	done   chan struct{}
	result []byte
}

var (
	executionMu    sync.Mutex
	executionCache = map[string]*sharedExecution{}
)

func getSharedExecution(key string, run func() []byte) *sharedExecution {
	executionMu.Lock()
	if existing := executionCache[key]; existing != nil {
		executionMu.Unlock()
		return existing
	}
	entry := &sharedExecution{done: make(chan struct{})}
	executionCache[key] = entry
	executionMu.Unlock()
	go func() {
		entry.result = run()
		close(entry.done)
		// Keep the completed result briefly so an immediate retry receives the
		// same answer rather than starting another turn.
		time.AfterFunc(2*time.Minute, func() {
			executionMu.Lock()
			if executionCache[key] == entry {
				delete(executionCache, key)
			}
			executionMu.Unlock()
		})
	}()
	return entry
}

// startAsyncExecute emits one valid logical event immediately, which commits
// the HTTP response before Codex's ~30s idle deadline. Work continues in the
// background; keepalives every 10s keep the connection active.
func startAsyncExecute(raw []byte, req executorRequest) ([]byte, error) {
	streamID := strings.TrimSpace(req.StreamID)
	model := requestedModelName(req.Model)
	id := "prism-" + streamRequestID(req)
	created := time.Now().Unix()
	responsesOutput := isResponsesFormat(req.Format)

	var first []byte
	if responsesOutput {
		first = responsesStartedFrame(model, id, created)
	} else {
		first = chatStreamFrame(model, id, created,
			map[string]any{"role": "assistant", "content": ""}, nil)
	}
	if err := emitHostStream(streamID, first); err != nil {
		return errorEnvelope("stream_error", "无法发送首个流事件: "+err.Error(), http.StatusBadGateway), nil
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	req.Metadata["prism_output_id"] = id
	req.Metadata["prism_started_emitted"] = true
	workerRaw, _ := json.Marshal(req)

	key := streamRequestID(req)
	shared := getSharedExecution(key, func() []byte {
		out, _ := handleExecuteSync(workerRaw, true)
		return out
	})

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-shared.done:
				out := shared.result
				var env envelope
				if err := json.Unmarshal(out, &env); err != nil {
					closeHostStream(streamID, "无法解析插件流结果: "+err.Error())
					return
				}
				if !env.OK || env.Error != nil {
					message := "prism 执行失败"
					if env.Error != nil && env.Error.Message != "" {
						message = env.Error.Message
					}
					closeHostStream(streamID, message)
					return
				}
				var response streamResponse
				if err := json.Unmarshal(env.Result, &response); err != nil {
					closeHostStream(streamID, "无法解析插件流数据: "+err.Error())
					return
				}
				for _, chunk := range response.Chunks {
					if err := emitHostStream(streamID, chunk.Payload); err != nil {
						closeHostStream(streamID, err.Error())
						return
					}
				}
				closeHostStream(streamID, "")
				return
			case <-ticker.C:
				var heartbeat []byte
				if responsesOutput {
					// SSE comments are valid and the Responses validator preserves them.
					heartbeat = []byte(": prism keepalive\n\n")
				} else {
					heartbeat = chatStreamFrame(model, id, created, map[string]any{}, nil)
				}
				if err := emitHostStream(streamID, heartbeat); err != nil {
					closeHostStream(streamID, err.Error())
					return
				}
			}
		}
	}()

	// Empty inline chunks keep the async bridge open (rpc_client_stream.go).
	return okEnvelope(streamResponse{
		Headers: http.Header{
			"Content-Type":  []string{"text/event-stream"},
			"Cache-Control": []string{"no-cache"},
		},
	})
}

func requestedModelName(raw string) string {
	name := strings.TrimSpace(raw)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return fallbackModel
	}
	return name
}

func streamRequestID(req executorRequest) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(strings.TrimSpace(req.Model)))
	_, _ = hash.Write(req.StorageJSON)
	_, _ = hash.Write(req.OriginalRequest)
	_, _ = hash.Write(req.Payload)
	// StreamID/HostCallbackID are intentionally excluded: they identify one
	// downstream attempt, while retries of the same semantic turn must share.
	return hex.EncodeToString(hash.Sum(nil)[:16])
}

func chatStreamFrame(model, id string, created int64, delta map[string]any, finish any) []byte {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
	raw, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion.chunk",
		"created": created, "model": model, "choices": []map[string]any{choice},
	})
	return []byte("data: " + string(raw) + "\n\n")
}

func emitHostStream(streamID string, payload []byte) error {
	raw, err := json.Marshal(hostStreamEmitRequest{StreamID: streamID, Payload: payload})
	if err != nil {
		return err
	}
	return callHostJSON(methodHostStreamEmit, raw, nil)
}

func closeHostStream(streamID, message string) {
	raw, err := json.Marshal(hostStreamCloseRequest{StreamID: streamID, Error: message})
	if err == nil {
		_ = callHostJSON(methodHostStreamClose, raw, nil)
	}
}

// handleCountTokens answers executor.count_tokens, which the host expects as a
// full ExecutorResponse ({"Payload": <base64>}) — a bare object is rejected.
//
// Prism reports no usage for a turn and exposes no tokenizer, so this is a stub
// that mirrors CPA's own reference executor plugin: total_tokens is 0 rather
// than a fabricated estimate.
func handleCountTokens(_ []byte) ([]byte, error) {
	body, err := json.Marshal(countTokensBody{TotalTokens: 0})
	if err != nil {
		return nil, err
	}
	return okEnvelope(executorResponse{
		Payload: body,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// hydrate fills in the user id before the first turn, since metadata.userId must
// be the OpenAI handle, not the user UUID. A user id already known from the
// credential file or config short-circuits the /auth/session round trip, so in
// that case the cookie is only validated by the upstream turn itself.
func (c *prismClient) hydrate(ctx context.Context, sa *storedAuth) error {
	if c.userID != "" {
		return nil
	}
	session, err := c.session()
	if err != nil {
		return err
	}
	if session.UserTier != "logged_in" || session.User.ID == "" {
		return fmt.Errorf("prism 会话未登录，cookie 可能已失效")
	}
	id := session.OpenAIUserID()
	if id == "" {
		return fmt.Errorf("无法从 /auth/session 解析 OpenAI user id")
	}
	c.userID = id
	rememberUserID(sa, id)
	return nil
}

// ------------------------------------------------------------ chat mapping

type chatRequest struct {
	Model       string          `json:"model"`
	Messages    []chatMessage   `json:"messages"`
	Stream      bool            `json:"stream"`
	RawTools    json.RawMessage `json:"tools"`
	ToolChoice  any             `json:"tool_choice"`
	ClientTools []clientTool    `json:"-"`
}

type responsesRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Input        json.RawMessage `json:"input"`
	Stream       bool            `json:"stream"`
	RawTools     json.RawMessage `json:"tools"`
	ToolChoice   any             `json:"tool_choice"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Kind     string `json:"kind,omitempty"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
}

func decodeChatRequest(raw []byte) (*chatRequest, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("请求体为空")
	}
	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("无法解析 chat-completions: %w", err)
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages 为空")
	}
	if choice, _ := req.ToolChoice.(string); choice != "none" {
		req.ClientTools = normalizeClientTools(rawToolValues(req.RawTools))
	}
	return &req, nil
}

// decodeResponsesRequest keeps only textual messages. Reasoning, tool calls,
// tool outputs and additional_tools are replay artifacts for Codex; prism's
// web turn endpoint cannot consume them, and converting them through Chat
// Completions produced hundreds of empty/structural messages.
func decodeResponsesRequest(raw []byte) (*chatRequest, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("请求体为空")
	}
	var req responsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("无法解析 responses: %w", err)
	}
	out := &chatRequest{Model: req.Model, Stream: req.Stream, ToolChoice: req.ToolChoice}
	toolValues := rawToolValues(req.RawTools)
	seenInstructions := map[string]struct{}{}
	appendText := func(role, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if role == "system" || role == "developer" {
			if _, exists := seenInstructions[text]; exists {
				return
			}
			seenInstructions[text] = struct{}{}
			role = "system"
		}
		content, _ := json.Marshal(text)
		out.Messages = append(out.Messages, chatMessage{Role: role, Content: content})
	}
	appendText("system", req.Instructions)

	var inputText string
	if json.Unmarshal(req.Input, &inputText) == nil && strings.TrimSpace(inputText) != "" {
		appendText("user", inputText)
		if choice, _ := req.ToolChoice.(string); choice != "none" {
			out.ClientTools = normalizeClientTools(toolValues)
		}
		return out, nil
	}
	var items []struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Tools     []any           `json:"tools"`
		ID        string          `json:"id"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, fmt.Errorf("responses.input 不是字符串或数组: %w", err)
	}
	for _, item := range items {
		itemType := strings.TrimSpace(item.Type)
		switch itemType {
		case "additional_tools":
			toolValues = append(toolValues, item.Tools...)
			continue
		case "function_call", "custom_tool_call":
			kind := "function"
			payload := string(item.Arguments)
			if itemType == "custom_tool_call" {
				kind = "custom"
				var input string
				if json.Unmarshal(item.Input, &input) == nil {
					payload = input
				} else {
					payload = string(item.Input)
				}
			} else {
				var argumentText string
				if json.Unmarshal(item.Arguments, &argumentText) == nil {
					payload = argumentText
				}
			}
			marker := formatClientToolCallMarker(item.Name, kind, payload)
			appendText("assistant", marker)
			continue
		case "function_call_output", "custom_tool_call_output":
			output := strings.TrimSpace(flattenContent(item.Output))
			if output == "" {
				var text string
				if json.Unmarshal(item.Output, &text) == nil {
					output = text
				} else {
					output = strings.TrimSpace(string(item.Output))
				}
			}
			appendText("tool", formatClientToolResult(firstNonEmpty(item.CallID, item.ID), output))
			continue
		case "reasoning", "item_reference", "":
			if itemType != "" {
				continue
			}
		case "message":
		default:
			continue
		}
		role := strings.TrimSpace(item.Role)
		switch role {
		case "system", "developer", "user", "assistant":
		default:
			continue
		}
		text := strings.TrimSpace(flattenContent(item.Content))
		if text == "" {
			continue
		}
		appendText(role, text)
	}
	if choice, _ := req.ToolChoice.(string); choice != "none" {
		out.ClientTools = normalizeClientTools(toolValues)
	}
	if len(out.Messages) == 0 {
		return nil, fmt.Errorf("responses.input 没有可发送给 prism 的文本消息")
	}
	return out, nil
}

// flattenContent accepts both the string form and the multimodal array form of
// `content`, keeping only text.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// buildInput converts chat-completions messages into the Responses-style item
// array that /api/llm/response_with_tools_start expects.
func buildInput(messages []chatMessage) ([]map[string]any, error) {
	cfg := currentConfig()
	out := make([]map[string]any, 0, len(messages)+1)

	hasSystem := false
	for _, m := range messages {
		if m.Role == "system" {
			hasSystem = true
			break
		}
	}
	if !hasSystem {
		system := cfg.SystemPrompt
		if system == "" {
			system = "You are a helpful assistant."
		}
		out = append(out, map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": system}},
		})
	}

	for _, m := range messages {
		text := strings.TrimSpace(flattenContent(m.Content))
		if text == "" {
			// Responses→Chat conversion creates structural assistant/tool
			// messages for tool calls and reasoning. Prism cannot consume those
			// structures, and sending them as empty messages only bloats the
			// request (535 Responses items became 378 chat messages in one live
			// Codex turn). Keep only actual text.
			continue
		}
		switch m.Role {
		case "system":
			out = append(out, map[string]any{
				"type": "message", "role": "system",
				"content": []map[string]any{{"type": "input_text", "text": text}},
			})
		case "assistant":
			out = append(out, map[string]any{
				"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": text}},
			})
		case "user", "developer", "tool":
			out = append(out, map[string]any{
				"type": "message", "role": "user",
				"content": []map[string]any{{"type": "input_text", "text": text}},
			})
		}
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxInputBytes {
		return nil, fmt.Errorf("对话上下文 %d 字节，超过 prism 的 %d 字节上限；请缩短历史消息", len(encoded), maxInputBytes)
	}
	return out, nil
}

// resolveModel picks the model for this turn and refuses names prism does not
// offer. Without that check a stale name reaches the upstream agent, which
// answers with an opaque "Error while processing conversation (400 Bad
// Request)" instead of naming the model as the problem.
//
// When the upstream list cannot be fetched the requested name is passed through
// unchanged: a lookup failure must not break a setup that works.
func resolveModel(ctx context.Context, client *prismClient, requested, bodyModel string) (string, error) {
	cfg := currentConfig()
	name := ""
	for _, candidate := range []string{requested, bodyModel} {
		c := strings.TrimSpace(candidate)
		if c == "" {
			continue
		}
		// Strip a provider prefix such as "prism-provider/gpt-5.6-sol".
		if i := strings.LastIndex(c, "/"); i >= 0 {
			c = c[i+1:]
		}
		name = c
		break
	}

	catalog, _ := client.catalogFor(ctx)
	if name != "" {
		if catalog == nil || knownModel(catalog, cfg.Models, name) {
			return name, nil
		}
		return "", fmt.Errorf("prism 当前不提供模型 %q，可用：%s（模型清单由上游下发，可在插件配置 models 里覆盖）",
			name, strings.Join(catalogIDs(catalog), "、"))
	}
	if cfg.DefaultModel != "" && (catalog == nil || knownModel(catalog, cfg.Models, cfg.DefaultModel)) {
		return cfg.DefaultModel, nil
	}
	if catalog != nil {
		// The web client's own default is the first entry of the list.
		return catalog.Models[0].ID, nil
	}
	if cfg.DefaultModel != "" {
		return cfg.DefaultModel, nil
	}
	return fallbackModel, nil
}

func knownModel(catalog *modelCatalog, configured []string, name string) bool {
	for _, m := range catalog.Models {
		if strings.EqualFold(m.ID, name) {
			return true
		}
	}
	for _, m := range configured {
		if strings.EqualFold(m, name) {
			return true
		}
	}
	return false
}

func catalogIDs(catalog *modelCatalog) []string {
	ids := make([]string, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		ids = append(ids, m.ID)
	}
	return ids
}

// ------------------------------------------------------------- responses

func isResponsesFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "responses", "openai-response", "openai-responses", "openai_responses":
		return true
	default:
		return false
	}
}

func normalizedResponseID(upstreamID string) string {
	id := strings.TrimSpace(upstreamID)
	if id == "" {
		return "resp_prism"
	}
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	return "resp_" + strings.TrimPrefix(id, "chatcmpl-")
}

func responsesCompletion(model, upstreamID, text string, calls []clientToolCall) []byte {
	id := normalizedResponseID(upstreamID)
	messageID := "msg_" + id
	output := make([]map[string]any, 0, 1+len(calls))
	if text != "" || len(calls) == 0 {
		output = append(output, map[string]any{
			"id": messageID, "type": "message", "status": "completed", "role": "assistant",
			"content": []map[string]any{{
				"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": text,
			}},
		})
	}
	for i, call := range calls {
		item := map[string]any{
			"id":      fmt.Sprintf("%s_%d", map[bool]string{true: "ctc", false: "fc"}[call.Kind == "custom"], i),
			"call_id": call.ID, "name": call.Name, "status": "completed",
		}
		if call.Kind == "custom" {
			item["type"] = "custom_tool_call"
			item["input"] = call.Arguments
		} else {
			item["type"] = "function_call"
			item["arguments"] = call.Arguments
		}
		output = append(output, item)
	}
	body := map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "background": false, "error": nil,
		"incomplete_details": nil, "model": model, "output": output,
		"usage": map[string]any{
			"input_tokens": 0, "output_tokens": 0, "total_tokens": 0,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
		},
	}
	raw, _ := json.Marshal(body)
	return raw
}

func responsesSSEEvent(event string, body map[string]any) streamChunk {
	raw, _ := json.Marshal(body)
	return streamChunk{Payload: []byte("event: " + event + "\ndata: " + string(raw) + "\n\n")}
}

// responsesStartedFrame is emitted before prism work starts. It is a complete
// logical Responses event, so CPA commits headers immediately and Codex does
// not abort/retry at ~30s.
func responsesStartedFrame(model, upstreamID string, created int64) []byte {
	id := normalizedResponseID(upstreamID)
	createdBody := map[string]any{
		"type": "response.created", "sequence_number": 1,
		"response": map[string]any{
			"id": id, "object": "response", "created_at": created,
			"status": "in_progress", "background": false, "error": nil,
			"output": []any{}, "model": model,
		},
	}
	inProgress := map[string]any{
		"type": "response.in_progress", "sequence_number": 2,
		"response": map[string]any{
			"id": id, "object": "response", "created_at": created,
			"status": "in_progress", "output": []any{}, "model": model,
		},
	}
	one := responsesSSEEvent("response.created", createdBody).Payload
	two := responsesSSEEvent("response.in_progress", inProgress).Payload
	return append(one, two...)
}

func responsesSSEChunks(model, upstreamID, text string, calls []clientToolCall) []streamChunk {
	id := normalizedResponseID(upstreamID)
	created := time.Now().Unix()
	seq := 3 // 1/2 were emitted by responsesStartedFrame in async mode.
	next := func() int { value := seq; seq++; return value }
	chunks := make([]streamChunk, 0, 7+len(calls)*4)
	output := make([]map[string]any, 0, 1+len(calls))
	outputIndex := 0

	if text != "" || len(calls) == 0 {
		messageID := "msg_" + id
		itemAdded := map[string]any{
			"type": "response.output_item.added", "sequence_number": next(), "output_index": outputIndex,
			"item": map[string]any{"id": messageID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
		}
		part := map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""}
		partAdded := map[string]any{
			"type": "response.content_part.added", "sequence_number": next(),
			"item_id": messageID, "output_index": outputIndex, "content_index": 0, "part": part,
		}
		delta := map[string]any{
			"type": "response.output_text.delta", "sequence_number": next(),
			"item_id": messageID, "output_index": outputIndex, "content_index": 0, "delta": text, "logprobs": []any{},
		}
		textDone := map[string]any{
			"type": "response.output_text.done", "sequence_number": next(),
			"item_id": messageID, "output_index": outputIndex, "content_index": 0, "text": text, "logprobs": []any{},
		}
		finalPart := map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": text}
		partDone := map[string]any{
			"type": "response.content_part.done", "sequence_number": next(),
			"item_id": messageID, "output_index": outputIndex, "content_index": 0, "part": finalPart,
		}
		message := map[string]any{
			"id": messageID, "type": "message", "status": "completed", "role": "assistant",
			"content": []map[string]any{finalPart},
		}
		itemDone := map[string]any{
			"type": "response.output_item.done", "sequence_number": next(), "output_index": outputIndex, "item": message,
		}
		chunks = append(chunks,
			responsesSSEEvent("response.output_item.added", itemAdded),
			responsesSSEEvent("response.content_part.added", partAdded),
			responsesSSEEvent("response.output_text.delta", delta),
			responsesSSEEvent("response.output_text.done", textDone),
			responsesSSEEvent("response.content_part.done", partDone),
			responsesSSEEvent("response.output_item.done", itemDone),
		)
		output = append(output, message)
		outputIndex++
	}

	for i, call := range calls {
		itemID := fmt.Sprintf("fc_%s_%d", id, i)
		itemType := "function_call"
		payloadField := "arguments"
		deltaType := "response.function_call_arguments.delta"
		doneType := "response.function_call_arguments.done"
		if call.Kind == "custom" {
			itemID = fmt.Sprintf("ctc_%s_%d", id, i)
			itemType = "custom_tool_call"
			payloadField = "input"
			deltaType = "response.custom_tool_call_input.delta"
			doneType = "response.custom_tool_call_input.done"
		}
		base := map[string]any{
			"id": itemID, "type": itemType, "call_id": call.ID, "name": call.Name,
		}
		addedItem := cloneAnyMapLocal(base)
		addedItem["status"] = "in_progress"
		addedItem[payloadField] = ""
		chunks = append(chunks, responsesSSEEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "sequence_number": next(), "output_index": outputIndex, "item": addedItem,
		}))
		if call.Arguments != "" {
			chunks = append(chunks, responsesSSEEvent(deltaType, map[string]any{
				"type": deltaType, "sequence_number": next(), "item_id": itemID,
				"output_index": outputIndex, "delta": call.Arguments,
			}))
		}
		chunks = append(chunks, responsesSSEEvent(doneType, map[string]any{
			"type": doneType, "sequence_number": next(), "item_id": itemID,
			"output_index": outputIndex, payloadField: call.Arguments,
		}))
		finalItem := cloneAnyMapLocal(base)
		finalItem["status"] = "completed"
		finalItem[payloadField] = call.Arguments
		chunks = append(chunks, responsesSSEEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": next(), "output_index": outputIndex, "item": finalItem,
		}))
		output = append(output, finalItem)
		outputIndex++
	}

	completedResponse := map[string]any{
		"id": id, "object": "response", "created_at": created, "status": "completed",
		"background": false, "error": nil, "model": model, "output": output,
		"usage": map[string]any{
			"input_tokens": 0, "output_tokens": 0, "total_tokens": 0,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
		},
	}
	chunks = append(chunks, responsesSSEEvent("response.completed", map[string]any{
		"type": "response.completed", "sequence_number": next(), "response": completedResponse,
	}))
	return chunks
}

func cloneAnyMapLocal(source map[string]any) map[string]any {
	out := make(map[string]any, len(source)+2)
	for key, value := range source {
		out[key] = value
	}
	return out
}

func completion(model, upstreamID, text string, calls []clientToolCall) []byte {
	id := upstreamID
	if id == "" {
		id = "prism"
	}
	message := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
		toolCalls := make([]map[string]any, 0, len(calls))
		for i, call := range calls {
			arguments := call.Arguments
			if call.Kind == "custom" {
				raw, _ := json.Marshal(map[string]any{"input": call.Arguments})
				arguments = string(raw)
			}
			toolCalls = append(toolCalls, map[string]any{
				"index": i, "id": call.ID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": arguments},
			})
		}
		message["tool_calls"] = toolCalls
	}
	body := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index": 0, "message": message, "finish_reason": finish,
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
	raw, _ := json.Marshal(body)
	return raw
}

// sseChunks frames the answer as an OpenAI streaming response. Prism delivers
// the whole turn at once, so this is emitted as a short replay rather than a
// live stream.
func sseChunks(model, upstreamID, text string, calls []clientToolCall) []streamChunk {
	id := upstreamID
	if id == "" {
		id = "prism"
	}
	created := time.Now().Unix()
	base := map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion.chunk",
		"created": created, "model": model,
	}
	emit := func(delta map[string]any, finish any) streamChunk {
		frame := cloneAnyMapLocal(base)
		frame["choices"] = []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}
		raw, _ := json.Marshal(frame)
		return streamChunk{Payload: []byte("data: " + string(raw) + "\n\n")}
	}
	chunks := []streamChunk{emit(map[string]any{"role": "assistant", "content": ""}, nil)}
	if text != "" {
		chunks = append(chunks, emit(map[string]any{"content": text}, nil))
	}
	if len(calls) > 0 {
		for i, call := range calls {
			arguments := call.Arguments
			if call.Kind == "custom" {
				raw, _ := json.Marshal(map[string]any{"input": call.Arguments})
				arguments = string(raw)
			}
			chunks = append(chunks, emit(map[string]any{"tool_calls": []map[string]any{{
				"index": i, "id": call.ID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": arguments},
			}}}, nil))
		}
		chunks = append(chunks, emit(map[string]any{}, "tool_calls"))
	} else {
		chunks = append(chunks, emit(map[string]any{}, "stop"))
	}
	chunks = append(chunks, streamChunk{Payload: []byte("data: [DONE]\n\n")})
	return chunks
}
