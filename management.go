package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Plugin-owned management endpoint for finishing a sign-in.
//
// The host's own "paste the redirect URL" box cannot be used here: it validates
// the pasted state with ValidateOAuthState, whose maxOAuthStateLength is 128,
// while prism's state is ~484 characters -- so it rejects the callback with
// "invalid state" before it ever looks for our session. prism's state cannot be
// shortened either, because prism's server needs it to redeem the code.
//
// So the plugin exposes its own route. The host mounts it under the plugin's
// management prefix, and it takes the callback URL (or a bare code) either as a
// JSON body or as query parameters, which makes it usable from a browser
// address bar as well as from any HTTP client.

// managementRoute is the registration entry for the host. Only the fields the
// host reads are sent; the handler interface it carries on the Go side has no
// meaning across the process boundary.
type managementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
}

type managementRegistration struct {
	Routes []managementRoute `json:"Routes"`
}

// callbackPath is appended to whatever prefix the host mounts plugin management
// routes under.
const callbackPath = "/callback"

func handleManagementRegister() ([]byte, error) {
	description := "Unattended sign-in completion: POST {\"redirect_url\": \"…\"} or GET ?redirect_url=…"
	return okEnvelope(managementRegistration{
		Routes: []managementRoute{
			{Method: http.MethodPost, Path: callbackPath, Description: description},
			{Method: http.MethodGet, Path: callbackPath, Description: description},
		},
	})
}

func handleManagementHandle(request []byte) ([]byte, error) {
	var req struct {
		Method  string              `json:"Method"`
		Path    string              `json:"Path"`
		Query   map[string][]string `json:"Query"`
		Body    []byte              `json:"Body"`
		Headers http.Header         `json:"Headers"`
	}
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return managementReply(http.StatusBadRequest, "无法解析管理请求: "+err.Error())
		}
	}

	source := "直接粘贴"
	pasted := firstQueryValue(req.Query, "redirect_url", "url", "callback", "redirect")
	if strings.TrimSpace(pasted) == "" {
		// Fall back to a bare code, which still needs the flow's own state.
		if code := firstQueryValue(req.Query, "code"); code != "" {
			pasted = "code=" + code
		}
	}
	if strings.TrimSpace(pasted) == "" && len(req.Body) > 0 {
		pasted = bodyValue(req.Body)
		source = "请求体"
	}
	if strings.TrimSpace(pasted) == "" {
		return managementReply(http.StatusBadRequest,
			"没有拿到回调 URL：请用 ?redirect_url=… 或在 body 里给 {\"redirect_url\":\"…\"}")
	}

	loginLog("info", "收到管理接口提交的回调", map[string]any{"source": source})
	_, err := completeLogin(context.Background(), pasted)
	if err != nil {
		loginLog("error", "管理接口提交的回调未能完成登录", map[string]any{"error": err.Error()})
		return managementReply(http.StatusBadRequest, err.Error())
	}
	loginLog("info", "登录完成（管理接口提交）", nil)
	return managementReply(http.StatusOK, "登录完成，凭据已写入 prism-provider")
}

// bodyValue pulls the callback out of a JSON body, accepting the same field
// names as the query form plus the whole body as a last resort.
func bodyValue(body []byte) string {
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err == nil {
		for _, key := range []string{"redirect_url", "url", "callback", "redirect", "code", "state"} {
			if value, ok := fields[key].(string); ok && strings.TrimSpace(value) != "" {
				if key == "code" || key == "state" {
					continue
				}
				return value
			}
		}
		if code, ok := fields["code"].(string); ok && strings.TrimSpace(code) != "" {
			return "code=" + code
		}
	}
	return strings.Trim(string(body), " \t\r\n\"'")
}

func firstQueryValue(values map[string][]string, keys ...string) string {
	for _, key := range keys {
		for candidate, list := range values {
			if !strings.EqualFold(candidate, key) {
				continue
			}
			for _, value := range list {
				if strings.TrimSpace(value) != "" {
					return value
				}
			}
		}
	}
	return ""
}

// managementReply renders a ManagementResponse the host passes straight back to
// the caller.
func managementReply(status int, message string) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{
		"status":  status < 400,
		"message": message,
	})
	// The host reads the plugin's answer as the usual {"ok":…,"result":…}
	// envelope and only then decodes it into a ManagementResponse, so the
	// response object has to sit inside one.
	return okEnvelope(map[string]any{
		"StatusCode": status,
		"Headers":    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		"Body":       body,
	})
}
