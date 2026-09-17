package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// newUUID returns a random RFC 4122 v4 UUID. Prism expects the client to mint
// project ids itself, and uuid.NewRandom is the only such place we need it.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; fall back to a time-based id
		// rather than aborting a request.
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

const (
	baseURL       = "https://prism.openai.com"
	maxInputBytes = 1_800_000 // 前端在 18e5 字节处截断 input
	userAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0"
)

type prismClient struct {
	http      *http.Client
	cookies   string
	projectID string
	userID    string
	// credKey identifies the credential this client speaks for, so an
	// auto-created project can be reused across requests.
	credKey string
}

func newPrismClient(sa *storedAuth) (*prismClient, error) {
	if strings.TrimSpace(sa.Cookies) == "" {
		return nil, fmt.Errorf("缺少 prism cookie")
	}
	return &prismClient{
		http:      &http.Client{Timeout: 180 * time.Second},
		cookies:   sa.Cookies,
		projectID: sa.ProjectID,
		userID:    sa.UserID,
		credKey:   credentialKey(sa.Cookies),
	}, nil
}

// credentialKey derives a stable, non-reversible cache key from a cookie jar.
// The cookie itself must not be used as a map key: it is a long-lived secret.
func credentialKey(cookies string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(cookies)))
	return hex.EncodeToString(sum[:16])
}

// ------------------------------------------------------------- session

type sessionResponse struct {
	User struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		IsAnonymous bool   `json:"is_anonymous"`
		AppMetadata struct {
			UserID string `json:"user_id"`
		} `json:"app_metadata"`
	} `json:"user"`
	Policy struct {
		RequiresOpenAIAccessTokenCookie bool `json:"requires_openai_access_token_cookie"`
		User                            struct {
			PrismUserID  string `json:"prism_user_id"`
			OpenAIUserID string `json:"openai_user_id"`
			Email        string `json:"email"`
		} `json:"user"`
	} `json:"policy"`
	UserTier string `json:"userTier"`
}

// OpenAIUserID is the `user-xxxx` handle that /api/llm expects in
// metadata.userId. It is NOT the user UUID.
func (s *sessionResponse) OpenAIUserID() string {
	if s.Policy.User.OpenAIUserID != "" {
		return s.Policy.User.OpenAIUserID
	}
	return s.User.AppMetadata.UserID
}

func (c *prismClient) session() (*sessionResponse, error) {
	var out sessionResponse
	if err := c.do(context.Background(), http.MethodGet, "/auth/session", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ------------------------------------------------------------- projects

// projectCache remembers which project a credential created. prismClient is
// rebuilt for every request and the created UUID is never written back to the
// credential file, so without this cache a conversation started without an
// explicit project_uuid would mint a brand new project on every single request.
const projectTTL = 6 * time.Hour

var (
	projectMu    sync.Mutex
	projectCache = map[string]projectEntry{}
)

type projectEntry struct {
	uuid    string
	expires time.Time
}

func lookupProject(credKey string) (string, bool) {
	if credKey == "" {
		return "", false
	}
	projectMu.Lock()
	defer projectMu.Unlock()
	entry, ok := projectCache[credKey]
	if !ok || time.Now().After(entry.expires) {
		if ok {
			delete(projectCache, credKey)
		}
		return "", false
	}
	return entry.uuid, true
}

func rememberProject(credKey, uuid string) {
	if credKey == "" || uuid == "" {
		return
	}
	projectMu.Lock()
	projectCache[credKey] = projectEntry{uuid: uuid, expires: time.Now().Add(projectTTL)}
	projectMu.Unlock()
}

// dropProject forgets the cached project for a credential.
func dropProject(credKey string) {
	projectMu.Lock()
	delete(projectCache, credKey)
	projectMu.Unlock()
}

// ensureProject returns a usable project UUID, creating one on first use.
// Prism expects the client to mint the UUID itself.
func (c *prismClient) ensureProject(ctx context.Context) (string, error) {
	cfg := currentConfig()
	if c.projectID != "" {
		return c.projectID, nil
	}
	if cfg.ProjectUUID != "" {
		c.projectID = cfg.ProjectUUID
		return c.projectID, nil
	}
	if cached, ok := lookupProject(c.credKey); ok {
		c.projectID = cached
		return cached, nil
	}

	id := newUUID()
	body := map[string]any{
		"project_uuid": id,
		"title":        cfg.ProjectTitle,
		"file_uuids":   []string{},
	}
	var resp struct {
		UUID string `json:"uuid"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/projects", body, &resp); err != nil {
		return "", fmt.Errorf("创建 prism 项目失败: %w", err)
	}
	if resp.UUID == "" {
		resp.UUID = id
	}
	c.projectID = resp.UUID
	rememberProject(c.credKey, resp.UUID)
	return c.projectID, nil
}

// ------------------------------------------------------------- turns

type turnResponse struct {
	RequestID string          `json:"request_id"`
	Status    string          `json:"status"`
	Message   string          `json:"message"`
	TurnState json.RawMessage `json:"turn_state"`
	Response  *turnResult     `json:"response"`
}

// terminalTurnError reports a response that ends the turn in failure. Prism
// signals these with a top-level status other than started/pending/completed
// plus an explanatory message — observed live as HTTP 500
// {"status":"error","message":"auth-session-policy-unavailable"}.
//
// Without this check a terminal error that arrived with HTTP 200 and no
// request_id would be reported as "未返回 request_id" (losing the actual
// reason), and one that arrived mid-poll would be retried until the caller's
// 15-minute context expired instead of failing fast.
func terminalTurnError(resp *turnResponse) *prismError {
	switch resp.Status {
	case "error", "failed":
		status := http.StatusBadGateway
		if strings.Contains(resp.Message, "auth-session") {
			// The credential, not prism, is what needs fixing.
			status = http.StatusUnauthorized
		}
		message := resp.Message
		if message == "" {
			message = "prism 返回 " + resp.Status
		}
		return &prismError{Status: status, Reason: resp.Status, Message: message}
	}
	return nil
}

type turnResult struct {
	Status  string      `json:"status"`
	Payload turnPayload `json:"payload"`
}

type turnPayload struct {
	Reason         string       `json:"reason"`
	Message        string       `json:"message"`
	RootCause      string       `json:"rootCause"`
	MessageKey     string       `json:"messageKey"`
	HTTPStatus     int          `json:"httpStatus"`
	Output         []outputItem `json:"output"`
	ID             string       `json:"id"`
	ConversationID string       `json:"conversationId"`
}

type outputItem struct {
	Type    string        `json:"type"`
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
	Summary []contentPart `json:"summary"`
}

type contentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

// Text concatenates the assistant's visible answer. `output` interleaves
// messages, reasoning summaries, and tool calls, so everything else is skipped.
func (p *turnPayload) Text() string {
	var b strings.Builder
	for _, item := range p.Output {
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		for _, part := range item.Content {
			switch part.Type {
			case "output_text":
				b.WriteString(part.Text)
			case "refusal":
				b.WriteString(part.Refusal)
			}
		}
	}
	return b.String()
}

// reasonSandboxReconnecting is reported as response.status=="error" but is not
// fatal: the turn resumes once the sandbox is ready.
//
// The wire spells this snake_case ("sandbox_reconnecting", measured live
// 2026-09-17) while the frontend enum spells it "SandboxReconnecting", so the
// comparison goes through sameReason rather than string equality. Matching the
// CamelCase form literally meant every such turn was surfaced as a hard failure
// instead of being retried.
const reasonSandboxReconnecting = "SandboxReconnecting"

const reasonConversationTooLarge = "ConversationTooLarge"

// sameReason compares a wire `reason` against a frontend enum name, ignoring
// case and separators so snake_case and CamelCase spellings both match.
func sameReason(wireValue, enumName string) bool {
	normalize := func(s string) string {
		s = strings.ToLower(s)
		s = strings.ReplaceAll(s, "_", "")
		s = strings.ReplaceAll(s, "-", "")
		return strings.ReplaceAll(s, " ", "")
	}
	return normalize(wireValue) == normalize(enumName)
}

type turnRequest struct {
	Input              []map[string]any `json:"input"`
	PreviousResponseID string           `json:"previousResponseId,omitempty"`
	Metadata           map[string]any   `json:"metadata"`
	ConversationID     string           `json:"conversationId,omitempty"`
}

func (c *prismClient) startTurn(ctx context.Context, body turnRequest) (*turnResponse, error) {
	var out turnResponse
	if err := c.do(ctx, http.MethodPost, "/api/llm/response_with_tools_start", body, &out); err != nil {
		return nil, err
	}
	if pe := terminalTurnError(&out); pe != nil {
		return nil, pe
	}
	if strings.TrimSpace(out.RequestID) == "" {
		return nil, fmt.Errorf("response_with_tools_start 未返回 request_id")
	}
	if out.Status == "started" && len(out.TurnState) == 0 {
		return nil, fmt.Errorf("response_with_tools_start 缺少 turn_state")
	}
	return &out, nil
}

func (c *prismClient) pollTurn(ctx context.Context, requestID string, turnState json.RawMessage) (*turnResponse, error) {
	body := map[string]any{"request_id": requestID, "turn_state": turnState}
	var out turnResponse
	if err := c.do(ctx, http.MethodPost, "/api/llm/response_with_tools_status", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// runTurn drives one full assistant turn: start, then poll until the server
// reports completion. Mirrors the web client's start/poll/reconnect loop — a
// SandboxReconnecting result is retried rather than surfaced as a failure.
func (c *prismClient) runTurn(ctx context.Context, req turnRequest, poll time.Duration, onProgress func(string)) (*turnPayload, error) {
	if poll <= 0 {
		poll = 5 * time.Second
	}
	for attempt := 0; attempt < 3; attempt++ {
		payload, err := c.runOnce(ctx, req, poll)
		if err == nil {
			return payload, nil
		}
		if !errors.Is(err, errSandboxReconnecting) {
			return nil, err
		}
		if onProgress != nil {
			onProgress("等待 prism 沙箱就绪…")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
	return nil, fmt.Errorf("prism 沙箱反复重连失败，请稍后重试")
}

func (c *prismClient) runOnce(ctx context.Context, req turnRequest, poll time.Duration) (*turnPayload, error) {
	resp, err := c.startTurn(ctx, req)
	if err != nil {
		return nil, err
	}
	for {
		if resp.Status == "completed" {
			if resp.Response == nil {
				return nil, fmt.Errorf("response_with_tools_start 已完成但没有 response")
			}
			return interpret(resp.Response)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}

		next, err := c.pollTurn(ctx, resp.RequestID, resp.TurnState)
		if err != nil {
			return nil, err
		}
		if pe := terminalTurnError(next); pe != nil {
			return nil, pe
		}
		if next.Status == "pending" && len(next.TurnState) == 0 {
			return nil, fmt.Errorf("response_with_tools_status 的 pending 缺少 turn_state")
		}
		resp = next
	}
}

// interpret turns a terminal turn result into a payload, translating the
// recoverable SandboxReconnecting error into a sentinel the caller retries on.
func interpret(res *turnResult) (*turnPayload, error) {
	if res.Status == "success" {
		return &res.Payload, nil
	}
	if sameReason(res.Payload.Reason, reasonSandboxReconnecting) {
		return nil, errSandboxReconnecting
	}
	msg := res.Payload.Message
	if msg == "" {
		msg = res.Payload.Reason
	}
	return nil, &prismError{
		Status:     res.Payload.HTTPStatus,
		Reason:     res.Payload.Reason,
		Message:    msg,
		RootCause:  res.Payload.RootCause,
		MessageKey: res.Payload.MessageKey,
	}
}

var errSandboxReconnecting = fmt.Errorf("sandbox reconnecting")

type prismError struct {
	Status     int
	Reason     string
	Message    string
	RootCause  string
	MessageKey string
}

func (e *prismError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Reason
}

// HTTPStatus maps a prism failure onto the status CPA should surface.
func (e *prismError) HTTPStatus() int {
	if e.Status >= 400 && e.Status <= 599 {
		return e.Status
	}
	switch {
	case sameReason(e.Reason, reasonConversationTooLarge):
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusBadGateway
	}
}

// ------------------------------------------------------------- sandbox

// sandboxSession is a sandbox that prism's agent can actually run in. Reaching
// that state takes far more than POST /api/backend/1/new: the sandbox also has
// to be handed the project's resource token, then the y-sweet token, and it
// only reports ready once a Yjs provider is synced over a live socket.
//
// Skipping any of those leaves wait-for-sync at "syncing" and the turn dies in
// prism's own gateway timeout:
//
//	codex_v2_restore_start failed (504 Gateway Timeout)
//	"504: Timed out waiting for sandbox workspace file synchronization."
//
// Measured 2026-09-17; the full handshake is documented in docs/prism-protocol.md A8.3.
type sandboxSession struct {
	// URL is the public sandbox proxy base with a trailing slash. This form
	// goes into metadata; the server substitutes its internal address itself.
	URL   string
	Token string
	// bust is prism's cache-buster query parameter, constant per sandbox.
	bust string
	// yjs holds the provider socket open. It must outlive the turn: dropping it
	// puts the sandbox back into "syncing".
	yjs *yjsSession

	expires time.Time
}

func (s *sandboxSession) urlWithBust(path string) string {
	return s.URL + path + "?prism_cache_bust=" + s.bust
}

// yjsCredentials is the POST /api/y response. The sandbox is configured by
// forwarding this entire object to its /token endpoint.
type yjsCredentials struct {
	URL           string `json:"url"`
	BaseURL       string `json:"baseUrl"`
	DocID         string `json:"docId"`
	Token         string `json:"token"`
	Authorization string `json:"authorization"`
}

// sandboxTTL is a conservative reuse window: re-provisioning costs seconds, and
// the y-sweet socket is only useful while its sandbox lives.
const sandboxTTL = 10 * time.Minute

// waitForSyncBudget bounds how long we wait for the workspace to report synced.
const waitForSyncBudget = 60 * time.Second

var (
	sandboxMu    sync.Mutex
	sandboxCache = map[string]*sandboxSession{}
)

// ensureSandbox returns a ready sandbox for the project, provisioning and
// syncing a new one when the cached entry is missing or stale.
func (c *prismClient) ensureSandbox(ctx context.Context, projectID string) (*sandboxSession, error) {
	sandboxMu.Lock()
	if cached, ok := sandboxCache[projectID]; ok && time.Now().Before(cached.expires) {
		sandboxMu.Unlock()
		return cached, nil
	}
	stale := sandboxCache[projectID]
	delete(sandboxCache, projectID)
	sandboxMu.Unlock()
	if stale != nil {
		stale.yjs.close()
	}

	session, err := c.provisionSandbox(ctx, projectID)
	if err != nil {
		return nil, err
	}

	sandboxMu.Lock()
	sandboxCache[projectID] = session
	sandboxMu.Unlock()
	return session, nil
}

// provisionSandbox runs the whole pre-turn handshake.
func (c *prismClient) provisionSandbox(ctx context.Context, projectID string) (*sandboxSession, error) {
	var created struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/backend/1/new", nil, &created); err != nil {
		return nil, fmt.Errorf("领取 prism 沙箱失败: %w", err)
	}
	if created.URL == "" || created.Token == "" {
		return nil, fmt.Errorf("领取 prism 沙箱返回缺少 url/token")
	}
	session := &sandboxSession{URL: created.URL, Token: created.Token, bust: newCacheBust()}
	if !strings.HasSuffix(session.URL, "/") {
		session.URL += "/"
	}

	// The sandbox has to be handed the project's files and a y-sweet provider
	// before prism will run the agent in it.
	if err := c.grantResourceToken(ctx, session, projectID); err != nil {
		return nil, err
	}
	creds, err := c.grantYjsToken(ctx, session, projectID)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/%s?token=%s", creds.URL, creds.DocID, url.QueryEscape(creds.Token))
	yjs, err := dialYjs(endpoint, creds.Token)
	if err != nil {
		return nil, err
	}
	session.yjs = yjs

	if err := c.waitForSync(ctx, session); err != nil {
		yjs.close()
		return nil, err
	}
	session.expires = time.Now().Add(sandboxTTL)
	return session, nil
}

// grantResourceToken hands the sandbox a resource token so it can pull the
// project's files. Both hops are required: prism mints the token, the sandbox
// consumes it.
func (c *prismClient) grantResourceToken(ctx context.Context, session *sandboxSession, projectID string) error {
	var minted struct {
		AccessToken      string `json:"access_token"`
		ResourcesBaseURL string `json:"resources_base_url"`
	}
	err := c.do(ctx, http.MethodPost, "/api/projects/"+projectID+"/sandbox/resources-token", map[string]any{
		"sandbox_session_id": nil,
		"sandbox_token":      session.Token,
	}, &minted)
	if err != nil {
		return fmt.Errorf("申请项目资源令牌失败: %w", err)
	}
	if minted.AccessToken == "" {
		return fmt.Errorf("申请项目资源令牌返回缺少 access_token")
	}
	base := minted.ResourcesBaseURL
	if base == "" {
		base = baseURL + "/s/sandbox-resources/"
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}

	var ack struct {
		Status string `json:"status"`
	}
	err = c.doSandbox(ctx, http.MethodPost, session.urlWithBust("resources-token"), session.Token, map[string]any{
		"token":           minted.AccessToken,
		"resourceBaseUrl": base,
		"projectId":       projectID,
	}, &ack)
	if err != nil {
		return fmt.Errorf("把资源令牌交给沙箱失败: %w", err)
	}
	if ack.Status != "" && ack.Status != "success" {
		return fmt.Errorf("把资源令牌交给沙箱被拒: %s", ack.Status)
	}
	return nil
}

// grantYjsToken mints the y-sweet credentials and forwards them verbatim to the
// sandbox's /token endpoint. Without this the sandbox never reports
// hasCurrentYSweetToken and wait-for-sync stays at "syncing" forever.
func (c *prismClient) grantYjsToken(ctx context.Context, session *sandboxSession, projectID string) (*yjsCredentials, error) {
	var creds yjsCredentials
	if err := c.do(ctx, http.MethodPost, "/api/y", map[string]any{"docId": projectID}, &creds); err != nil {
		return nil, fmt.Errorf("获取 y-sweet 凭据失败: %w", err)
	}
	if creds.URL == "" || creds.Token == "" || creds.DocID == "" {
		return nil, fmt.Errorf("y-sweet 凭据缺少 url/docId/token")
	}

	var ack struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := c.doSandbox(ctx, http.MethodPost, session.urlWithBust("token"), session.Token, creds, &ack); err != nil {
		return nil, fmt.Errorf("把 y-sweet 凭据交给沙箱失败: %w", err)
	}
	if !ack.Success {
		return nil, fmt.Errorf("把 y-sweet 凭据交给沙箱被拒: %s", ack.Message)
	}
	return &creds, nil
}

// waitForSync blocks until the sandbox reports the workspace synced.
func (c *prismClient) waitForSync(ctx context.Context, session *sandboxSession) error {
	deadline := time.Now().Add(waitForSyncBudget)
	var last string
	for {
		var out struct {
			Status string `json:"status"`
		}
		if err := c.doSandbox(ctx, http.MethodGet,
			session.urlWithBust("wait-for-sync")+"&wait_ms=10000",
			session.Token, nil, &out); err != nil {
			return fmt.Errorf("等待沙箱工作区同步失败: %w", err)
		}
		last = out.Status
		if out.Status == "synced" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("沙箱工作区同步超时（wait-for-sync 仍为 %q）", last)
		}
	}
}

// dropSandbox invalidates the cached sandbox for a project so the next call
// provisions a fresh one (used when the server reports the session expired).
func dropSandbox(projectID string) {
	sandboxMu.Lock()
	stale := sandboxCache[projectID]
	delete(sandboxCache, projectID)
	sandboxMu.Unlock()
	if stale != nil {
		stale.yjs.close()
	}
}

// newCacheBust mirrors prism's own cache-buster: a fixed random number per
// sandbox, carried through the sandbox proxy calls.
func newCacheBust() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000_000_000)
	}
	return fmt.Sprintf("%d", binary.BigEndian.Uint64(b[:])%1_000_000_000_000_000)
}

// ------------------------------------------------------------- transport

// do issues a request against the prism origin.
func (c *prismClient) do(ctx context.Context, method, path string, body any, out any) error {
	return c.doRequest(ctx, method, baseURL+path, body, nil, out)
}

// doSandbox issues a request through the sandbox proxy, which needs the sandbox
// token header and lives outside the origin's /api namespace.
func (c *prismClient) doSandbox(ctx context.Context, method, target, sandboxToken string, body, out any) error {
	return c.doRequest(ctx, method, target, body, map[string]string{
		"X-Crixet-Sandbox-Token": sandboxToken,
	}, out)
}

func (c *prismClient) doRequest(ctx context.Context, method, target string, body any, extra map[string]string, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", c.cookies)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Referer", c.referer())
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	for name, value := range extra {
		req.Header.Set(name, value)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		if resp.Header.Get("x-prism-auth-error") == "missing-openai-token" {
			return fmt.Errorf("prism 缺少 OpenAI access token cookie，请重新登录后更新 cookie")
		}
		// prism reports several distinct failures as 401, and the body says which
		// one; dropping it produced a misleading "session expired" for all of them.
		detail := strings.TrimSpace(string(raw))
		if len(detail) > 300 {
			detail = detail[:300]
		}
		if detail == "" {
			detail = resp.Header.Get("x-prism-auth-error")
		}
		if detail == "" {
			detail = "响应体为空"
		}
		return fmt.Errorf("prism %s 返回 401: %s", target, detail)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Cloudflare answers a flagged client with an HTML block page. The
		// markup is hundreds of bytes of noise that buries the one fact the
		// operator needs, so name the cause instead of forwarding it verbatim.
		//
		// Measured 2026-09-17: this page means the *egress IP* is flagged, not
		// that the cookie is bad. The identical request through a US exit
		// returned 200 while the direct one got this 403, so routing the
		// process through a proxy is the fix; adding cf_clearance is not.
		if isCloudflareChallenge(raw) {
			return fmt.Errorf("prism %s 被 Cloudflare 拦截 (HTTP %d)：出口 IP 被判定为异常流量。"+
				"实测同一请求经美国出口即 200，与 cookie 无关——请给 CPA 进程设置 HTTPS_PROXY 走代理"+
				"（插件使用 Go 默认 transport，会自动读取该环境变量）", target, resp.StatusCode)
		}
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return fmt.Errorf("prism %s 返回 %d: %s", target, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// isCloudflareChallenge reports whether an error body is one of Cloudflare's
// HTML challenge pages rather than a prism JSON error.
func isCloudflareChallenge(raw []byte) bool {
	head := raw
	if len(head) > 2048 {
		head = head[:2048]
	}
	lower := strings.ToLower(string(head))
	return strings.Contains(lower, "<!doctype html") ||
		strings.Contains(lower, "<html") ||
		strings.Contains(lower, "cloudflare")
}

func (c *prismClient) referer() string {
	if c.projectID == "" {
		return baseURL + "/"
	}
	return fmt.Sprintf("%s/?u=%s&pg=1&m=main.tex", baseURL, c.projectID)
}
