package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quotaApp(t *testing.T, handler http.HandlerFunc) (*app, *credential) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = server.URL
	cfg.QuotaCooldownSeconds = 600
	a.cfg = cfg

	account := &credential{
		ID:          "raccoon2api-a",
		Alias:       "a",
		Priority:    10,
		AccessToken: fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-a", "ses-a"),
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Source:      "upload",
	}
	a.upsertAccount(account)
	return a, account
}

func TestProbeQuotaMarksHealthy(t *testing.T) {
	a, account := quotaApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"1"},"finish_reason":"stop"}]}`)
	})

	state, note := a.probeQuota(context.Background(), account)
	if state != quotaOK || note != "" {
		t.Fatalf("state = %q note = %q", state, note)
	}
	stored := a.poolAccount(account.ID)
	if stored.QuotaState != quotaOK || stored.QuotaCheckedAt == 0 {
		t.Fatalf("quota not recorded: %+v", stored)
	}
	if stored.cooling(time.Now()) {
		t.Fatal("a healthy account must not be cooling")
	}
}

func TestProbeQuotaMarksExhaustedAndCools(t *testing.T) {
	a, account := quotaApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprint(w, `{"code":1000007,"message":"insufficient credits"}`)
	})

	state, _ := a.probeQuota(context.Background(), account)
	if state != quotaExhausted {
		t.Fatalf("state = %q", state)
	}
	stored := a.poolAccount(account.ID)
	if stored.QuotaState != quotaExhausted {
		t.Fatalf("quota state = %q", stored.QuotaState)
	}
	if !stored.cooling(time.Now()) {
		t.Fatal("an exhausted account must cool down")
	}
}

func TestProbeQuotaKeepsUnknownOnNetworkError(t *testing.T) {
	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = "http://127.0.0.1:1"
	a.cfg = cfg
	account := &credential{
		ID: "raccoon2api-a", Alias: "a", Priority: 10,
		AccessToken: fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-a", "ses-a"),
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
	}
	a.upsertAccount(account)

	state, note := a.probeQuota(context.Background(), account)
	if state != quotaUnknown || note == "" {
		t.Fatalf("state = %q note = %q", state, note)
	}
	if a.poolAccount(account.ID).cooling(time.Now()) {
		t.Fatal("a network failure must not cool the account")
	}
}

func TestQuotaSummaryCounts(t *testing.T) {
	a := newTestApp(t)
	a.upsertAccount(&credential{ID: "raccoon2api-a", Alias: "a", Priority: 10, AccessToken: "t1", QuotaState: quotaOK})
	a.upsertAccount(&credential{ID: "raccoon2api-b", Alias: "b", Priority: 9, AccessToken: "t2", QuotaState: quotaExhausted})
	a.upsertAccount(&credential{ID: "raccoon2api-c", Alias: "c", Priority: 8, AccessToken: "t3"})

	summary := a.quotaSummary()
	if summary["healthy"] != 1 || summary["exhausted"] != 1 || summary["unknown"] != 1 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestDeleteAccountRemovesCPAFile(t *testing.T) {
	var savedPath string
	previous := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method == methodHostAuthSave {
			request := payload.(hostAuthSaveRequest)
			savedPath = filepath.Join(t.TempDir(), request.Name)
			if err := os.WriteFile(savedPath, []byte(request.JSON), 0o600); err != nil {
				t.Fatalf("write record: %v", err)
			}
			raw, errMarshal := json.Marshal(map[string]string{"name": request.Name, "path": savedPath})
			if errMarshal != nil {
				t.Fatalf("marshal response: %v", errMarshal)
			}
			return raw, nil
		}
		return json.RawMessage(`{}`), nil
	}
	defer func() { hostCall = previous }()

	a := newTestApp(t)
	account := &credential{
		ID: "raccoon2api-a", Alias: "a", Priority: 10,
		AccessToken: fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-a", "ses-a"),
		Source:      "upload",
	}
	a.upsertAccount(account)
	a.publishAccount(account, true)
	if _, err := os.Stat(savedPath); err != nil {
		t.Fatalf("record not written: %v", err)
	}

	out := postJSON(t, a, "/accounts/delete", `{"id":"raccoon2api-a"}`)
	if out["ok"] != true {
		t.Fatalf("delete failed: %v", out)
	}
	if a.poolAccount("raccoon2api-a") != nil {
		t.Fatal("account still in the pool")
	}
	if _, err := os.Stat(savedPath); !os.IsNotExist(err) {
		t.Fatalf("CPA credential file still present: %v", err)
	}
}

func TestQuotaCheckPayloadSingleAccount(t *testing.T) {
	a, _ := quotaApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprint(w, `{"code":1000007,"message":"insufficient credits"}`)
	})
	out := postJSON(t, a, "/quota/check", `{"id":"raccoon2api-a"}`)
	if out["ok"] != true || out["exhausted"] != float64(1) {
		t.Fatalf("unexpected result: %v", out)
	}
	if message, _ := out["message"].(string); message == "" {
		t.Fatal("expected a summary message")
	}
}

func TestDeleteAccountRemovesSourceFile(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AccountsDir = dir
	a.cfg = cfg

	path := filepath.Join(dir, "work.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"`+fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-w", "ses-w")+`"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	a.scanAccountsDir(cfg)
	if a.poolAccount("raccoon2api-work") == nil {
		t.Fatalf("account not imported: %+v", a.poolSnapshot())
	}

	a.deleteAccount("raccoon2api-work")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("source file still present: %v", err)
	}
	a.scanAccountsDir(cfg)
	if a.poolAccount("raccoon2api-work") != nil {
		t.Fatal("account came back after the next scan")
	}
}

// pointsApp wires both upstream routes: the points API and the chat fallback.
func pointsApp(t *testing.T, balance string, balanceStatus int) (*app, *credential, *int) {
	t.Helper()
	chatCalls := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/web/points/v1/balance":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(balanceStatus)
			_, _ = w.Write([]byte(balance))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			*chatCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"choices":[{"message":{"content":"1"}}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = server.URL
	cfg.AuthBaseURL = server.URL + "/api/web/auth/v1"
	a.cfg = cfg
	account := &credential{
		ID:          "raccoon2api-points",
		Alias:       "points",
		Priority:    10,
		AccessToken: fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-p", "ses-p"),
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Source:      "upload",
	}
	a.upsertAccount(account)
	return a, account, chatCalls
}

func TestCheckQuotaReadsPointsBalance(t *testing.T) {
	a, account, chatCalls := pointsApp(t, `{"code":0,"message":"success","data":{"available_points":6210,"daily_points":210,"reward_points":6000,"topup_points":0,"total_chargeable_points":6210}}`, http.StatusOK)
	state, note := a.checkQuota(context.Background(), account)
	if state != quotaOK || note != "" {
		t.Fatalf("state=%q note=%q", state, note)
	}
	stored := a.poolAccount(account.ID)
	if stored.PointsAvailable != 6210 || stored.PointsChargeable != 6210 {
		t.Fatalf("points not stored: %+v", stored)
	}
	if stored.PointsDaily != 210 || stored.PointsReward != 6000 {
		t.Fatalf("breakdown not stored: %+v", stored)
	}
	if stored.CooldownUntil != 0 {
		t.Fatal("healthy account must not cool down")
	}
	if *chatCalls != 0 {
		t.Fatalf("points API answered but the spending probe still ran %d time(s)", *chatCalls)
	}
}

func TestCheckQuotaMarksExhaustedAtZeroPoints(t *testing.T) {
	a, account, chatCalls := pointsApp(t, `{"code":0,"data":{"available_points":0,"total_chargeable_points":0,"topup_frozen":true}}`, http.StatusOK)
	state, note := a.checkQuota(context.Background(), account)
	if state != quotaExhausted {
		t.Fatalf("state = %q, want exhausted", state)
	}
	if !strings.Contains(note, "冻结") {
		t.Fatalf("note = %q, want the frozen hint", note)
	}
	stored := a.poolAccount(account.ID)
	if stored.CooldownUntil == 0 {
		t.Fatal("exhausted account must cool down")
	}
	if *chatCalls != 0 {
		t.Fatalf("points API answered but the spending probe still ran %d time(s)", *chatCalls)
	}
}

func TestCheckQuotaFallsBackToProbe(t *testing.T) {
	a, account, chatCalls := pointsApp(t, `{"code":404}`, http.StatusNotFound)
	state, note := a.checkQuota(context.Background(), account)
	if state != quotaOK {
		t.Fatalf("state = %q note = %q, want the probe fallback to succeed", state, note)
	}
	if *chatCalls == 0 {
		t.Fatal("points API failed but the probe never ran")
	}
}

func TestQuotaSummarySumsPoints(t *testing.T) {
	a := newTestApp(t)
	first := testAccount("raccoon2api-x", 10, time.Now().Add(time.Hour))
	first.QuotaState = quotaOK
	first.PointsAvailable = 1200
	first.PointsChargeable = 1500
	second := testAccount("raccoon2api-y", 10, time.Now().Add(time.Hour))
	second.QuotaState = quotaOK
	second.PointsAvailable = 800
	second.PointsChargeable = 800
	a.upsertAccount(first)
	a.upsertAccount(second)

	summary := a.quotaSummary()
	if summary["points_available"] != int64(2000) || summary["points_chargeable"] != int64(2300) {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestPointsForTurnSumsMatchingBills(t *testing.T) {
	bills := []pointsBill{
		{BizID: "ses-abc:ses-abc-turn-1", BizType: "chat", Points: -12},
		{BizID: "ses-abc:ses-abc-turn-1", BizType: "chat", Points: -3},
		{BizID: "ses-abc:ses-abc-turn-2", BizType: "chat", Points: -7},
		{BizID: "ses-abc:ses-abc-turn-1", BizType: "daily_grant", Points: 300},
	}
	points, ok := pointsForTurn(bills, "ses-abc", "ses-abc-turn-1")
	if !ok || points != -15 {
		t.Fatalf("turn 1 points = %d ok=%v, want -15", points, ok)
	}
	points, ok = pointsForTurn(bills, "ses-abc", "ses-abc-turn-2")
	if !ok || points != -7 {
		t.Fatalf("turn 2 points = %d ok=%v, want -7", points, ok)
	}
	if _, ok := pointsForTurn(bills, "ses-abc", "ses-abc-turn-11"); ok {
		t.Fatal("turn-1 must not match turn-11")
	}
}

func TestBillMatchesTurnToleratesServerSessionPrefix(t *testing.T) {
	if !billMatchesTurn("server-session:ses-abc-turn-1", "ses-abc", "ses-abc-turn-1") {
		t.Fatal("turn suffix should match even when the server uses another session prefix")
	}
	if billMatchesTurn("server-session:ses-abc-turn-11", "ses-abc", "ses-abc-turn-1") {
		t.Fatal("turn-1 must not match turn-11")
	}
}

func TestFetchPointsBillsReadsTurn(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/web/points/v2/bills" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"items":[` +
			`{"biz_id":"ses-abc:ses-abc-turn-1","biz_type":"chat","points":-12,"created_at":"2026-10-27 18:00:00"},` +
			`{"biz_id":"ses-abc:ses-abc-turn-1","biz_type":"chat","points":-3,"created_at":"2026-10-27 18:00:00"}]}}`))
	}))
	t.Cleanup(server.Close)

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = server.URL
	cfg.AuthBaseURL = server.URL + "/api/web/auth/v1"
	a.cfg = cfg
	account := &credential{
		ID: "raccoon2api-bills", Alias: "bills", Priority: 10,
		AccessToken: fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-b", "ses-b"),
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
	}
	a.upsertAccount(account)

	points, ok := a.fetchPointsForTurn(context.Background(), account, "ses-abc", "ses-abc-turn-1")
	if !ok || points != -15 {
		t.Fatalf("points = %d ok=%v, want -15", points, ok)
	}
	if !strings.Contains(gotQuery, "paging.limit=50") || !strings.Contains(gotQuery, "type=all") {
		t.Fatalf("query = %q", gotQuery)
	}
}

func TestAttachRequestPointsFillsRequestLog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"biz_id":"ses-abc:ses-abc-turn-1","biz_type":"chat","points":-9}]}}`))
	}))
	t.Cleanup(server.Close)

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = server.URL
	cfg.AuthBaseURL = server.URL + "/api/web/auth/v1"
	a.cfg = cfg
	account := &credential{
		ID: "raccoon2api-attach", Alias: "attach", Priority: 10,
		AccessToken: fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-a", "ses-a"),
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
	}
	a.upsertAccount(account)

	seq := a.recordRequest(requestEntry{Model: "sn-glm-5-3-flash", Status: http.StatusOK})
	a.attachRequestPoints(seq, account.ID, conversation{SessionID: "ses-abc", TurnID: "ses-abc-turn-1"})

	entries := a.requests.tail(1)
	if len(entries) != 1 || entries[0].Points == nil || *entries[0].Points != -9 {
		t.Fatalf("entry = %+v", entries)
	}
	if summary := a.requests.summary(); summary["points"] != int64(-9) {
		t.Fatalf("summary = %+v", summary)
	}
}
