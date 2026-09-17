package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleExecute services executor.execute / executor.execute_stream. Prism has
// no upstream SSE: a turn is start + poll, so both entry points collect the
// full answer and only differ in how it is framed for the client.
func handleExecute(raw []byte, stream bool) ([]byte, error) {
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

	chat, err := decodeChatRequest(req.Payload)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}
	model := resolveModel(req.Model, chat.Model)

	input, err := buildInput(chat.Messages)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), http.StatusBadRequest), nil
	}

	projectID, err := client.ensureProject(ctx)
	if err != nil {
		return errorEnvelope("server_error", err.Error(), http.StatusBadGateway), nil
	}

	cfg := currentConfig()
	metadata := map[string]any{
		"projectId":        projectID,
		"userId":           client.userID,
		"model":            model,
		"reasoning_effort": cfg.ReasoningEffort,
		"frontend_origin":  baseURL,
	}
	// 沙箱凭证必须放进 metadata，服务端 Agent 才有工具执行环境（读写 .tex、编译 PDF）。
	// 实测 metadata.sandbox_url 用公开地址即可，服务端在 turn_state 里会换成内部集群地址。
	if cfg.EnableSandbox {
		sb, sbErr := client.ensureSandbox(ctx, projectID)
		if sbErr != nil {
			return errorEnvelope("upstream_error", sbErr.Error(), http.StatusBadGateway), nil
		}
		metadata["sandbox_url"] = sb.URL
		metadata["sandbox_token"] = sb.Token
	}

	turn := turnRequest{Input: input, Metadata: metadata}

	payload, err := client.runTurn(ctx, turn, 5*time.Second, nil)
	if err != nil {
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
	if strings.TrimSpace(text) == "" {
		text = "（prism 未返回可显示的文本）"
	}

	// Framing follows the RPC method, not the client's "stream" flag: the host
	// dispatched on the method and will decode exactly one of the two shapes.
	if stream {
		return okEnvelope(streamResponse{
			Headers: http.Header{
				"Content-Type":  []string{"text/event-stream"},
				"Cache-Control": []string{"no-cache"},
			},
			Chunks: sseChunks(model, payload.ID, text),
		})
	}
	return okEnvelope(executorResponse{
		Payload: completion(model, payload.ID, text),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
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
	sa.UserID = id
	return nil
}

// ------------------------------------------------------------ chat mapping

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func decodeChatRequest(raw []byte) (*chatRequest, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("请求体为空")
	}
	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("只支持 chat-completions 格式: %w", err)
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages 为空")
	}
	return &req, nil
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
		text := flattenContent(m.Content)
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
		default: // user, tool, developer
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

func resolveModel(requested, bodyModel string) string {
	cfg := currentConfig()
	for _, candidate := range []string{requested, bodyModel} {
		c := strings.TrimSpace(candidate)
		if c == "" {
			continue
		}
		// Strip a provider prefix such as "prism-provider/gpt-6-astra".
		if i := strings.LastIndex(c, "/"); i >= 0 {
			c = c[i+1:]
		}
		for _, known := range cfg.Models {
			if strings.EqualFold(known, c) {
				return known
			}
		}
		return c
	}
	return cfg.DefaultModel
}

// ------------------------------------------------------------- responses

func completion(model, upstreamID, text string) []byte {
	id := upstreamID
	if id == "" {
		id = "prism"
	}
	body := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
	raw, _ := json.Marshal(body)
	return raw
}

// sseChunks frames the answer as an OpenAI streaming response. Prism delivers
// the whole turn at once, so this is emitted as a short replay rather than a
// live stream.
func sseChunks(model, upstreamID, text string) []streamChunk {
	id := upstreamID
	if id == "" {
		id = "prism"
	}
	created := time.Now().Unix()
	base := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
	}
	emit := func(delta map[string]any, finish any) streamChunk {
		frame := map[string]any{}
		for k, v := range base {
			frame[k] = v
		}
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != nil {
			choice["finish_reason"] = finish
		} else {
			choice["finish_reason"] = nil
		}
		frame["choices"] = []map[string]any{choice}
		raw, _ := json.Marshal(frame)
		return streamChunk{Payload: []byte("data: " + string(raw) + "\n\n")}
	}

	return []streamChunk{
		emit(map[string]any{"role": "assistant", "content": ""}, nil),
		emit(map[string]any{"content": text}, nil),
		emit(map[string]any{}, "stop"),
		{Payload: []byte("data: [DONE]\n\n")},
	}
}
