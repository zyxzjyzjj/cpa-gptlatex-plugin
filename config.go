package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// config is the parsed form of the `plugins.configs.prism-provider` YAML block.
type config struct {
	Cookies              string
	UserID               string
	ProjectUUID          string
	ProjectTitle         string
	Models               []string
	DefaultModel         string
	ReasoningEffort      string
	SystemPrompt         string
	ClientTools          bool
	ClientToolsMaxRounds int
	// EnableSandbox 决定是否在对话前领取沙箱并把凭证放进 metadata。
	// **这不是可选项**：实测（2026-09-17）不提供 sandbox_url 时服务端只会一直
	// 返回 sandbox_reconnecting，整轮永远不会成功，因此默认开启。
	EnableSandbox bool
}

// fallbackModel is used only when neither the config nor a live upstream
// lookup can name a model: a credential-less first start with prism
// unreachable. The real list comes from prism itself (see catalogFor).
const fallbackModel = "gpt-5.6-sol"

func defaultConfig() config {
	return config{
		ProjectTitle: "CLIProxyAPI",
		// Models 留空表示"按上游下发的清单走"：prism 的可选模型由 Statsig
		// 动态配置决定，写死会过期（gpt-6-astra 一天之间就从可用变成 400）。
		// 填了就在这里覆盖，供需要固定模型的部署使用。
		Models:               nil,
		DefaultModel:         "",
		ReasoningEffort:      "medium",
		ClientTools:          true,
		ClientToolsMaxRounds: 2,
		// 没有沙箱就拿不到答案。
		EnableSandbox: true,
	}
}

func currentConfig() config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfgState
}

// configure parses the plugin config block handed over by plugin.register /
// plugin.reconfigure. The host sends rpcLifecycleRequest, i.e.
// {"config_yaml": <base64 of plugins.configs.prism-provider>, "schema_version": N}.
// Unknown keys are ignored so the config stays forward-compatible.
func configure(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var body []byte
	var req lifecycleRequest
	if err := json.Unmarshal(raw, &req); err == nil {
		// A successful decode means the host used the documented wrapper; an
		// absent config_yaml then legitimately means "nothing configured".
		body = req.ConfigYAML
	} else {
		// Defensive: tolerate a host that passes the bare block instead.
		body = raw
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	var flat map[string]any
	if err := yaml.Unmarshal(body, &flat); err != nil {
		return fmt.Errorf("解析 plugins.configs.%s 失败: %w", providerName, err)
	}

	next := defaultConfig()
	if v, ok := flat["cookies"].(string); ok {
		next.Cookies = strings.TrimSpace(v)
	}
	if v, ok := flat["user_id"].(string); ok {
		next.UserID = strings.TrimSpace(v)
	}
	if v, ok := flat["project_uuid"].(string); ok {
		next.ProjectUUID = strings.TrimSpace(v)
	}
	if v, ok := flat["project_title"].(string); ok && strings.TrimSpace(v) != "" {
		next.ProjectTitle = strings.TrimSpace(v)
	}
	if v, ok := flat["default_model"].(string); ok && strings.TrimSpace(v) != "" {
		next.DefaultModel = strings.TrimSpace(v)
	}
	if v, ok := flat["reasoning_effort"].(string); ok {
		next.ReasoningEffort = normalizeEffort(v)
	}
	if v, ok := flat["system_prompt"].(string); ok {
		next.SystemPrompt = v
	}
	if v, ok := flat["client_tools"].(bool); ok {
		next.ClientTools = v
	}
	if v, ok := flat["client_tools_max_rounds"].(int); ok && v > 0 {
		next.ClientToolsMaxRounds = v
	} else if v, ok := flat["client_tools_max_rounds"].(float64); ok && v > 0 {
		next.ClientToolsMaxRounds = int(v)
	}
	if v, ok := flat["sandbox"].(bool); ok {
		next.EnableSandbox = v
	}
	if models, ok := parseModels(flat["models"]); ok {
		next.Models = models
	}

	cfgMu.Lock()
	cfgState = next
	cfgMu.Unlock()
	return nil
}

// parseModels accepts either a JSON array of strings or a JSON array of
// objects carrying `id`. The docs require a single-line JSON array; YAML block
// sequences arrive here already decoded as []any.
func parseModels(v any) ([]string, bool) {
	if v == nil {
		return nil, false
	}
	list, ok := v.([]any)
	if !ok {
		if s, isStr := v.(string); isStr {
			var decoded []any
			if json.Unmarshal([]byte(s), &decoded) != nil {
				return nil, false
			}
			list = decoded
		} else {
			return nil, false
		}
	}
	if len(list) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		switch t := item.(type) {
		case string:
			if s := strings.TrimSpace(t); s != "" {
				out = append(out, s)
			}
		case map[string]any:
			if id, _ := t["id"].(string); strings.TrimSpace(id) != "" {
				out = append(out, strings.TrimSpace(id))
			}
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func normalizeEffort(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "low":
		return "low"
	case "high":
		return "high"
	case "xhigh":
		return "xhigh"
	default:
		return "medium"
	}
}
