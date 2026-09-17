package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// storedAuth is the credential payload this plugin owns. It lives in the host
// auth file as StorageJSON, base64-wrapped on the wire.
//
// Prism authenticates purely with cookies, so the only durable secret is the
// cookie header. userID/projectID are cached here after first use so a cold
// start does not have to re-probe them.
type storedAuth struct {
	Cookies   string `json:"cookies"`
	UserID    string `json:"userId,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
}

// rememberProjectID writes the project id back into the credential file so it
// survives a restart. Re-deriving it costs a lookup against prism's project
// list, which is both slower and, measured 2026-09-17, the one endpoint that
// kept timing out while the rest of prism answered normally.
func rememberProjectID(sa *storedAuth, projectID string) {
	if sa == nil || projectID == "" || sa.ProjectID == projectID {
		return
	}
	sa.ProjectID = projectID
	auth := &authData{
		Provider:    providerName,
		Label:       "prism (browser sign-in)",
		Prefix:      "prism",
		FileName:    credentialFileName,
		StorageJSON: sa.encode(),
	}
	if err := saveHostAuth(auth); err != nil {
		// Losing this only costs a lookup next start; the turn itself is fine.
		hostLog("warn", "把项目 ID 写回凭据失败 error="+err.Error(), nil)
		return
	}
	hostLog("info", "项目 ID 已写回凭据 project="+projectID, nil)
}

// importFile is the shape accepted by `auth.parse` — a hand-written JSON file
// the operator drops into CPA's auth directory.
type importFile struct {
	Cookies   string `json:"cookies"`
	UserID    string `json:"userId"`
	ProjectID string `json:"projectId"`
	Label     string `json:"label"`
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("凭据为空：请先在 CPA 中导入 prism 的 cookie")
	}
	var sa storedAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("解析凭据失败: %w", err)
	}
	if strings.TrimSpace(sa.Cookies) == "" {
		// Fall back to the plugin config so a single-account setup only has to
		// put the cookie in config.yaml.
		sa.Cookies = currentConfig().Cookies
	}
	if sa.Cookies == "" {
		return nil, fmt.Errorf("凭据缺少 cookies 字段")
	}
	return &sa, nil
}

func (sa *storedAuth) encode() []byte {
	raw, _ := json.Marshal(sa)
	return raw
}

// handleParseAuth recognises credential files this plugin owns. Prism cookies
// are not a standard OAuth blob, so we accept any JSON object carrying a
// non-empty `cookies` string.
func handleParseAuth(request []byte) ([]byte, error) {
	var req authParseRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode auth.parse: %w", err)
	}
	// Sign-in completion arrives here by two routes, both of which end up as a
	// file in the auth directory:
	//
	//  1. The host's own callback box ("paste the redirect URL"): it validates the
	//     state against the session auth.login.start created and writes
	//     {"code":…,"state":…} as .oauth-<provider>-<state>.oauth.
	//  2. The operator pasting the callback URL straight into a file, which may
	//     be any plain text (a bare URL, a "Copy as cURL" line, a HAR fragment).
	var callback struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(req.RawJSON, &callback); err == nil && strings.TrimSpace(callback.Code) != "" {
		auth, err := completeLoginWithState(context.Background(), callback.Code, callback.State, false)
		if err != nil {
			return errorEnvelope("authentication_error", err.Error(), http.StatusBadRequest), nil
		}
		return okEnvelope(authParseResponse{Handled: true, Auth: auth})
	}
	if !json.Valid(req.RawJSON) {
		if _, isCallback := callbackCode(string(req.RawJSON)); isCallback {
			auth, err := completeLogin(context.Background(), string(req.RawJSON), false)
			if err != nil {
				return errorEnvelope("authentication_error", err.Error(), http.StatusBadRequest), nil
			}
			return okEnvelope(authParseResponse{Handled: true, Auth: auth})
		}
	}

	var f importFile
	if err := json.Unmarshal(req.RawJSON, &f); err != nil || strings.TrimSpace(f.Cookies) == "" {
		return okEnvelope(authParseResponse{Handled: false})
	}
	label := f.Label
	if label == "" {
		label = "prism"
	}
	return okEnvelope(authParseResponse{
		Handled: true,
		Auth: &authData{
			Provider: providerName,
			Label:    label,
			// Prefix is a namespace *segment*, not a string prefix: the host
			// composes "<Prefix>/<modelID>" (internal/config: e.g. "teamA" ->
			// "teamA/claude-sonnet-4"). A trailing separator here would yield a
			// dangling alias, so this must stay unprefixed.
			Prefix: "prism",
			StorageJSON: (&storedAuth{
				Cookies:   strings.TrimSpace(f.Cookies),
				UserID:    strings.TrimSpace(f.UserID),
				ProjectID: strings.TrimSpace(f.ProjectID),
			}).encode(),
		},
	})
}

// handleRefreshAuth validates the stored cookie against /auth/session and
// caches the discovered user id. Prism cookies are long-lived, so the next
// refresh is scheduled far out.
func handleRefreshAuth(request []byte) ([]byte, error) {
	var req authRefreshRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode auth.refresh: %w", err)
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	client, err := newPrismClient(sa)
	if err != nil {
		return nil, err
	}
	session, err := client.session()
	if err != nil {
		return errorEnvelope("authentication_error", err.Error(), http.StatusUnauthorized), nil
	}
	if session.UserTier != "logged_in" || session.User.ID == "" {
		return errorEnvelope("authentication_error",
			"prism 会话未登录：cookie 可能已失效", http.StatusUnauthorized), nil
	}
	if sa.UserID == "" {
		sa.UserID = session.OpenAIUserID()
	}

	meta := map[string]any{}
	for k, v := range req.Metadata {
		meta[k] = v
	}
	meta["email"] = session.User.Email
	meta["prism_user_id"] = session.Policy.User.PrismUserID

	return okEnvelope(authRefreshResponse{
		Auth: authData{
			Provider:         providerName,
			ID:               req.AuthID,
			StorageJSON:      sa.encode(),
			Metadata:         meta,
			NextRefreshAfter: time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339),
		},
		NextRefreshAfter: time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339),
	})
}

func handleModelStatic(_ []byte) ([]byte, error) {
	// No credential here, so this can only answer from cache or config.
	return okEnvelope(modelResponse{Provider: providerName, Models: availableModels(nil)})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req authModelRequest
	if err := json.Unmarshal(raw, &req); err != nil || len(req.StorageJSON) == 0 {
		return okEnvelope(modelResponse{Provider: providerName, Models: availableModels(nil)})
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return okEnvelope(modelResponse{Provider: providerName, Models: availableModels(nil)})
	}
	return okEnvelope(modelResponse{Provider: providerName, Models: availableModels(sa)})
}

// availableModels resolves the advertised model list:
//
//  1. an explicit `models:` list in the plugin config (operator override),
//  2. the list prism publishes for this credential (cached for catalogTTL),
//  3. the last list fetched for any credential,
//  4. fallbackModel.
//
// Advertising the upstream list is what keeps the plugin usable across model
// rotations: CPA only routes models its providers advertise, so a stale
// hardcoded name is unreachable *and* a name that no longer exists upstream
// fails the turn with an opaque 400.
func availableModels(sa *storedAuth) []modelInfo {
	cfg := currentConfig()
	if len(cfg.Models) > 0 {
		return modelInfos(cfg.Models)
	}

	var catalog *modelCatalog
	if sa != nil {
		if client, err := newPrismClient(sa); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
			defer cancel()
			if fetched, err := client.catalogFor(ctx); err == nil {
				catalog = fetched
			}
		}
	}
	if catalog == nil {
		catalog = defaultCatalog()
	}
	if catalog != nil {
		ids := make([]string, 0, len(catalog.Models))
		for _, m := range catalog.Models {
			ids = append(ids, m.ID)
		}
		return modelInfos(ids)
	}
	if cfg.DefaultModel != "" {
		return modelInfos([]string{cfg.DefaultModel})
	}
	return modelInfos([]string{fallbackModel})
}

func modelInfos(ids []string) []modelInfo {
	out := make([]modelInfo, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		out = append(out, modelInfo{
			ID:            id,
			Object:        "model",
			OwnedBy:       providerName,
			Type:          "chat",
			DisplayName:   id,
			Description:   "prism.openai.com 网页订阅",
			ContextLength: 200000,
			UserDefined:   true,
		})
	}
	return out
}
