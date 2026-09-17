package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Sign-in flow.
//
// prism cannot be logged into without a browser: its OAuth redirect_uri is
// locked to https://prism.openai.com/auth/popup-callback (a localhost callback
// is rejected with invalid_request), and every session cookie is HttpOnly, so
// nothing can read the result out of the browser afterwards.
//
// What a plugin *can* do is hold the flow's own state:
//
//	POST /api/auth/redirect   -> the authorize URL *and* a
//	                             prism_openai_oauth_state_binding cookie, into
//	                             a cookie jar we own
//	operator opens the URL, signs in, and hands back the callback URL
//	POST /api/auth/popup-callback {code, state}  -> sets prism_session_token,
//	                             prism_oai_access_token, ... into the same jar
//
// So the operator's part is one paste, and never a cookie jar or a curl command.
// The pasted value arrives through the ordinary auth-file channel, which means
// no new host capability is needed: drop the callback URL into the auth
// directory and the host calls auth.parse with it.

const loginFlowTTL = 15 * time.Minute

// callbackFileHint is the file the operator pastes the callback URL into.
const callbackFileHint = "prism-login.txt"

type loginFlow struct {
	state        string
	authorizeURL string
	prismState   string
	client       *prismClient
	created      time.Time
	// browser is the window we opened for the operator. When present the flow
	// completes by reading its cookie jar over CDP, not by asking for a paste.
	browser *browserLogin

	result *authData
	failed string
}

var (
	loginMu     sync.Mutex
	activeLogin *loginFlow
)

// newLoginClient builds a client whose cookie jar survives between the two
// halves of the flow. The jar is what makes the exchange work: the binding
// cookie set by /api/auth/redirect has to come back on /api/auth/popup-callback.
func newLoginClient(seedCookies string) (*prismClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("创建 cookie jar 失败: %w", err)
	}
	origin, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if cookies := parseCookieHeader(seedCookies); len(cookies) > 0 {
		jar.SetCookies(origin, cookies)
	}
	return &prismClient{
		http:      &http.Client{Timeout: 120 * time.Second, Jar: jar},
		credKey:   "login-flow",
		projectID: "",
	}, nil
}

// parseCookieHeader turns a Cookie: header into cookies. Values are never
// logged; this is only used to seed the jar.
func parseCookieHeader(header string) []*http.Cookie {
	var out []*http.Cookie
	for _, part := range strings.Split(header, ";") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || name == "" {
			continue
		}
		out = append(out, &http.Cookie{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
	}
	return out
}

// startLogin begins a sign-in and returns the flow, including the URL the
// operator has to open.
func startLogin(ctx context.Context) (*loginFlow, error) {
	client, err := newLoginClient(currentConfig().Cookies)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data struct {
			Provider string `json:"provider"`
			URL      string `json:"url"`
		} `json:"data"`
		Error any `json:"error"`
	}
	if err := client.do(ctx, http.MethodPost, "/api/auth/redirect", map[string]any{
		"action":   "sign-in",
		"provider": "keycloak",
	}, &resp); err != nil {
		return nil, fmt.Errorf("获取 prism 登录地址失败: %w", err)
	}
	if resp.Data.URL == "" {
		return nil, fmt.Errorf("prism 未返回登录地址")
	}

	prismState := ""
	if parsed, err := url.Parse(resp.Data.URL); err == nil {
		prismState = parsed.Query().Get("state")
	}
	if prismState == "" {
		return nil, fmt.Errorf("prism 登录地址缺少 state")
	}

	flow := &loginFlow{
		state:        newLoginState(),
		authorizeURL: resp.Data.URL,
		prismState:   prismState,
		client:       client,
		created:      time.Now(),
	}

	// Open a window for the operator. If no Chromium browser is installed we
	// still hand back the URL, and the paste path in auth.parse stays available
	// as a fallback.
	if browser, err := launchBrowserLogin(ctx, resp.Data.URL); err == nil {
		flow.browser = browser
	}

	loginMu.Lock()
	if previous := activeLogin; previous != nil && previous.browser != nil {
		previous.browser.close()
	}
	activeLogin = flow
	loginMu.Unlock()
	return flow, nil
}

// newLoginState returns our own state token. The host stores it as the OAuth
// session key and hands it back on every poll, and it must satisfy the host's
// ValidateOAuthState: non-empty, url-safe, no path separators.
func newLoginState() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return "prism" + hex.EncodeToString(b[:])
}

// pollLogin reports progress for one flow.
func pollLogin(state string) (status string, message string, authed *authData) {
	loginMu.Lock()
	flow := activeLogin
	switch {
	case flow == nil || flow.state != state:
		loginMu.Unlock()
		return "error", "找不到对应的登录流程，请重新点击登录", nil
	case time.Since(flow.created) > loginFlowTTL:
		if flow.browser != nil {
			flow.browser.close()
			flow.browser = nil
		}
		loginMu.Unlock()
		return "error", "登录流程已超时，请重新点击登录", nil
	case flow.failed != "":
		message := flow.failed
		loginMu.Unlock()
		return "error", message, nil
	case flow.result != nil:
		result := flow.result
		loginMu.Unlock()
		return "success", "登录完成", result
	}
	browser := flow.browser
	loginMu.Unlock()

	// Reading the browser's cookie jar is a network round trip, so it happens
	// with the lock released.
	if browser != nil {
		cookies, signedIn, err := browser.sessionCookieHeader(context.Background())
		if err == nil && signedIn {
			auth := &authData{
				Provider:    providerName,
				Label:       "prism (browser sign-in)",
				Prefix:      "prism",
				StorageJSON: (&storedAuth{Cookies: cookies}).encode(),
			}
			loginMu.Lock()
			if activeLogin == flow {
				flow.result = auth
			}
			loginMu.Unlock()
			browser.close()
			return "success", "登录完成", auth
		}
		return "pending", "已在浏览器窗口里打开 prism 的登录页，登录完成后这里会自动完成，不用复制任何东西。", nil
	}

	return "pending", fmt.Sprintf(
		"没有找到可用的 Chrome/Edge，无法自动读取登录结果。请手动完成：在浏览器里登录后，"+
			"把回调 URL 粘贴到 CPA 的 auth 目录下新建的 %s 文件（Fiddler 里的 "+
			"GET /auth/popup-callback?code=… 那条也可以整条粘贴）。",
		callbackFileHint), nil
}

// pendingLogin returns the current flow, if any, so a pasted callback URL can be
// matched against it.
func pendingLogin() *loginFlow {
	loginMu.Lock()
	defer loginMu.Unlock()
	if activeLogin == nil || time.Since(activeLogin.created) > loginFlowTTL {
		return nil
	}
	return activeLogin
}

// callbackCode extracts the authorization code from pasted text.
//
// prism's callback page is a popup: it redeems the code and closes itself, so
// the operator usually cannot copy the bare URL out of the address bar. The URL
// therefore tends to arrive embedded in something else -- a DevTools
// "Copy as cURL", a request line, a header dump -- so the first thing this does
// is scan for a callback URL anywhere in the text.
//
// The bare "code=" form stays deliberately narrow: requiring the text to *start*
// with it keeps an ordinary cookie header (which routinely contains "code="
// inside some unrelated value) from being mistaken for a callback.
func callbackCode(text string) (string, bool) {
	// Copied values often arrive wrapped in quotes, angle brackets or markdown
	// punctuation; strip that framing before looking at the content.
	trimmed := strings.Trim(strings.TrimSpace(text), "\"'<>` \t\r\n,")
	if trimmed == "" {
		return "", false
	}

	// Find the URL token inside arbitrary surrounding text. The delimiters cover
	// the shapes people actually paste: quotes from a cURL command, and the
	// brackets of a markdown link.
	if i := strings.Index(trimmed, "://"); i >= 0 {
		start := strings.LastIndexAny(trimmed[:i], " \t\r\n\"'([<") + 1
		candidate := trimmed[start:]
		if end := strings.IndexAny(candidate, " \t\r\n\"')]>,"); end >= 0 {
			candidate = candidate[:end]
		}
		if parsed, err := url.Parse(candidate); err == nil && strings.Contains(parsed.Path, "popup-callback") {
			if code := parsed.Query().Get("code"); code != "" {
				return code, true
			}
		}
	}

	rest, ok := strings.CutPrefix(trimmed, "code=")
	if !ok {
		return "", false
	}
	if end := strings.IndexAny(rest, "&\r\n\t "); end >= 0 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	return rest, rest != ""
}

// completeLogin finishes the flow from a pasted callback URL and returns the
// credential the host should store.
func completeLogin(ctx context.Context, pasted string) (*authData, error) {
	code, ok := callbackCode(pasted)
	if !ok {
		return nil, fmt.Errorf("这不是一个 prism 回调 URL")
	}
	flow := pendingLogin()
	if flow == nil {
		return nil, fmt.Errorf("没有进行中的登录流程（或已超时）：请先点击登录，再粘贴回调 URL")
	}

	var ack struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := flow.client.do(ctx, http.MethodPost, "/api/auth/popup-callback", map[string]any{
		"code":     code,
		"legacy":   false,
		"provider": "openai",
		"state":    flow.prismState,
	}, &ack); err != nil {
		return nil, fmt.Errorf("提交回调失败: %w", err)
	}
	if !ack.OK {
		// The usual cause is a code the browser already redeemed: prism's own
		// popup-callback page consumes it as soon as it loads.
		return nil, fmt.Errorf("prism 未接受该回调（%s）。常见原因：这条 code 已被浏览器页面用掉——"+
			"请在登录跳转回 prism 时立刻按 Esc 停止加载，再复制地址栏 URL 粘贴", ack.Error)
	}

	origin, _ := url.Parse(baseURL)
	jarCookies := flow.client.http.Jar.Cookies(origin)
	header := joinCookieHeader(jarCookies)
	if !strings.Contains(header, "prism_session_token=") {
		return nil, fmt.Errorf("回调被接受但没有拿到会话 cookie，请重试一次")
	}

	// The session is enough on its own; hydrate() fills in the user id from
	// /auth/session on first use.
	auth := &authData{
		Provider: providerName,
		Label:    "prism (browser sign-in)",
		Prefix:   "prism",
		// The whole jar travels with the credential: it carries
		// prism_session_token plus the OpenAI tokens prism set alongside it.
		StorageJSON: (&storedAuth{Cookies: header}).encode(),
	}
	loginMu.Lock()
	if activeLogin == flow {
		activeLogin.result = auth
	}
	loginMu.Unlock()
	return auth, nil
}

// joinCookieHeader renders cookies the way a browser would send them.
func joinCookieHeader(cookies []*http.Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if c.Name == "" || c.Value == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// loginFlowJSON is the shape returned to the host from auth.login.start.
type loginStartResponse struct {
	Provider  string         `json:"Provider"`
	URL       string         `json:"URL"`
	State     string         `json:"State"`
	ExpiresAt time.Time      `json:"ExpiresAt"`
	Metadata  map[string]any `json:"Metadata,omitempty"`
}

func handleLoginStart(ctx context.Context) ([]byte, error) {
	flow, err := startLogin(ctx)
	if err != nil {
		return nil, err
	}
	return okEnvelope(loginStartResponse{
		Provider:  providerName,
		URL:       flow.authorizeURL,
		State:     flow.state,
		ExpiresAt: flow.created.Add(loginFlowTTL),
		Metadata: map[string]any{
			"instruction": "登录完成后把地址栏里的回调 URL 粘贴到 auth 目录下的 " + callbackFileHint,
		},
	})
}

func handleLoginPoll(request []byte) ([]byte, error) {
	var req struct {
		State string `json:"State"`
	}
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode auth.login.poll: %w", err)
		}
	}
	status, message, authed := pollLogin(req.State)
	resp := map[string]any{"Status": status, "Message": message}
	if authed != nil {
		resp["Auth"] = *authed
	}
	return okEnvelope(resp)
}
