package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// twoAccountApp builds an app with two pooled accounts (priority 10 and 20) and
// points the upstream at the given server.
func twoAccountApp(t *testing.T, chatURL, authURL string) (*app, Config, *credential, *credential) {
	t.Helper()
	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = chatURL
	if authURL != "" {
		cfg.AuthBaseURL = authURL
	}
	cfg.MaxAttempts = 3
	a.cfg = cfg

	first := &credential{
		ID: "raccoon2api-a", Alias: "a", Priority: 10, Source: "file",
		AccessToken: "token-a", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	second := &credential{
		ID: "raccoon2api-b", Alias: "b", Priority: 20, Source: "file",
		AccessToken: "token-b", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	a.upsertAccount(first)
	a.upsertAccount(second)
	return a, cfg, first, second
}

func TestForbiddenIsLoggedAndCoolsDown(t *testing.T) {
	var callsA, callsB int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			atomic.AddInt32(&callsA, 1)
			w.Header().Set("Server", "waf-test")
			w.WriteHeader(http.StatusForbidden)
		case "Bearer token-b":
			atomic.AddInt32(&callsB, 1)
			fmt.Fprint(w, `{"id":"c1","choices":[{"index":0,"message":{"content":"ok"}}]}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	a, cfg, first, second := twoAccountApp(t, server.URL, "")
	_, _, _, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, first.ID)
	if err == nil {
		t.Fatal("expected a 403 failure")
	}
	if failure := asFailure(err); failure.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", failure.Status)
	}
	// Diagnostics first: a 403 still fails the request instead of rotating.
	if atomic.LoadInt32(&callsB) != 0 {
		t.Fatalf("403 must not rotate yet: account b called %d times", callsB)
	}
	cooled := a.poolAccount(first.ID)
	if cooled == nil || !cooled.cooling(time.Now()) {
		t.Fatalf("403 account should cool down: %+v", cooled)
	}
	if remaining := cooled.cooldownRemaining(time.Now()); remaining < 25*time.Second || remaining > forbiddenCooldown {
		t.Fatalf("403 cooldown = %v, want ~%v", remaining, forbiddenCooldown)
	}
	if cooled.Dead {
		t.Fatalf("a 403 must not kill the account: %+v", cooled)
	}
	if healthy := a.poolAccount(second.ID); healthy == nil || healthy.cooling(time.Now()) {
		t.Fatalf("account b must stay healthy: %+v", healthy)
	}
	var found bool
	for _, entry := range a.log.tail(0) {
		if !strings.Contains(entry.Message, "403 mode=buffered") {
			continue
		}
		found = true
		for _, want := range []string{"account=a", "attempt=1", "proto=HTTP", "session=ses-", "turn=", "waf-test"} {
			if !strings.Contains(entry.Message, want) {
				t.Fatalf("403 log missing %q: %s", want, entry.Message)
			}
		}
	}
	if !found {
		t.Fatalf("403 response was not logged: %+v", a.log.tail(0))
	}
}

func TestForbiddenStreamIsLoggedAndCoolsDown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "sid=secret-value")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	a, cfg, first, second := twoAccountApp(t, server.URL, "")
	_, _, _, err := a.openStream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[],"stream":true}`), nil, first.ID)
	if err == nil {
		t.Fatal("expected a 403 failure")
	}
	if failure := asFailure(err); failure.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", failure.Status)
	}
	if cooled := a.poolAccount(first.ID); cooled == nil || !cooled.cooling(time.Now()) {
		t.Fatalf("403 account should cool down: %+v", cooled)
	}
	if healthy := a.poolAccount(second.ID); healthy == nil || healthy.cooling(time.Now()) {
		t.Fatalf("account b must stay healthy: %+v", healthy)
	}
	var found bool
	for _, entry := range a.log.tail(0) {
		if !strings.Contains(entry.Message, "403 mode=stream") {
			continue
		}
		found = true
		if strings.Contains(entry.Message, "secret-value") {
			t.Fatalf("cookie value leaked into the log: %s", entry.Message)
		}
	}
	if !found {
		t.Fatalf("stream 403 was not logged: %+v", a.log.tail(0))
	}
}

func TestForbiddenDoesNotShortenLongerCooldown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	a, cfg, first, _ := twoAccountApp(t, server.URL, "")
	a.setCooldown(first.ID, time.Now().Add(time.Hour), "quota")
	_, _, _, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, first.ID)
	if err == nil {
		t.Fatal("expected a 403 failure")
	}
	cooled := a.poolAccount(first.ID)
	if remaining := cooled.cooldownRemaining(time.Now()); remaining < 55*time.Minute {
		t.Fatalf("403 shortened an hour-long cooldown to %v", remaining)
	}
}

func TestLimiterRetryKeepsAccountInPool(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"code":200103,"message":"limiter_request_error","details":"request exceed max time, retry after:0s"}`)
			return
		}
		fmt.Fprint(w, `{"id":"c1","choices":[{"index":0,"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()

	a, cfg, first, _ := twoAccountApp(t, server.URL, "")
	headers, payload, _, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, first.ID)
	if err != nil {
		t.Fatalf("limiter refusal must be retried, got %v", err)
	}
	if headers == nil || !strings.Contains(string(payload), `"ok"`) {
		t.Fatalf("payload = %s", payload)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
	account := a.poolAccount(first.ID)
	if account == nil || account.cooling(time.Now()) {
		t.Fatalf("a limiter refusal must not cool the account down: %+v", account)
	}
}

func TestLimiterExhaustionRotatesToNextAccount(t *testing.T) {
	var callsA, callsB int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			atomic.AddInt32(&callsA, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"code":200103,"message":"limiter_request_error"}`)
		case "Bearer token-b":
			atomic.AddInt32(&callsB, 1)
			fmt.Fprint(w, `{"id":"c1","choices":[{"index":0,"message":{"content":"ok"}}]}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	a, cfg, first, second := twoAccountApp(t, server.URL, "")
	_, payload, usedID, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, first.ID)
	if err != nil {
		t.Fatalf("the busy account must hand over to the idle one: %v", err)
	}
	if usedID != second.ID || !strings.Contains(string(payload), `"ok"`) {
		t.Fatalf("used %s payload %s, want the second account", usedID, payload)
	}
	if got := atomic.LoadInt32(&callsA); got != 1+limiterRetries {
		t.Fatalf("busy account calls = %d, want %d", got, 1+limiterRetries)
	}
	if got := atomic.LoadInt32(&callsB); got != 1 {
		t.Fatalf("idle account calls = %d, want 1", got)
	}
	for _, account := range []*credential{a.poolAccount(first.ID), a.poolAccount(second.ID)} {
		if account == nil || account.cooling(time.Now()) {
			t.Fatalf("limiter refusals must not park an account: %+v", account)
		}
	}
}

func TestLimiterRefusalRidesOutShortBurst(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= limiterRetries {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"code":200103,"message":"limiter_request_error"}`)
			return
		}
		fmt.Fprint(w, `{"id":"c1","choices":[{"index":0,"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()

	a, cfg, first, _ := twoAccountApp(t, server.URL, "")
	if _, _, _, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, first.ID); err != nil {
		t.Fatalf("limiter retries must ride it out: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != limiterRetries+1 {
		t.Fatalf("calls = %d, want %d", got, limiterRetries+1)
	}
}

func TestLimiterIsTransientButQuotaIsNot(t *testing.T) {
	limiter := classifyFailure(http.StatusTooManyRequests,
		[]byte(`{"code":200103,"message":"limiter_request_error","details":"request exceed max time, retry after:0s"}`))
	if !limiter.Transient || limiter.Quota || limiter.Status != http.StatusTooManyRequests {
		t.Fatalf("limiter failure = %+v, want transient 429", limiter)
	}
	// The phrase alone on a 5xx must stay on the retry/rotate path.
	server := classifyFailure(http.StatusBadGateway, []byte(`{"message":"request exceed max time"}`))
	if server.Transient {
		t.Fatalf("a 502 must not be treated as the limiter: %+v", server)
	}
	quota := classifyFailure(http.StatusTooManyRequests, []byte(`{"code":1000007,"message":"limiter_request_error insufficient credits"}`))
	if !quota.Quota || quota.Transient {
		t.Fatalf("quota must win over the limiter: %+v", quota)
	}
	auth := classifyFailure(http.StatusTooManyRequests, []byte(`{"code":200001,"message":"limiter_request_error"}`))
	if auth.Status != http.StatusUnauthorized || auth.Transient {
		t.Fatalf("auth must win over the limiter: %+v", auth)
	}
}

func TestAccountConcurrencyDisabled(t *testing.T) {
	a, cfg, first, _ := twoAccountApp(t, "http://127.0.0.1:1", "")
	cfg.AccountConcurrency = 0
	a.cfg = cfg
	release, err := a.acquireAccountSlot(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	release() // must stay a no-op
}

func TestAccountConcurrencyCapsInflight(t *testing.T) {
	var inflight, peak int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt32(&inflight, 1)
		for {
			seen := atomic.LoadInt32(&peak)
			if current <= seen || atomic.CompareAndSwapInt32(&peak, seen, current) {
				break
			}
		}
		time.Sleep(120 * time.Millisecond)
		atomic.AddInt32(&inflight, -1)
		fmt.Fprint(w, `{"id":"c1","choices":[{"index":0,"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()

	a, cfg, first, _ := twoAccountApp(t, server.URL, "")
	cfg.AccountConcurrency = 2
	a.cfg = cfg

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
				[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, first.ID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("queued request failed: %v", err)
		}
	}
	if peak > 2 {
		t.Fatalf("peak in-flight = %d, want <= 2", peak)
	}
	if peak < 2 {
		t.Fatalf("peak in-flight = %d, the queue never ran in parallel", peak)
	}
}

func TestRetryRotatesOnRateLimit(t *testing.T) {
	var callsA, callsB int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			atomic.AddInt32(&callsA, 1)
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"rate limited"}`)
		case "Bearer token-b":
			atomic.AddInt32(&callsB, 1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	a, cfg, first, second := twoAccountApp(t, server.URL, "")
	_, payload, _, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, first.ID)
	if err != nil {
		t.Fatalf("callUpstream: %v", err)
	}
	if !strings.Contains(string(payload), `"ok"`) {
		t.Fatalf("payload = %s", payload)
	}
	if atomic.LoadInt32(&callsA) != 1 || atomic.LoadInt32(&callsB) != 1 {
		t.Fatalf("calls a=%d b=%d, want 1/1", callsA, callsB)
	}
	// The throttled account cools down for the delay the upstream asked for.
	cooled := a.poolAccount(first.ID)
	if cooled == nil || !cooled.cooling(time.Now()) {
		t.Fatalf("account a not cooling: %+v", cooled)
	}
	if remaining := cooled.cooldownRemaining(time.Now()); remaining < 25*time.Second {
		t.Fatalf("Retry-After not honoured: %v", remaining)
	}
	if healthy := a.poolAccount(second.ID); healthy == nil || healthy.cooling(time.Now()) {
		t.Fatalf("account b must stay healthy: %+v", healthy)
	}
}

func TestQuotaFailsFastWithoutRotating(t *testing.T) {
	var callsA, callsB int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			atomic.AddInt32(&callsA, 1)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"code":1000007,"message":"insufficient credits"}`)
		default:
			atomic.AddInt32(&callsB, 1)
			fmt.Fprint(w, `{"id":"c1","choices":[{"index":0,"message":{"content":"ok"}}]}`)
		}
	}))
	defer server.Close()

	a, cfg, first, _ := twoAccountApp(t, server.URL, "")
	_, _, _, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, first.ID)
	if err == nil {
		t.Fatal("expected a quota failure")
	}
	failure := asFailure(err)
	if failure.Status != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", failure.Status)
	}
	if atomic.LoadInt32(&callsB) != 0 {
		t.Fatalf("quota error must not rotate: account b called %d times", callsB)
	}
	cooled := a.poolAccount(first.ID)
	if cooled == nil || !cooled.cooling(time.Now()) {
		t.Fatalf("quota account should cool down: %+v", cooled)
	}
}

func TestUnauthorizedRefreshThenRotate(t *testing.T) {
	var callsA, callsB, refreshes int32
	newToken := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-a", "ses-a")

	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			atomic.AddInt32(&callsA, 1)
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"code":200003,"message":"authorization_verify_error"}`)
		case "Bearer " + newToken:
			atomic.AddInt32(&callsA, 1)
			fmt.Fprint(w, `{"id":"c1","choices":[{"index":0,"message":{"content":"refreshed"}}]}`)
		default:
			atomic.AddInt32(&callsB, 1)
			fmt.Fprint(w, `{"id":"c1","choices":[{"index":0,"message":{"content":"rotated"}}]}`)
		}
	}))
	defer chat.Close()

	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshes, 1)
		fmt.Fprintf(w, `{"code":0,"data":{"access_token":%q,"refresh_token":"r2"}}`, newToken)
	}))
	defer auth.Close()

	a, cfg, first, _ := twoAccountApp(t, chat.URL, auth.URL)
	first.RefreshToken = "r1"
	a.upsertAccount(first)

	_, payload, _, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, first.ID)
	if err != nil {
		t.Fatalf("callUpstream: %v", err)
	}
	if !strings.Contains(string(payload), "refreshed") {
		t.Fatalf("payload = %s", payload)
	}
	if atomic.LoadInt32(&refreshes) != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	if atomic.LoadInt32(&callsB) != 0 {
		t.Fatalf("account b should not be needed: %d", callsB)
	}
	if next := a.poolAccount(first.ID); next == nil || next.AccessToken != newToken {
		t.Fatalf("refreshed token not stored: %+v", next)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	cases := []struct {
		header map[string]string
		want   time.Duration
	}{
		{map[string]string{"Retry-After": "30"}, 30 * time.Second},
		{map[string]string{"Retry-After-Ms": "1500"}, 1500 * time.Millisecond},
		{map[string]string{"X-Ratelimit-Reset": fmt.Sprintf("%d", time.Now().Add(45*time.Second).Unix())}, 45 * time.Second},
		{map[string]string{"Retry-After": time.Now().Add(20 * time.Second).UTC().Format(http.TimeFormat)}, 20 * time.Second},
		{map[string]string{"Retry-After": "0"}, 0},
		{map[string]string{}, 0},
	}
	for _, tc := range cases {
		headers := http.Header{}
		for key, value := range tc.header {
			headers.Set(key, value)
		}
		got := retryAfter(headers)
		if tc.want == 0 {
			if got != 0 {
				t.Fatalf("%v => %v, want 0", tc.header, got)
			}
			continue
		}
		if diff := got - tc.want; diff > 3*time.Second || diff < -3*time.Second {
			t.Fatalf("%v => %v, want ~%v", tc.header, got, tc.want)
		}
	}
}

func TestBackoffHasJitter(t *testing.T) {
	cfg := defaultConfig()
	cfg.RetryJitter = true
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		seen[backoff(2, cfg)] = true
	}
	if len(seen) < 2 {
		t.Fatal("jitter produced identical delays")
	}
	cfg.RetryJitter = false
	if got := backoff(2, cfg); got != 800*time.Millisecond {
		t.Fatalf("no-jitter backoff = %v, want 800ms", got)
	}
}

// failingReader yields the payload once, then fails: a stream cut mid-flight.
type failingReader struct {
	data []byte
	done bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.done {
		f.done = true
		return copy(p, f.data), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func TestPumpStreamClosesPolitelyAfterPartialContent(t *testing.T) {
	host := &fakeHost{}
	host.install(t)
	a := newTestApp(t)

	body := "data: {\"id\":\"c1\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"
	reader := &failingReader{data: []byte(body)}
	if err := a.pumpStream("s1", "m", io.NopCloser(reader), nil, nil); err != nil {
		t.Fatalf("partial stream should end cleanly, got %v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.emits) != 2 {
		t.Fatalf("emits = %d, want content + finish", len(host.emits))
	}
	last := string(host.emits[1])
	if !strings.Contains(last, `"finish_reason":"stop"`) || !strings.Contains(last, "upstream_stream_error") {
		t.Fatalf("finish chunk = %s", last)
	}
}

func TestPumpStreamFailsBeforeAnyContent(t *testing.T) {
	host := &fakeHost{}
	host.install(t)
	a := newTestApp(t)

	reader := &failingReader{data: []byte(": keep-alive\n\n")}
	err := a.pumpStream("s1", "m", io.NopCloser(reader), nil, nil)
	if err == nil {
		t.Fatal("a stream that dies before content must fail")
	}
}

func TestExchangeAuthorizationCode(t *testing.T) {
	token := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-login", "ses-login")
	var sawCode string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code string `json:"authorization_code"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		sawCode = body.Code
		fmt.Fprintf(w, `{"code":0,"message":"success","data":{"access_token":%q,"refresh_token":"r1","office_identity":"personal"}}`, token)
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = server.URL
	a.cfg = cfg

	account, err := a.exchangeAuthorizationCode(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if sawCode != "the-code" {
		t.Fatalf("upstream saw code %q", sawCode)
	}
	if account.AccessToken != token || account.RefreshToken != "r1" {
		t.Fatalf("account = %+v", account)
	}
	if account.Subject != "pu-login" {
		t.Fatalf("subject = %s", account.Subject)
	}
}

func TestExchangeAuthorizationCodeRejectsUsedCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":200035,"message":"authorization_code_not_found_error"}`)
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = server.URL
	a.cfg = cfg

	if _, err := a.exchangeAuthorizationCode(context.Background(), "used"); err == nil {
		t.Fatal("expected a 200035 failure")
	} else if !strings.Contains(err.Error(), "已被使用") {
		t.Fatalf("error = %v", err)
	}
}

func TestCompleteLoginAddsAccount(t *testing.T) {
	host := &fakeHost{}
	host.install(t)
	token := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-login", "ses-login")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":0,"data":{"access_token":%q,"refresh_token":"r1"}}`, token)
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = server.URL
	a.cfg = cfg

	session := a.newLoginSession()
	account, err := a.completeLogin(context.Background(), session.State, "office-raccoon://auth/callback?code=abc123", "work")
	if err != nil {
		t.Fatalf("completeLogin: %v", err)
	}
	if account.Alias != "work" || account.ID != "raccoon2api-work" {
		t.Fatalf("account identity = %+v", account)
	}
	if got := a.poolAccount("raccoon2api-work"); got == nil || got.Source != "login" {
		t.Fatalf("pool = %+v", got)
	}
	if host.count(methodHostAuthSave) != 1 {
		t.Fatalf("auth record not written: %d", host.count(methodHostAuthSave))
	}
	// The CPA login poll reports the finished session.
	polled, err := a.pollLogin(mustJSON(t, map[string]string{"State": session.State}))
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !strings.Contains(string(polled), `"Status":"success"`) {
		t.Fatalf("poll response = %s", polled)
	}
}

func TestParseAuthWithAuthorizationCode(t *testing.T) {
	host := &fakeHost{}
	host.install(t)
	token := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-code", "ses-code")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":0,"data":{"access_token":%q}}`, token)
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = server.URL
	a.cfg = cfg

	raw := mustJSON(t, map[string]any{
		"Provider": providerKey,
		"FileName": "team-b.json",
		"RawJSON":  []byte(`{"type":"raccoon2api","authorization_code":"abc"}`),
	})
	out, err := a.parseAuth(raw)
	if err != nil {
		t.Fatalf("parseAuth: %v", err)
	}
	if !strings.Contains(string(out), `"Handled":true`) {
		t.Fatalf("response = %s", out)
	}
	if got := a.poolAccount("raccoon2api-team-b"); got == nil {
		t.Fatalf("account not registered: %+v", a.poolSnapshot())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestDeadAccountStaysDeadAndIsSkipped(t *testing.T) {
	a := newTestApp(t)
	now := time.Now()
	broken := testAccount("raccoon2api-broken", 30, now.Add(time.Hour))
	a.upsertAccount(broken)
	a.markAccountDead(broken.ID, "authorization_verify_error")

	dead := a.poolAccount(broken.ID)
	if !dead.Dead {
		t.Fatal("account should be marked dead")
	}
	if !dead.cooling(now) {
		t.Fatal("a dead account must also cool down, otherwise it is retried first every time")
	}

	// Re-importing the same token must not resurrect it.
	again := testAccount("raccoon2api-broken", 30, now.Add(time.Hour))
	again.AccessToken = broken.AccessToken
	a.upsertAccount(again)
	if !a.poolAccount(broken.ID).Dead {
		t.Fatal("same token must keep the dead flag")
	}

	healthy := testAccount("raccoon2api-good", 10, now.Add(time.Hour))
	a.upsertAccount(healthy)
	if got := a.selectAccount("", nil, now); got == nil || got.ID != healthy.ID {
		t.Fatalf("dead account selected while a healthy one exists: %+v", got)
	}
}

func TestSyncFromHostDoesNotDeleteWhenHostIsEmpty(t *testing.T) {
	previous := hostCall
	defer func() { hostCall = previous }()
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method == methodHostAuthList {
			return json.RawMessage(`{"files":[]}`), nil
		}
		return json.RawMessage(`{}`), nil
	}

	a := newTestApp(t)
	primary := testAccount("raccoon2api-default", 10, time.Now().Add(time.Hour))
	primary.Source = "desktop"
	a.upsertAccount(primary)

	a.syncAccountsFromHost()
	if a.poolSize() != 1 {
		t.Fatalf("an empty host list must not wipe the pool: %d", a.poolSize())
	}
}

func TestSyncFromHostKeepsAccountWhenFileNameDiffers(t *testing.T) {
	record, _ := json.Marshal(map[string]any{
		"type":         providerKey,
		"id":           "raccoon2api-default",
		"alias":        "default",
		"access_token": fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-1", "ses-1"),
	})
	previous := hostCall
	defer func() { hostCall = previous }()
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case methodHostAuthList:
			return json.RawMessage(`{"files":[{"name":"raccoon2api-renamed.json","auth_index":"idx-1","provider":"raccoon2api"}]}`), nil
		case methodHostAuthGet:
			raw, _ := json.Marshal(map[string]any{"auth_index": "idx-1", "json": json.RawMessage(record)})
			return raw, nil
		}
		return json.RawMessage(`{}`), nil
	}

	a := newTestApp(t)
	primary := testAccount("raccoon2api-default", 10, time.Now().Add(time.Hour))
	primary.Source = "desktop"
	a.upsertAccount(primary)

	a.syncAccountsFromHost()
	if a.poolAccount("raccoon2api-default") == nil {
		t.Fatal("account deleted because its file name did not match the record id")
	}
}

func TestRefreshKeepsAccountSource(t *testing.T) {
	token := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-a", "ses-a")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":0,"data":{"access_token":%q,"refresh_token":"r2"}}`, token)
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = server.URL
	a.cfg = cfg

	account := testAccount("raccoon2api-default", 10, time.Now().Add(time.Minute))
	account.Source = "desktop"
	account.RefreshToken = "r1"
	a.upsertAccount(account)

	if _, err := a.refreshAccount(context.Background(), account); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	refreshed := a.poolAccount(account.ID)
	if refreshed.Source != "desktop" {
		t.Fatalf("source = %q, want desktop (host sync would otherwise delete it)", refreshed.Source)
	}
}

func TestLastFailureStatusSurvivesExhaustion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"message":"rate limited"}`)
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = server.URL
	cfg.MaxAttempts = 2
	a.cfg = cfg
	account := &credential{
		ID: "raccoon2api-a", Alias: "a", Priority: 10,
		AccessToken: "token-a", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	a.upsertAccount(account)

	_, _, _, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, account.ID)
	if err == nil {
		t.Fatal("expected a failure")
	}
	failure := asFailure(err)
	if failure.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (not a synthetic 503)", failure.Status)
	}
}
