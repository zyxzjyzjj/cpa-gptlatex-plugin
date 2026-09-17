package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Browser-assisted sign-in.
//
// prism's OAuth redirect_uri is locked to https://prism.openai.com/auth/popup-callback
// (auth.openai.com answers 400 invalid_request for anything else), and every
// session cookie is HttpOnly, so no amount of HTTP work on our side can observe
// the result. What we *can* do is own the browser: launch one with a debugging
// port under a profile of ours, let the operator sign in normally in it, and read
// the cookie jar over CDP -- Storage.getCookies returns HttpOnly cookies, which
// is exactly what is needed and cannot be done from page JavaScript.
//
// Side benefit: the browser's own /api/auth/popup-callback call fails with
// 401 openai-provider-validation-failed (the provider/state-binding cookies live
// in *our* jar, not the browser's), so the authorization code is never consumed
// by the page and the session is created cleanly by our own exchange.

// browserLogin is one in-flight browser-assisted sign-in.
type browserLogin struct {
	debugPort int
	profile   string
	cmd       *exec.Cmd
	started   time.Time
}

// findBrowser locates a Chromium browser we can drive.
func findBrowser() (string, error) {
	var candidates []string
	if v := strings.TrimSpace(os.Getenv("CPA_PRISM_BROWSER")); v != "" {
		candidates = append(candidates, v)
	}
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
			if root == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(root, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(root, `Microsoft\Edge\Application\msedge.exe`),
			)
		}
	} else {
		candidates = append(candidates, "google-chrome", "chromium", "chromium-browser", "microsoft-edge")
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if !strings.ContainsAny(candidate, `/\`) {
			if path, err := exec.LookPath(candidate); err == nil {
				return path, nil
			}
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("找不到 Chrome/Edge；可用环境变量 CPA_PRISM_BROWSER 指定浏览器路径")
}

// freePort asks the OS for an unused localhost port.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// launchBrowserLogin opens a browser window on url with remote debugging on.
//
// The profile is ours (not the operator's everyday profile) so nothing about
// their normal browsing is touched, and so the debugging port is always allowed
// -- Chrome 136+ refuses --remote-debugging-port on the default profile.
func launchBrowserLogin(ctx context.Context, url string) (*browserLogin, error) {
	browser, err := findBrowser()
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("分配调试端口失败: %w", err)
	}
	profile, err := browserProfileDir()
	if err != nil {
		return nil, err
	}

	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
	}
	// The browser has to reach prism.openai.com on its own, so hand it the same
	// proxy the plugin uses when one is configured.
	if proxy := proxyServerArgument(); proxy != "" {
		args = append(args, "--proxy-server="+proxy)
	}
	args = append(args, url)

	cmd := exec.Command(browser, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动浏览器失败: %w", err)
	}
	return &browserLogin{debugPort: port, profile: profile, cmd: cmd, started: time.Now()}, nil
}

// browserProfileDir keeps the browser profile inside the user's local app data.
func browserProfileDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "cpa-prism-provider", "browser")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建浏览器 profile 目录失败: %w", err)
	}
	return dir, nil
}

// proxyServerArgument renders the configured proxy as Chrome's --proxy-server.
func proxyServerArgument() string {
	proxyURL, err := proxyFromEnvironment()
	if err != nil || proxyURL == nil {
		return ""
	}
	return proxyURL.String()
}

// close terminates the browser we launched (and only that one).
func (b *browserLogin) close() {
	if b == nil || b.cmd == nil || b.cmd.Process == nil {
		return
	}
	_ = b.cmd.Process.Kill()
	_, _ = b.cmd.Process.Wait()
}

// cdpCookie is the subset of a CDP Network.Cookie we need.
type cdpCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
}

// sessionCookieHeader asks the running browser for its cookie jar and renders the
// prism cookies as a Cookie header. Returns "" while the operator has not signed
// in yet.
func (b *browserLogin) sessionCookieHeader(ctx context.Context) (string, bool, error) {
	cookies, err := b.cookies(ctx)
	if err != nil {
		return "", false, err
	}
	var parts []string
	haveSession := false
	for _, c := range cookies {
		if !strings.HasSuffix(c.Domain, "prism.openai.com") && c.Domain != "openai.com" {
			continue
		}
		if c.Name == "" || c.Value == "" {
			continue
		}
		// Skip a browser that is merely carrying someone's unrelated cookies:
		// the pair prism actually needs is the session plus the OpenAI token.
		if c.Name == "prism_session_token" {
			haveSession = true
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	if !haveSession {
		return "", false, nil
	}
	return strings.Join(parts, "; "), true, nil
}

// cookies performs the CDP round trip: resolve the page target, then ask for the
// whole jar with Storage.getCookies (browser-wide, so it does not matter which
// URL the tab currently shows).
func (b *browserLogin) cookies(ctx context.Context) ([]cdpCookie, error) {
	wsURL, err := b.debuggerURL(ctx)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	// Loopback: never route the debugger connection through the upstream proxy.
	dialer.Proxy = nil
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("连接浏览器调试端口失败: %w", err)
	}
	defer conn.Close()

	request := map[string]any{"id": 1, "method": "Storage.getCookies"}
	if err := conn.WriteJSON(request); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		var reply struct {
			ID     int `json:"id"`
			Result struct {
				Cookies []cdpCookie `json:"cookies"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(payload, &reply); err != nil {
			continue
		}
		if reply.Error != nil {
			return nil, fmt.Errorf("浏览器返回错误: %s", reply.Error.Message)
		}
		if reply.ID == 1 {
			return reply.Result.Cookies, nil
		}
	}
	return nil, fmt.Errorf("浏览器调试端口无响应")
}

// debuggerURL reads the browser's WebSocket endpoint for the browser target.
func (b *browserLogin) debuggerURL(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/version", b.debugPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("浏览器调试端口还没就绪: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &version); err != nil {
		return "", err
	}
	if version.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("浏览器没有暴露调试 WebSocket")
	}
	return version.WebSocketDebuggerURL, nil
}

// loginLog reports sign-in progress through the host logger, so it shows up in
// CPA's own output instead of a file the operator has to go find. Values that
// are credentials -- cookies, the state, the authorize URL -- are never included.
func loginLog(level, message string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["component"] = "login"
	hostLog(level, message, fields)
}
