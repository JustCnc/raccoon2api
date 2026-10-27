package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *app) managementRegistration() managementRegistration {
	prefix := "/" + providerKey
	return managementRegistration{
		Routes: []managementRouteSpec{
			{Method: "GET", Path: prefix + "/status"},
			{Method: "GET", Path: prefix + "/models"},
			{Method: "GET", Path: prefix + "/accounts"},
			{Method: "POST", Path: prefix + "/accounts/reset"},
			{Method: "POST", Path: prefix + "/accounts/import"},
			{Method: "POST", Path: prefix + "/accounts/add"},
			{Method: "POST", Path: prefix + "/accounts/delete"},
			{Method: "GET", Path: prefix + "/accounts/export"},
			{Method: "GET", Path: prefix + "/quota"},
			{Method: "POST", Path: prefix + "/quota/check"},
			{Method: "POST", Path: prefix + "/quota/grant"},
			{Method: "POST", Path: prefix + "/accounts/refresh"},
			{Method: "POST", Path: prefix + "/login/start"},
			{Method: "POST", Path: prefix + "/login/complete"},
			{Method: "POST", Path: prefix + "/login/poll"},
			{Method: "POST", Path: prefix + "/catalog/refresh"},
			{Method: "POST", Path: prefix + "/test"},
			{Method: "GET", Path: prefix + "/requests"},
			{Method: "GET", Path: prefix + "/logs"},
			{Method: "GET", Path: prefix + "/config"},
		},
		Resources: []resourceRouteSpec{
			{Path: "/console", Menu: "Raccoon Office", Description: "小浣熊反代控制台：账号池、登录态、模型目录与日志。"},
		},
	}
}

// managementRoutes is the closed set of paths this plugin answers. Matching on
// the suffix keeps working when the host derives the plugin id from the library
// file name (raccoon2api-linux-amd64.so) instead of the provider key.
var managementRoutes = []string{
	"/console",
	"/status",
	"/models",
	"/accounts/reset",
	"/accounts/import",
	"/accounts/add",
	"/accounts/delete",
	"/accounts/export",
	"/quota/check",
	"/quota/grant",
	"/quota",
	"/accounts/refresh",
	"/accounts",
	"/login/start",
	"/login/complete",
	"/login/poll",
	"/catalog/refresh",
	"/test",
	"/requests",
	"/logs",
	"/config",
}

// normalizeManagementPath maps a host-supplied path onto a route key.
func normalizeManagementPath(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if idx := strings.Index(p, providerKey); idx >= 0 {
		trimmed := p[idx+len(providerKey):]
		if trimmed == "" {
			p = "/"
		} else {
			p = trimmed
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
		}
	}
	for _, route := range managementRoutes {
		if p == route || strings.HasSuffix(p, route) {
			return route
		}
	}
	return p
}

func (a *app) handleManagement(req managementRequest) managementResponse {
	path := normalizeManagementPath(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	switch {
	case method == "GET" && path == "/console":
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":  []string{"text/html; charset=utf-8"},
				"Cache-Control": []string{"no-store"},
			},
			Body: consoleHTML,
		}
	case method == "GET" && (path == "/" || path == "/status"):
		return jsonResponse(a.statusPayload())
	case method == "GET" && path == "/models":
		return jsonResponse(map[string]any{
			"provider": providerKey,
			"source":   a.snapshotCatalog().Source,
			"models":   a.snapshotCatalog().Models,
			"exposed":  a.modelInfoList(),
		})
	case method == "GET" && path == "/accounts":
		return jsonResponse(map[string]any{"accounts": a.accountViews()})
	case method == "POST" && path == "/accounts/reset":
		return jsonResponse(a.resetAccountPayload(req))
	case method == "POST" && path == "/accounts/import":
		return jsonResponse(a.importAccountsPayload())
	case method == "POST" && path == "/accounts/add":
		return jsonResponse(a.addAccountPayload(req))
	case method == "POST" && path == "/accounts/delete":
		return jsonResponse(a.deleteAccountPayload(req))
	case method == "GET" && path == "/accounts/export":
		return a.exportAccounts(req)
	case method == "GET" && path == "/quota":
		return jsonResponse(map[string]any{
			"summary":  a.quotaSummary(),
			"accounts": a.accountViews(),
		})
	case method == "POST" && path == "/quota/check":
		return jsonResponse(a.quotaCheckPayload(req))
	case method == "POST" && path == "/quota/grant":
		return jsonResponse(a.dailyGrantPayload(req))
	case method == "POST" && path == "/accounts/refresh":
		return jsonResponse(a.refreshAccountPayload(req))
	case method == "POST" && path == "/login/start":
		return jsonResponse(a.loginStartPayload())
	case method == "POST" && path == "/login/complete":
		return jsonResponse(a.loginCompletePayload(req))
	case method == "POST" && path == "/login/poll":
		return jsonResponse(a.loginPollPayload(req))
	case method == "POST" && path == "/catalog/refresh":
		return jsonResponse(a.catalogPayload())
	case method == "POST" && path == "/test":
		return jsonResponse(a.testPayload(req))
	case method == "GET" && path == "/requests":
		limit := 100
		if raw := req.Query.Get("limit"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 300 {
				limit = parsed
			}
		}
		return jsonResponse(map[string]any{
			"summary": a.requests.summary(),
			"entries": a.requests.tail(limit),
		})
	case method == "GET" && path == "/logs":
		limit := 200
		if raw := req.Query.Get("limit"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 2000 {
				limit = parsed
			}
		}
		return jsonResponse(map[string]any{"entries": a.log.tail(limit)})
	case method == "GET" && path == "/config":
		return jsonResponse(map[string]any{"config": a.sanitizedConfig()})
	default:
		return managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"error":"unknown route"}`),
		}
	}
}

func jsonResponse(v any) managementResponse {
	raw, err := json.Marshal(v)
	if err != nil {
		raw = []byte(`{"error":"encode failed"}`)
	}
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       raw,
	}
}

type accountView struct {
	ID             string `json:"id"`
	Alias          string `json:"alias"`
	Subject        string `json:"subject,omitempty"`
	Source         string `json:"source,omitempty"`
	Priority       int    `json:"priority"`
	OfficeIdentity string `json:"office_identity,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	ExpiresIn      int64  `json:"expires_in_seconds"`
	HasRefresh     bool   `json:"has_refresh_token"`
	TokenTail      string `json:"token_tail,omitempty"`
	Disabled       bool   `json:"disabled"`
	Dead           bool   `json:"dead"`
	Cooling        bool   `json:"cooling"`
	CooldownUntil  string `json:"cooldown_until,omitempty"`
	CooldownLeft   int64  `json:"cooldown_left_seconds"`
	LastError      string `json:"last_error,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`

	QuotaState     string `json:"quota_state,omitempty"`
	QuotaCheckedAt string `json:"quota_checked_at,omitempty"`
	QuotaNote      string `json:"quota_note,omitempty"`
	// Credits reported by the upstream points API, in 积分.
	PointsAvailable  int64 `json:"points_available"`
	PointsDaily      int64 `json:"points_daily"`
	PointsReward     int64 `json:"points_reward"`
	PointsTopup      int64 `json:"points_topup"`
	PointsChargeable int64 `json:"points_chargeable"`
	// Daily gift: the day it was claimed ("" = never) and what it handed out.
	DailyGrantDay    string `json:"daily_grant_day,omitempty"`
	DailyGrantPoints int64  `json:"daily_grant_points"`
	DailyGrantAt     string `json:"daily_grant_at,omitempty"`
	DailyGrantToday  bool   `json:"daily_grant_today"`
	Requests         int64  `json:"requests"`
	Failures         int64  `json:"failures"`
	LastUsedAt       string `json:"last_used_at,omitempty"`
	CPAFile          string `json:"cpa_file"`
}

func (a *app) accountViews() []accountView {
	now := time.Now()
	accounts := a.poolSnapshot()
	views := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		view := accountView{
			ID:             account.ID,
			Alias:          account.Alias,
			Subject:        account.Subject,
			Source:         account.Source,
			Priority:       account.Priority,
			OfficeIdentity: account.OfficeIdentity,
			HasRefresh:     account.RefreshToken != "",
			TokenTail:      tailOf(account.AccessToken),
			Disabled:       account.Disabled,
			Dead:           account.Dead,
			Cooling:        account.cooling(now),
			CooldownLeft:   int64(account.cooldownRemaining(now).Seconds()),
			LastError:      account.LastError,
			QuotaState:     account.QuotaState,
			QuotaNote:      account.QuotaNote,

			PointsAvailable:  account.PointsAvailable,
			PointsDaily:      account.PointsDaily,
			PointsReward:     account.PointsReward,
			PointsTopup:      account.PointsTopup,
			PointsChargeable: account.PointsChargeable,
			DailyGrantDay:    account.DailyGrantDay,
			DailyGrantPoints: account.DailyGrantPoints,
			DailyGrantToday:  account.DailyGrantDay == grantDay(now),
			Requests:         account.Requests,
			Failures:         account.Failures,
			CPAFile:          account.ID + ".json",
		}
		if account.QuotaCheckedAt > 0 {
			view.QuotaCheckedAt = time.UnixMilli(account.QuotaCheckedAt).UTC().Format(time.RFC3339)
		}
		if account.LastUsedAt > 0 {
			view.LastUsedAt = time.UnixMilli(account.LastUsedAt).UTC().Format(time.RFC3339)
		}
		if account.DailyGrantAt > 0 {
			view.DailyGrantAt = time.UnixMilli(account.DailyGrantAt).UTC().Format(time.RFC3339)
		}
		if account.ExpiresAt > 0 {
			view.ExpiresAt = account.expiresAtTime().UTC().Format(time.RFC3339)
			view.ExpiresIn = int64(time.Until(account.expiresAtTime()).Seconds())
		}
		if account.CooldownUntil > 0 {
			view.CooldownUntil = time.UnixMilli(account.CooldownUntil).UTC().Format(time.RFC3339)
		}
		if account.UpdatedAt > 0 {
			view.UpdatedAt = time.UnixMilli(account.UpdatedAt).UTC().Format(time.RFC3339)
		}
		views = append(views, view)
	}
	return views
}

func (a *app) statusPayload() map[string]any {
	cfg := a.currentConfig()
	snapshot := a.snapshotCatalog()
	now := time.Now()

	accounts := a.poolSnapshot()
	usable, cooling, disabled := 0, 0, 0
	for _, account := range accounts {
		switch {
		case account.Disabled || account.Dead:
			disabled++
		case account.cooling(now):
			cooling++
		default:
			usable++
		}
	}

	return map[string]any{
		"provider": providerKey,
		"version":  pluginVersion,
		"upstream": map[string]any{
			"base_url":      cfg.BaseURL,
			"auth_base_url": cfg.AuthBaseURL,
		},
		"fingerprint": map[string]any{
			"client_name":     cfg.ClientName,
			"client_platform": cfg.ClientPlatform,
			"client_version":  cfg.ClientVersion,
			"client_os":       cfg.ClientOSVersion,
			"client_channel":  cfg.ClientChannel,
			"device_id":       a.deviceID(cfg),
			"user_agent":      cfg.UserAgent,
			"sdk_version":     cfg.SDKVersion,
			"python_version":  cfg.PythonVersion,
		},
		"accounts": map[string]any{
			"total":    len(accounts),
			"usable":   usable,
			"cooling":  cooling,
			"disabled": disabled,
			"primary":  cfg.credentialID(),
		},
		"quota": a.quotaSummary(),
		"catalog": map[string]any{
			"source":     snapshot.Source,
			"fetched_at": snapshot.FetchedAt,
			"default":    snapshot.Default,
			"count":      len(snapshot.Models),
			"exposed":    len(a.modelInfoList()),
		},
		"metrics": a.metrics.snapshot(),
		"runtime": map[string]any{
			"data_dir":               cfg.DataDir,
			"accounts_dir":           cfg.AccountsDir,
			"max_tokens_default":     cfg.MaxTokensDefault,
			"request_timeout":        cfg.RequestTimeoutSeconds,
			"import_interval":        cfg.ImportInterval,
			"refresh_margin":         cfg.RefreshMargin,
			"refresh_seconds":        cfg.RefreshSeconds,
			"image_modalities":       cfg.ImageModalities,
			"reasoning_effort":       cfg.ReasoningEffort,
			"max_attempts":           cfg.MaxAttempts,
			"cooldown_seconds":       cfg.CooldownSeconds,
			"quota_cooldown_seconds": cfg.QuotaCooldownSeconds,
			"quota_probe_seconds":    cfg.QuotaProbeSeconds,
			"points_logging":         cfg.PointsLogging,
		},
	}
}

func (a *app) resetAccountPayload(req managementRequest) map[string]any {
	var body struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(req.Body, &body)
	if strings.TrimSpace(body.ID) == "" {
		// Reset everything when no account is named.
		count := 0
		for _, account := range a.poolSnapshot() {
			if a.resetAccount(account.ID) {
				count++
			}
		}
		return map[string]any{"ok": true, "reset": count, "message": "已重置全部账号的冷却与失效标记"}
	}
	if !a.resetAccount(body.ID) {
		return map[string]any{"ok": false, "message": "账号不存在：" + body.ID}
	}
	return map[string]any{"ok": true, "message": "已重置 " + body.ID}
}

func (a *app) importAccountsPayload() map[string]any {
	cfg := a.currentConfig()
	a.scanAccountsDir(cfg)
	a.syncAccountsFromHost()
	a.ensureHostRecords()
	return map[string]any{
		"ok":       true,
		"accounts": len(a.poolSnapshot()),
		"message":  "已重新读取账号目录并重新对齐 CPA 凭据",
	}
}

// addAccountPayload imports one account from a pasted or uploaded JSON blob.
// The body is either {"json": <object|string>, "alias", "priority"} or the
// credential object itself.
func (a *app) addAccountPayload(req managementRequest) map[string]any {
	var wrapper struct {
		JSON     json.RawMessage `json:"json"`
		Alias    string          `json:"alias"`
		Priority int             `json:"priority"`
		Verify   *bool           `json:"verify"`
	}
	if err := json.Unmarshal(req.Body, &wrapper); err != nil {
		return map[string]any{"ok": false, "message": "请求体不是合法 JSON"}
	}
	raw := bytes.TrimSpace(wrapper.JSON)
	switch {
	case len(raw) == 0:
		raw = bytes.TrimSpace(req.Body)
	case raw[0] == '"':
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return map[string]any{"ok": false, "message": "json 字段不是字符串"}
		}
		raw = bytes.TrimSpace([]byte(text))
	}
	if len(raw) == 0 {
		return map[string]any{"ok": false, "message": "没有收到账号 JSON"}
	}

	// An export file (array or {"accounts": [...]}) imports every account in it.
	if batch, ok := batchAccounts(raw); ok {
		return a.addAccountBatch(batch, wrapper.Verify)
	}

	var input credentialInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return map[string]any{"ok": false, "message": "账号 JSON 解析失败：" + err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A code-only JSON is a login, not a token import.
	if firstNonEmpty(input.AccessToken, input.Token, input.AuthToken) == "" {
		code := firstNonEmpty(input.AuthorizationCode, input.Code)
		if code == "" {
			return map[string]any{"ok": false, "message": "JSON 里没有 access_token 或 authorization_code"}
		}
		account, err := a.completeLogin(ctx, "", code, wrapper.Alias)
		if err != nil {
			return map[string]any{"ok": false, "message": err.Error()}
		}
		a.rememberCompletedCode(code, account)
		return map[string]any{
			"ok": true, "id": account.ID, "alias": account.Alias,
			"message": "已用授权码添加账号 " + account.Alias,
		}
	}

	account, errDecode := decodeCredential(raw)
	if errDecode != nil {
		return map[string]any{"ok": false, "message": errDecode.Error()}
	}
	cfg := a.currentConfig()
	if alias := sanitizeAlias(wrapper.Alias); alias != "" {
		account.Alias = alias
		account.ID = ""
	}
	if wrapper.Alias == "" && account.Alias == "" {
		account.Alias = subjectAlias(account.Subject)
	}
	if account.ID == "" {
		account.ID = accountIDForAlias(account.Alias)
	}
	if same := a.accountBySubject(account.Subject); same != nil && account.ID == accountIDForAlias(subjectAlias(account.Subject)) {
		// Same upstream account imported again without an explicit alias.
		account.ID = same.ID
		account.Alias = same.Alias
	}
	// An import without an explicit priority must not demote an existing entry.
	existing := a.poolAccount(account.ID)
	switch {
	case wrapper.Priority != 0:
		account.Priority = wrapper.Priority
	case account.Priority == 0 && existing == nil:
		account.Priority = fallbackPriority(cfg.CredentialPriority, 5)
	case existing != nil:
		account.Priority = existing.Priority
	}
	if account.Source == "" {
		if existing != nil && existing.AccessToken == account.AccessToken && existing.Source != "" {
			account.Source = existing.Source
		} else {
			account.Source = "import"
		}
	}
	account.UpdatedAt = time.Now().UnixMilli()

	if account.ExpiresAt > 0 && account.expiresAtTime().Before(time.Now()) {
		return map[string]any{"ok": false, "message": "这个 token 已经过期（" + account.expiresAtTime().Format(time.RFC3339) + "）"}
	}

	verify := true
	if wrapper.Verify != nil {
		verify = *wrapper.Verify
	}
	warning := ""
	if verify {
		if errVerify := a.verifyCredential(ctx, account); errVerify != nil {
			failure := asFailure(errVerify)
			if failure.Status == http.StatusUnauthorized || failure.Status == http.StatusForbidden {
				return map[string]any{"ok": false, "message": "上游拒绝了这个 token：" + failure.Message}
			}
			warning = "（未能验证：" + failure.Message + "）"
		}
	}

	a.upsertAccount(account)
	a.publishAccount(account, true)
	message := "已导入账号 " + account.Alias
	if warning != "" {
		message += warning
	}
	return map[string]any{
		"ok":         true,
		"id":         account.ID,
		"alias":      account.Alias,
		"subject":    account.Subject,
		"priority":   account.Priority,
		"expires_at": account.expiresAtTime().UTC().Format(time.RFC3339),
		"message":    message,
	}
}

// deleteAccountPayload removes an account from the pool and from the host's
// credential list.
func (a *app) deleteAccountPayload(req managementRequest) map[string]any {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"ok": false, "message": "请求体无效"}
	}
	id := strings.TrimSpace(body.ID)
	account := a.poolAccount(id)
	if account == nil {
		return map[string]any{"ok": false, "message": "账号不存在：" + id}
	}
	a.deleteAccount(id)
	a.log.info("accounts", "已删除账号 "+account.Alias+" 及其 CPA 凭据")
	return map[string]any{"ok": true, "message": "已删除账号 " + account.Alias}
}

// batchAccounts recognises an exported multi-account file.
func batchAccounts(raw []byte) ([]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if raw[0] == '[' {
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
			return nil, false
		}
		return list, true
	}
	var wrapper struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || len(wrapper.Accounts) == 0 {
		return nil, false
	}
	return wrapper.Accounts, true
}

// addAccountBatch imports every record of an exported file.
func (a *app) addAccountBatch(records []json.RawMessage, verify *bool) map[string]any {
	added := make([]string, 0, len(records))
	failed := make([]string, 0, len(records))
	for _, record := range records {
		body, err := json.Marshal(map[string]any{"json": json.RawMessage(record), "verify": verify})
		if err != nil {
			failed = append(failed, err.Error())
			continue
		}
		result := a.addAccountPayload(managementRequest{Method: "POST", Body: body})
		if result["ok"] == true {
			added = append(added, fmt.Sprint(result["alias"]))
			continue
		}
		failed = append(failed, fmt.Sprint(result["message"]))
	}
	message := fmt.Sprintf("已导入 %d 个账号", len(added))
	if len(failed) > 0 {
		message += fmt.Sprintf("，%d 个失败：%s", len(failed), strings.Join(failed, "；"))
	}
	return map[string]any{
		"ok":      len(added) > 0,
		"added":   added,
		"failed":  failed,
		"message": message,
	}
}

// exportAccounts returns the credential records so they can be moved to another
// install. A single account comes back as the bare record (importable as-is);
// the whole pool comes back as {"accounts": [...]}.
func (a *app) exportAccounts(req managementRequest) managementResponse {
	id := strings.TrimSpace(req.Query.Get("id"))
	if id != "" {
		account := a.poolAccount(id)
		if account == nil {
			return jsonResponse(map[string]any{"ok": false, "message": "账号不存在：" + id})
		}
		raw, err := json.MarshalIndent(a.exportRecord(account), "", "  ")
		if err != nil {
			return jsonResponse(map[string]any{"ok": false, "message": err.Error()})
		}
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":        []string{"application/json; charset=utf-8"},
				"Content-Disposition": []string{`attachment; filename="` + account.ID + `.json"`},
				"Cache-Control":       []string{"no-store"},
			},
			Body: append(raw, byte(10)),
		}
	}

	records := make([]credentialFile, 0, a.poolSize())
	for _, account := range a.poolSnapshot() {
		records = append(records, a.exportRecord(account))
	}
	raw, err := json.MarshalIndent(map[string]any{
		"provider":    providerKey,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"accounts":    records,
	}, "", "  ")
	if err != nil {
		return jsonResponse(map[string]any{"ok": false, "message": err.Error()})
	}
	name := "raccoon2api-accounts-" + time.Now().Format("20060102-1504") + ".json"
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":        []string{"application/json; charset=utf-8"},
			"Content-Disposition": []string{`attachment; filename="` + name + `"`},
			"Cache-Control":       []string{"no-store"},
		},
		Body: append(raw, byte(10)),
	}
}

// exportRecord is the portable shape: everything needed to rebuild the account,
// including the tokens (this endpoint is behind the management key).
func (a *app) exportRecord(account *credential) credentialFile {
	return credentialFile{
		Type:           providerKey,
		ID:             account.ID,
		Label:          credentialLabel(account),
		Alias:          account.Alias,
		AccessToken:    account.AccessToken,
		RefreshToken:   account.RefreshToken,
		ExpiresAt:      account.ExpiresAt,
		OfficeIdentity: account.OfficeIdentity,
		Subject:        account.Subject,
		SessionID:      account.SessionID,
		DeviceID:       account.DeviceID,
		Source:         account.Source,
		UpdatedAt:      account.UpdatedAt,
		Priority:       account.Priority,
		QuotaState:     account.QuotaState,
		QuotaCheckedAt: account.QuotaCheckedAt,
		QuotaNote:      account.QuotaNote,
		Requests:       account.Requests,
		Failures:       account.Failures,
		LastUsedAt:     account.LastUsedAt,
	}
}

// quotaCheckPayload probes one account (id) or the whole pool.
func (a *app) quotaCheckPayload(req managementRequest) map[string]any {
	var body struct {
		ID  string   `json:"id"`
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal(req.Body, &body)
	ids := body.IDs
	if id := strings.TrimSpace(body.ID); id != "" {
		if a.poolAccount(id) == nil {
			return map[string]any{"ok": false, "message": "账号不存在：" + id}
		}
		ids = []string{id}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	result := a.probeQuotaAll(ctx, ids)
	result["message"] = fmt.Sprintf("积分查询完成：正常 %d，积分不足 %d，未知 %d",
		result["healthy"], result["exhausted"], result["unknown"])
	return result
}

// dailyGrantPayload claims the daily login gift, for one account, a list, or all.
func (a *app) dailyGrantPayload(req managementRequest) map[string]any {
	var body struct {
		ID  string   `json:"id"`
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal(req.Body, &body)
	ids := body.IDs
	if id := strings.TrimSpace(body.ID); id != "" {
		ids = []string{id}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if len(ids) == 0 {
		summary := dailyGrantSummary(a.claimDailyPointsAll(ctx, nil))
		summary["ok"] = true
		summary["message"] = fmt.Sprintf("每日积分：新领取 %d 个账号，共 +%d 积分，失败 %d 个",
			summary["granted"], summary["points"], summary["failed"])
		return summary
	}

	results := make([]dailyGrantResult, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		account := a.poolAccount(id)
		if account == nil {
			results = append(results, dailyGrantResult{ID: id, Error: "账号不存在"})
			continue
		}
		results = append(results, a.claimDailyPoints(ctx, account))
	}
	summary := dailyGrantSummary(results)
	summary["ok"] = summary["failed"] == 0
	if len(results) == 1 {
		summary["message"] = dailyGrantMessage(results[0])
	} else {
		summary["message"] = fmt.Sprintf("每日积分：领取 %d 个，共 +%d 积分，失败 %d 个",
			summary["granted"], summary["points"], summary["failed"])
	}
	return summary
}

func dailyGrantMessage(result dailyGrantResult) string {
	switch {
	case result.Error != "":
		return "领取失败：" + result.Error
	case result.Granted:
		return fmt.Sprintf("已领取 %d 积分（%s）", result.Points, result.Day)
	default:
		return "今日已领取过（" + result.Day + "）"
	}
}

func (a *app) refreshAccountPayload(req managementRequest) map[string]any {
	var body struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(req.Body, &body)
	id := strings.TrimSpace(body.ID)
	if id == "" {
		id = a.currentConfig().credentialID()
	}
	account := a.poolAccount(id)
	if account == nil {
		return map[string]any{"ok": false, "message": "账号不存在：" + id}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	next, err := a.refreshAccount(ctx, account)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	return map[string]any{
		"ok":         true,
		"expires_at": next.expiresAtTime().UTC().Format(time.RFC3339),
		"message":    "账号 " + account.Alias + " 已刷新",
	}
}

func (a *app) loginStartPayload() map[string]any {
	session := a.newLoginSession()
	url := a.authorizeURL(session.State)
	a.log.info("login", "已生成授权链接："+url)
	callback := a.loginCallbackEnabled()
	message := "在浏览器打开链接登录，插件会自动接住回调；没有自动完成时把回调地址粘到下面"
	if !callback {
		message = "在浏览器打开链接登录，然后把回调地址里的 code 粘到下面"
	}
	return map[string]any{
		"ok":                    true,
		"url":                   url,
		"state":                 session.State,
		"expires_at":            session.Expires.UTC().Format(time.RFC3339),
		"callback":              callback,
		"callback_url":          fmt.Sprintf("http://127.0.0.1:%d%s", a.loginCallbackPort(), loginCallbackPath),
		"login_timeout_seconds": a.currentConfig().LoginTimeoutSeconds,
		"message":               message,
	}
}

func (a *app) loginCompletePayload(req managementRequest) map[string]any {
	var body struct {
		State    string `json:"state"`
		Code     string `json:"code"`
		Alias    string `json:"alias"`
		Priority int    `json:"priority"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"ok": false, "message": "请求体无效"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account, err := a.completeLogin(ctx, body.State, body.Code, body.Alias)
	if err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	if body.Priority != 0 {
		a.setPriority(account.ID, body.Priority)
		a.publishAccount(a.poolAccount(account.ID), true)
	}
	return map[string]any{
		"ok":      true,
		"id":      account.ID,
		"alias":   account.Alias,
		"subject": account.Subject,
		"message": "已添加账号 " + account.Alias,
	}
}

// loginPollPayload reports how a login session ended, so the console can close
// itself when the browser callback finished the exchange.
func (a *app) loginPollPayload(req managementRequest) map[string]any {
	var body struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"ok": false, "status": "error", "message": "请求体无效"}
	}
	session := a.loginSession(body.State)
	if session == nil {
		return map[string]any{"ok": true, "status": "error", "message": "登录会话不存在或已过期"}
	}
	if session.Auth != nil {
		return map[string]any{
			"ok":      true,
			"status":  "success",
			"message": "已添加账号 " + session.Auth.Alias,
			"id":      session.Auth.ID,
			"alias":   session.Auth.Alias,
		}
	}
	if session.Err != "" {
		return map[string]any{"ok": false, "status": "error", "message": session.Err}
	}
	if time.Now().After(session.Expires) {
		return map[string]any{"ok": true, "status": "expired", "message": "授权链接已过期，请重新生成"}
	}
	return map[string]any{"ok": true, "status": "pending", "message": "等待浏览器完成授权"}
}

func (a *app) catalogPayload() map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := a.refreshCatalog(ctx); err != nil {
		return map[string]any{"ok": false, "message": err.Error()}
	}
	return map[string]any{
		"ok":    true,
		"count": len(a.snapshotCatalog().Models),
	}
}

// testPayload sends one minimal completion upstream through one account.
func (a *app) testPayload(req managementRequest) map[string]any {
	var body struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	_ = json.Unmarshal(req.Body, &body)
	cfg := a.currentConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	account := a.poolAccount(strings.TrimSpace(body.ID))
	if account == nil {
		account = a.selectAccount(a.primaryID(), nil, time.Now())
	}
	if account == nil {
		return map[string]any{"ok": false, "message": "没有可用账号"}
	}
	cred, errFresh := a.ensureAccountFresh(ctx, account)
	if errFresh != nil && cred == nil {
		return map[string]any{"ok": false, "message": errFresh.Error()}
	}
	model, errModel := a.resolveModel(strings.TrimSpace(body.Model))
	if errModel != nil {
		return map[string]any{"ok": false, "message": errModel.Error()}
	}
	payload, _, errNorm := normalizeChatPayload([]byte(`{"messages":[{"role":"user","content":"ping"}]}`), model, 32, a.modelMaxTokens(model), false, "")
	if errNorm != nil {
		return map[string]any{"ok": false, "message": errNorm.Error()}
	}
	start := time.Now()
	_, respPayload, servedBy, errCall := a.callUpstream(ctx, cfg, model, payload, nil, account.ID)
	latency := time.Since(start).Milliseconds()
	if errCall != nil {
		failure := asFailure(errCall)
		return map[string]any{
			"ok": false, "message": failure.Message, "status": failure.Status,
			"latency_ms": latency, "account": account.Alias,
		}
	}
	content := ""
	if completion, errDecode := decodeCompletion(payload); errDecode == nil {
		content = completion
	}
	return map[string]any{
		"ok":         true,
		"account":    servedBy,
		"model":      model,
		"latency_ms": latency,
		"content":    content,
		"preview":    truncateRunes(string(respPayload), 200),
		"message":    "账号 " + servedBy + " 响应正常（" + fmt.Sprint(latency) + "ms）",
	}
}

// decodeCompletion pulls the assistant text out of a non-stream completion.
func decodeCompletion(raw []byte) (string, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", nil
	}
	return resp.Choices[0].Message.Content, nil
}

func (a *app) sanitizedConfig() map[string]any {
	cfg := a.currentConfig()
	return map[string]any{
		"data_dir":                 cfg.DataDir,
		"base_url":                 cfg.BaseURL,
		"auth_base_url":            cfg.AuthBaseURL,
		"desktop_device_file":      cfg.DesktopDeviceFile,
		"accounts_dir":             cfg.AccountsDir,
		"credential_alias":         cfg.CredentialAlias,
		"credential_priority":      cfg.CredentialPriority,
		"import_interval_seconds":  cfg.ImportInterval,
		"refresh_margin_seconds":   cfg.RefreshMargin,
		"quota_probe_seconds":      cfg.QuotaProbeSeconds,
		"client_name":              cfg.ClientName,
		"client_platform":          cfg.ClientPlatform,
		"client_version":           cfg.ClientVersion,
		"client_os_version":        cfg.ClientOSVersion,
		"client_channel":           cfg.ClientChannel,
		"client_device_id":         cfg.ClientDeviceID,
		"user_agent":               cfg.UserAgent,
		"sdk_version":              cfg.SDKVersion,
		"python_version":           cfg.PythonVersion,
		"refresh_seconds":          cfg.RefreshSeconds,
		"model_allowlist":          cfg.ModelAllowlist,
		"model_denylist":           cfg.ModelDenylist,
		"image_modalities":         cfg.ImageModalities,
		"vision_denylist":          cfg.VisionDenylist,
		"max_attempts":             cfg.MaxAttempts,
		"retry_jitter":             cfg.RetryJitter,
		"cooldown_seconds":         cfg.CooldownSeconds,
		"quota_cooldown_seconds":   cfg.QuotaCooldownSeconds,
		"request_timeout_seconds":  cfg.RequestTimeoutSeconds,
		"max_tokens_default":       cfg.MaxTokensDefault,
		"reasoning_effort":         cfg.ReasoningEffort,
		"app_name":                 cfg.AppName,
		"login_timeout_seconds":    cfg.LoginTimeoutSeconds,
		"debug_dump":               cfg.DebugDump,
		"access_token_configured":  cfg.AccessToken != "",
		"refresh_token_configured": cfg.RefreshToken != "",
	}
}

func tailOf(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "…"
	}
	return "…" + token[len(token)-4:]
}
