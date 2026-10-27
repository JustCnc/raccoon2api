package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// grantTestServer answers the daily-gift route and records what arrived.
type grantTestServer struct {
	server  *httptest.Server
	granted bool
	popup   *grantPopup
	calls   int
	lastReq *http.Request
}

func newGrantTestServer(t *testing.T, granted bool, popup *grantPopup) *grantTestServer {
	t.Helper()
	fake := &grantTestServer{granted: granted, popup: popup}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pointsGrantPath {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		fake.calls++
		fake.lastReq = r
		body := map[string]any{"code": 0, "message": "success", "data": map[string]any{"granted": granted, "popup": popup}}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func grantTestApp(t *testing.T, fake *grantTestServer) *app {
	t.Helper()
	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = fake.server.URL + "/api/web/auth/v1"
	cfg.DailyGrantEnabled = true
	a.cfg = cfg
	return a
}

// addTestAccount registers a claimable account on the test app.
func (a *app) addTestAccount(t *testing.T, alias, token string) *credential {
	t.Helper()
	account := &credential{
		ID:          "raccoon2api-" + alias,
		Alias:       alias,
		Priority:    10,
		AccessToken: fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-"+alias, "ses-"+alias),
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Source:      "upload",
	}
	a.upsertAccount(account)
	return a.poolAccount(account.ID)
}

func TestClaimDailyPointsRecordsTheDay(t *testing.T) {
	fake := newGrantTestServer(t, true, &grantPopup{Source: "desktopLogin", Points: 300})
	a := grantTestApp(t, fake)
	account := a.addTestAccount(t, "acc", "token-1")

	result := a.claimDailyPoints(context.Background(), account)
	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if !result.Granted || result.Points != 300 {
		t.Fatalf("result = %+v, want granted 300", result)
	}
	if result.Day != grantDay(time.Now()) {
		t.Fatalf("day = %q, want today", result.Day)
	}

	stored := a.poolAccount(account.ID)
	if stored.DailyGrantDay != result.Day || stored.DailyGrantPoints != 300 || stored.DailyGrantAt == 0 {
		t.Fatalf("stored grant = %+v", stored)
	}
	if fake.lastReq.Header.Get("x-client-platform") != "desktop-windows" {
		t.Fatalf("platform header = %q, want desktop-windows", fake.lastReq.Header.Get("x-client-platform"))
	}
}

func TestClaimDailyPointsSkipsTheSameDay(t *testing.T) {
	fake := newGrantTestServer(t, true, &grantPopup{Points: 300})
	a := grantTestApp(t, fake)
	a.addTestAccount(t, "acc", "token-1")

	first := a.claimDailyPointsAll(context.Background(), nil)
	second := a.claimDailyPointsAll(context.Background(), nil)
	if len(first) != 1 || len(second) != 0 {
		t.Fatalf("first = %d results, second = %d, want 1 then 0", len(first), len(second))
	}
	if fake.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", fake.calls)
	}
}

func TestClaimDailyPointsAlreadyGrantedKeepsDay(t *testing.T) {
	fake := newGrantTestServer(t, false, nil)
	a := grantTestApp(t, fake)
	account := a.addTestAccount(t, "acc", "token-1")

	result := a.claimDailyPoints(context.Background(), account)
	if result.Error != "" || result.Granted || result.Points != 0 {
		t.Fatalf("result = %+v, want a silent already-granted answer", result)
	}
	stored := a.poolAccount(account.ID)
	if stored.DailyGrantDay != grantDay(time.Now()) || stored.DailyGrantPoints != 0 {
		t.Fatalf("stored grant = %+v, want today with zero points", stored)
	}
}

func TestClaimDailyPointsDoesNotWriteOffFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"unauthorized"}`))
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = server.URL + "/api/web/auth/v1"
	a.cfg = cfg
	account := a.addTestAccount(t, "acc", "token-1")

	result := a.claimDailyPoints(context.Background(), account)
	if result.Error == "" {
		t.Fatal("expected an error for HTTP 401")
	}
	if stored := a.poolAccount(account.ID); stored.DailyGrantDay != "" {
		t.Fatalf("failed claim marked the day: %+v", stored)
	}
}

func TestDailyGrantPayloadReportsSummary(t *testing.T) {
	fake := newGrantTestServer(t, true, &grantPopup{Points: 300})
	a := grantTestApp(t, fake)
	a.addTestAccount(t, "acc", "token-1")

	summary := a.dailyGrantPayload(managementRequest{Body: []byte(`{}`)})
	if summary["ok"] != true {
		t.Fatalf("ok = %v", summary["ok"])
	}
	if summary["granted"] != 1 || summary["points"] != int64(300) {
		t.Fatalf("summary = %+v", summary)
	}
	if message, _ := summary["message"].(string); message == "" {
		t.Fatal("expected a message")
	}
}

func TestDailyGrantPayloadRejectsUnknownAccount(t *testing.T) {
	fake := newGrantTestServer(t, true, nil)
	a := grantTestApp(t, fake)
	summary := a.dailyGrantPayload(managementRequest{Body: []byte(`{"id":"nope"}`)})
	if summary["ok"] != false {
		t.Fatalf("ok = %v, want false", summary["ok"])
	}
	if fake.calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", fake.calls)
	}
}

func TestGrantDayUsesUpstreamZone(t *testing.T) {
	// 2026-10-25T16:30:00Z is already 2026-10-26 in +08:00.
	utc := time.Date(2026, 10, 25, 16, 30, 0, 0, time.UTC)
	if got := grantDay(utc); got != "2026-10-26" {
		t.Fatalf("grantDay = %q, want 2026-10-26", got)
	}
}
