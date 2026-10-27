package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The upstream authorize page only honours the loopback handoff on an exact
// contract, so the generated URL is checked parameter by parameter.
func TestAuthorizeURLUsesLoopbackContract(t *testing.T) {
	a := newTestApp(t)
	a.loginCb = &http.Server{} // pretend the receiver is up
	state := randomHex(32)
	if len(state) != 64 {
		t.Fatalf("state length = %d, want 64 hex chars", len(state))
	}
	parsed, err := url.Parse(a.authorizeURL(state))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	query := parsed.Query()
	if got := query.Get("login_source"); got != "desktop" {
		t.Fatalf("login_source = %q", got)
	}
	if got := query.Get("desktop_callback"); got != "loopback" {
		t.Fatalf("desktop_callback = %q", got)
	}
	if got := query.Get("state"); got != state {
		t.Fatalf("state = %q", got)
	}
	if got := query.Get("redirect"); got != "http://127.0.0.1:18763/auth/callback" {
		t.Fatalf("redirect = %q", got)
	}
	if got := query.Get("appname"); got == "" {
		t.Fatal("appname missing")
	}
}

// Without the receiver the URL must stay on the deep-link path, otherwise the
// user is sent to a port nothing is listening on.
func TestAuthorizeURLFallsBackToDeepLink(t *testing.T) {
	a := newTestApp(t)
	a.loginCb = nil
	parsed, err := url.Parse(a.authorizeURL(randomHex(32)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	query := parsed.Query()
	if query.Has("desktop_callback") || query.Has("redirect") {
		t.Fatalf("deep link URL carries loopback params: %v", query)
	}
}

func TestLoginCallbackRejectsUnknownState(t *testing.T) {
	a := newTestApp(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, loginCallbackPath+"?code=abc&state=deadbeef", nil)
	a.serveLoginCallback(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "登录会话已失效") {
		t.Fatalf("unexpected body: %s", recorder.Body.String())
	}
}

func TestLoginCallbackRejectsMissingCode(t *testing.T) {
	a := newTestApp(t)
	session := a.newLoginSession()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, loginCallbackPath+"?state="+session.State, nil)
	a.serveLoginCallback(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "没有拿到授权码") {
		t.Fatalf("unexpected body: %s", recorder.Body.String())
	}
}

func TestLoginCallbackExchangesCodeAndRegistersAccount(t *testing.T) {
	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/login_with_authorization_code") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"` + fakeJWT(time.Now().Add(2*time.Hour).Unix(), "pu-cb", "ses-cb") + `","refresh_token":"rt-cb","office_identity":"org-1"}}`))
	}))
	defer upstream.Close()
	cfg.AuthBaseURL = upstream.URL
	a.cfg = cfg

	session := a.newLoginSession()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, loginCallbackPath+"?state="+session.State+"&code=one-time", nil)
	a.serveLoginCallback(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "登录成功") {
		t.Fatalf("unexpected body: %s", recorder.Body.String())
	}
	completed := a.loginSession(session.State)
	if completed == nil || completed.Auth == nil {
		t.Fatal("session was not completed")
	}
	if account := a.poolAccount(completed.Auth.ID); account == nil {
		t.Fatalf("account %q missing from pool", completed.Auth.ID)
	}
}

func TestLoginCallbackUnknownPathIsNotFound(t *testing.T) {
	a := newTestApp(t)
	recorder := httptest.NewRecorder()
	a.serveLoginCallback(recorder, httptest.NewRequest(http.MethodGet, "/other", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}
