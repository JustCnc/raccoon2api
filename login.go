package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The gateway's desktop login is a one-time authorization code: the user opens
// /code/authorize, logs in, and the browser is redirected to
// office-raccoon://auth/callback?code=…. The code is single-use, so the plugin
// exchanges it immediately and stores the resulting tokens as an account.
//
// The WeChat QR route (/login/mp + login_with_qrcode_code) still answers polls
// but its page 404s in client 1.1.3, so it is not offered.

type loginSession struct {
	State   string
	Created time.Time
	Expires time.Time
	Auth    *credential
	Err     string
}

// authorizeURL builds the URL the user opens to sign in. When the loopback
// receiver is up the page is told to come back to it, otherwise the browser is
// left with the office-raccoon:// deep link and the user has to copy the code.
func (a *app) authorizeURL(state string) string {
	cfg := a.currentConfig()
	base := "https://xiaohuanxiong.com"
	if parsed, err := url.Parse(cfg.AuthBaseURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		base = parsed.Scheme + "://" + parsed.Host
	}
	values := url.Values{}
	values.Set("login_source", "desktop")
	values.Set("appname", cfg.AppName)
	values.Set("state", state)
	if a.loginCallbackEnabled() {
		values.Set("redirect", fmt.Sprintf("http://127.0.0.1:%d%s", a.loginCallbackPort(), loginCallbackPath))
		values.Set("desktop_callback", "loopback")
	}
	return base + "/code/authorize?" + values.Encode()
}

func (a *app) newLoginSession() *loginSession {
	cfg := a.currentConfig()
	now := time.Now()
	session := &loginSession{
		// The authorize page only accepts a 64-hex state for the loopback
		// handoff, and it round-trips the same value back.
		State:   randomHex(32),
		Created: now,
		Expires: now.Add(time.Duration(cfg.LoginTimeoutSeconds) * time.Second),
	}
	a.loginMu.Lock()
	if a.logins == nil {
		a.logins = map[string]*loginSession{}
	}
	for state, existing := range a.logins {
		if now.After(existing.Expires) {
			delete(a.logins, state)
		}
	}
	a.logins[session.State] = session
	a.loginMu.Unlock()
	return session
}

func (a *app) loginSession(state string) *loginSession {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	session, ok := a.logins[strings.TrimSpace(state)]
	if !ok {
		return nil
	}
	cloned := *session
	return &cloned
}

func (a *app) finishLogin(state string, account *credential, err error) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	session, ok := a.logins[strings.TrimSpace(state)]
	if !ok {
		return
	}
	if err != nil {
		session.Err = err.Error()
		return
	}
	session.Auth = account
}

// startLogin answers CPA's auth.login.start: it hands back the URL to open and
// the state the host polls with.
func (a *app) startLogin(raw []byte) ([]byte, error) {
	if _, err := decodeRequest[authLoginStartRequest](raw); err != nil {
		return nil, err
	}
	session := a.newLoginSession()
	a.log.info("login", "已生成授权链接："+a.authorizeURL(session.State))
	return okEnvelope(authLoginStartResponse{
		Provider:  providerKey,
		URL:       a.authorizeURL(session.State),
		State:     session.State,
		ExpiresAt: session.Expires,
	})
}

// pollLogin answers CPA's auth.login.poll. The code itself arrives out of band
// (the console or an auth record), so polling only reports progress.
func (a *app) pollLogin(raw []byte) ([]byte, error) {
	req, err := decodeRequest[authLoginPollRequest](raw)
	if err != nil {
		return nil, err
	}
	session := a.loginSession(req.State)
	if session == nil {
		return okEnvelope(authLoginPollResponse{
			Status:  "error",
			Message: "登录会话不存在或已过期，请重新发起授权",
		})
	}
	if session.Auth != nil {
		return okEnvelope(authLoginPollResponse{
			Status:  "success",
			Message: "登录成功",
			Auth:    a.authRecord(session.Auth, session.Auth.ID+".json"),
		})
	}
	if session.Err != "" {
		return okEnvelope(authLoginPollResponse{Status: "error", Message: session.Err})
	}
	if time.Now().After(session.Expires) {
		return okEnvelope(authLoginPollResponse{Status: "error", Message: "授权链接已过期，请重新发起"})
	}
	return okEnvelope(authLoginPollResponse{
		Status:  "pending",
		Message: "等待授权：登录后把回调地址里的 code 提交给插件",
	})
}

// completeLogin exchanges an authorization code and registers the account.
func (a *app) completeLogin(ctx context.Context, state, code, alias string) (*credential, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, fmt.Errorf("授权码为空")
	}
	if idx := strings.Index(code, "code="); idx >= 0 {
		// Accept a pasted callback URL as-is.
		code = code[idx+len("code="):]
		if cut := strings.IndexAny(code, "&#"); cut >= 0 {
			code = code[:cut]
		}
	}
	account, err := a.exchangeAuthorizationCode(ctx, code)
	if err != nil {
		a.finishLogin(state, nil, err)
		return nil, err
	}
	if alias = sanitizeAlias(alias); alias == "" {
		// Derive the alias from the account id so a second login for the same
		// account updates it instead of creating a duplicate pool entry.
		alias = subjectAlias(account.Subject)
	}
	account.ID = accountIDForAlias(alias)
	account.Alias = alias
	account.Source = "login"
	account.Priority = fallbackPriority(a.currentConfig().CredentialPriority, 10)
	account.UpdatedAt = time.Now().UnixMilli()
	if existing := a.poolAccount(account.ID); existing != nil {
		account.Priority = existing.Priority
	} else if same := a.accountBySubject(account.Subject); same != nil {
		// Same upstream account, new alias: keep the existing entry.
		account.ID = same.ID
		account.Alias = same.Alias
		account.Priority = same.Priority
	}
	a.upsertAccount(account)
	a.publishAccount(account, true)
	a.finishLogin(state, account, nil)
	a.log.info("login", "已通过授权码添加账号："+alias)
	return account, nil
}

// exchangeAuthorizationCode trades a one-time code for a token pair.
func (a *app) exchangeAuthorizationCode(ctx context.Context, code string) (*credential, error) {
	cfg := a.currentConfig()
	body, _ := json.Marshal(map[string]string{"authorization_code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.AuthBaseURL+"/login_with_authorization_code", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("授权请求失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 1<<20)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
		Details string      `json:"details"`
		Data    struct {
			AccessToken    string `json:"access_token"`
			RefreshToken   string `json:"refresh_token"`
			OfficeIdentity string `json:"office_identity"`
			OfficeOrgName  string `json:"office_org_name"`
			OfficeOrgRole  string `json:"office_org_role"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("授权响应不是 JSON：%w", err)
	}
	switch envelope.Code.String() {
	case "", "0", "200":
	case "200035":
		return nil, fmt.Errorf("授权码不存在、已过期或已被使用（桌面端可能已经先一步消费掉）")
	default:
		return nil, fmt.Errorf("授权失败：code=%s %s %s", envelope.Code.String(), envelope.Message, envelope.Details)
	}
	if strings.TrimSpace(envelope.Data.AccessToken) == "" {
		return nil, fmt.Errorf("授权响应没有 access_token")
	}
	account := &credential{
		AccessToken:    strings.TrimSpace(envelope.Data.AccessToken),
		RefreshToken:   strings.TrimSpace(envelope.Data.RefreshToken),
		OfficeIdentity: strings.TrimSpace(envelope.Data.OfficeIdentity),
		UpdatedAt:      time.Now().UnixMilli(),
	}
	account.normalize()
	return account, nil
}

// subjectAlias derives a stable alias from the JWT subject.
func subjectAlias(subject string) string {
	trimmed := strings.TrimSpace(subject)
	if trimmed == "" {
		return "login-" + randomHex(3)
	}
	if len(trimmed) > 8 {
		trimmed = trimmed[len(trimmed)-8:]
	}
	return sanitizeAlias("u-" + trimmed)
}

// accountBySubject finds an existing pool entry for the same upstream account.
func (a *app) accountBySubject(subject string) *credential {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return nil
	}
	for _, account := range a.poolSnapshot() {
		if account.Subject == subject {
			return account
		}
	}
	return nil
}

// completedCodeAccount returns the account a one-time code already produced, so
// a host reload that re-parses the same file does not fail with 200035.
func (a *app) completedCodeAccount(code string) *credential {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	account, ok := a.completedCode[strings.TrimSpace(code)]
	if !ok {
		return nil
	}
	return a.poolAccount(account.ID)
}

func (a *app) rememberCompletedCode(code string, account *credential) {
	code = strings.TrimSpace(code)
	if code == "" || account == nil {
		return
	}
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if a.completedCode == nil {
		a.completedCode = map[string]*credential{}
	}
	if len(a.completedCode) >= 64 {
		for key := range a.completedCode {
			delete(a.completedCode, key)
			break
		}
	}
	a.completedCode[code] = account.clone()
}
