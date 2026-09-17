package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
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

// handleManagementHandle serves both the management route and the resource
// panel: the host routes resource requests through management.handle as well.
//
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
	Routes    []managementRoute    `json:"Routes"`
	Resources []managementResource `json:"Resources,omitempty"`
}

// managementResource mirrors pluginapi.ResourceRoute. A resource needs no admin
// key, which is what lets the panel open in a plain browser.
type managementResource struct {
	Path        string
	Menu        string
	Description string
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
		Resources: []managementResource{
			{Path: panelPath, Menu: "Prism 登录", Description: "粘贴 prism 回调 URL 完成登录"},
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

	// The host dispatches resource requests through this same method -- the
	// panel is not a separate RPC -- so the path decides which surface ran.
	if strings.HasSuffix(strings.TrimRight(req.Path, "/"), panelPath) {
		return handlePanel(request)
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
	_, err := completeLogin(context.Background(), pasted, true)
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

// The operator-facing panel.
//
// Resource routes are served from the plugin's resource namespace, so the page
// can be opened in a browser without an admin key; the work it triggers is the
// same sign-in completion the management route does. A plain HTML form is used
// rather than JavaScript so the page cannot break on CSP or a stripped-down
// browser, and so the result is rendered server side.

const panelPath = "/panel"

func panelHTML(redirectURL, message string, failed bool, authorizeURL string) []byte {
	colour := "#0a7d33"
	if failed {
		colour = "#b3261e"
	}
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(redirectURL)
	notice := ""
	if strings.TrimSpace(message) != "" {
		notice = `<p style="color:` + colour + `;font-weight:600">` +
			strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(message) + `</p>`
	}
	return []byte(`<!DOCTYPE html>
<html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Prism 登录</title>
<style>
 body{font:15px/1.6 system-ui,"Segoe UI",sans-serif;margin:0;padding:28px;background:#faf9f7;color:#1f1f1f}
 main{max-width:760px;margin:0 auto}
 h1{font-size:20px;margin:0 0 4px}
 p.sub{color:#666;margin:0 0 20px}
 label{display:block;font-weight:600;margin:0 0 6px}
 textarea{width:100%;box-sizing:border-box;height:110px;padding:10px;border:1px solid #ccc;border-radius:6px;font:13px/1.5 ui-monospace,Consolas,monospace}
 button{margin-top:12px;padding:9px 18px;border:0;border-radius:6px;background:#1f1f1f;color:#fff;font-size:15px;cursor:pointer}
 ol{color:#555;padding-left:20px}
</style></head><body><main>
<h1>Prism 登录</h1>
` + loginLinkHTML(authorizeURL) + `
` + notice + `
<form method="get">
 <label for="redirect_url">回调 URL</label>
 <textarea id="redirect_url" name="redirect_url" placeholder="https://prism.openai.com/auth/popup-callback?code=…&amp;state=…" autofocus>` + escaped + `</textarea>
 <button type="submit">完成登录</button>
</form>
<p class="sub">回调 URL 会自动失效，整条链路要在 15 分钟内走完；超时就刷新本页重新生成登录链接。</p>
</main></body></html>`)
}

// handlePanel serves the form and processes its submission. The same handler
// backs both verbs because the resource namespace gives one path.
func handlePanel(request []byte) ([]byte, error) {
	var req struct {
		Method  string              `json:"Method"`
		Query   map[string][]string `json:"Query"`
		Body    []byte              `json:"Body"`
		Headers http.Header         `json:"Headers"`
	}
	if len(request) > 0 {
		_ = json.Unmarshal(request, &req)
	}

	pasted := strings.TrimSpace(firstQueryValue(req.Query, "redirect_url", "url", "callback"))
	if pasted == "" {
		pasted = strings.TrimSpace(formField(req.Body, req.Headers, "redirect_url"))
	}

	if req.Method == http.MethodGet && pasted == "" {
		// open the panel and the login link is already there: the operator should
		// not have to trigger a flow somewhere else first. prism mints the URL
		// (that is the provider's job), the page just starts the flow and shows
		// it. A flow that is still valid is reused, so refreshing does not mint a
		// new one every time.
		flow := pendingLogin()
		if flow == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			created, err := startLoginFunc(ctx)
			cancel()
			if err != nil {
				loginLog("error", "面板申请登录链接失败", map[string]any{"error": err.Error()})
				return htmlReply(http.StatusOK,
					panelHTML("", "无法向 prism 申请登录链接："+err.Error(), true, "")), nil
			}
			flow = created
		}
		return htmlReply(http.StatusOK, panelHTML("", "", false, flow.authorizeURL)), nil
	}
	if pasted == "" {
		return htmlReply(http.StatusOK, panelHTML("", "没有收到回调 URL，请粘贴后再提交。", true, "")), nil
	}

	loginLog("info", "面板收到回调", nil)
	if _, err := completeLogin(context.Background(), pasted, true); err != nil {
		loginLog("error", "面板提交的回调未能完成登录", map[string]any{"error": err.Error()})
		return htmlReply(http.StatusOK, panelHTML(pasted, err.Error(), true, "")), nil
	}
	loginLog("info", "登录完成（面板提交）", nil)
	return htmlReply(http.StatusOK, panelHTML("", "登录完成，凭据已写入 prism-provider。", false, "")), nil
}

// formField pulls a value out of an application/x-www-form-urlencoded body.
func formField(body []byte, headers http.Header, name string) string {
	contentType := headers.Get("Content-Type")
	if strings.Contains(contentType, "json") {
		return bodyValue(body)
	}
	for _, pair := range strings.Split(string(body), "&") {
		key, value, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		if decoded, err := url.QueryUnescape(key); err == nil && decoded == name {
			if unescaped, err := url.QueryUnescape(value); err == nil {
				return unescaped
			}
		}
	}
	return ""
}

func htmlReply(status int, page []byte) []byte {
	envelope, err := okEnvelope(map[string]any{
		"StatusCode": status,
		"Headers":    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		"Body":       page,
	})
	if err != nil {
		return errorEnvelope("internal_error", err.Error(), http.StatusInternalServerError)
	}
	return envelope
}

// redirectHTML bounces the browser to the authorization URL. A meta refresh is
// used rather than a 302 so the response shape stays a plain page, which is what
// the resource bridge is for, and so it works with scripting unavailable.
func redirectHTML(target string) []byte {
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(target)
	return []byte(`<!DOCTYPE html>
<html lang="zh"><head><meta charset="utf-8">
<meta http-equiv="refresh" content="0;url=` + escaped + `">
<title>正在前往 prism 登录…</title>
<style>body{font:15px/1.6 system-ui,"Segoe UI",sans-serif;padding:28px}
a{color:#1f1f1f}</style></head><body>
<p>正在前往 prism 的登录页…如果没有自动跳转，<a href="` + escaped + `">点这里</a>。</p>
<p>登录完成后回到本页，把回调 URL 粘进来即可。</p>
</body></html>`)
}

// loginLinkHTML renders the flow's authorization URL. The URL comes from the
// provider side; the page only presents it, so the operator clicks it in a normal
// tab instead of the popup CPA opens (that popup closes itself and cannot be
// copied from).
func loginLinkHTML(authorizeURL string) string {
	if strings.TrimSpace(authorizeURL) == "" {
		return `<p class="sub">没有拿到登录链接，刷新本页重试。</p>`
	}
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(authorizeURL)
	return `<p class="sub">① 在新标签页里打开这个登录链接并完成登录（<b>不要从 CPA 的按钮进</b>，` +
		`那条路会开一个授权后自关的弹窗，回调 URL 抄不出来）：</p>
<p><a href="` + escaped + `" target="_blank" rel="noopener" style="word-break:break-all">` + escaped + `</a></p>
<p class="sub">② 登录完成后，从浏览器历史（Ctrl+H 搜 <code>popup-callback</code>）或 Fiddler 里复制那条 <code>GET /auth/popup-callback?code=…</code> 的完整 URL</p>
<p class="sub">③ 粘到下面，点"完成登录"</p>`
}
