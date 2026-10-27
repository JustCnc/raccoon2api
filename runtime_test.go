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

// fakeHost replaces hostCall for the duration of a test and records calls.
type fakeHost struct {
	mu    sync.Mutex
	calls []string
	emits [][]byte
	// emitErr, when set, is returned for host.stream.emit.
	emitErr error
}

func (f *fakeHost) install(t *testing.T) {
	t.Helper()
	previous := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		f.mu.Lock()
		f.calls = append(f.calls, method)
		f.mu.Unlock()
		if method == methodHostStreamEmit {
			rawPayload, _ := json.Marshal(payload)
			var emit streamEmitRequest
			if json.Unmarshal(rawPayload, &emit) == nil {
				f.mu.Lock()
				f.emits = append(f.emits, emit.Payload)
				f.mu.Unlock()
			}
			if f.emitErr != nil {
				return nil, f.emitErr
			}
		}
		return json.RawMessage(`{}`), nil
	}
	t.Cleanup(func() { hostCall = previous })
}

func (f *fakeHost) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, call := range f.calls {
		if call == method {
			total++
		}
	}
	return total
}

func TestPumpStreamForwardsEventsAndClosesWithFinish(t *testing.T) {
	host := &fakeHost{}
	host.install(t)
	a := newTestApp(t)

	body := strings.Join([]string{
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"he"}}]}`,
		``,
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"llo"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	if err := a.pumpStream("s1", "m", io.NopCloser(strings.NewReader(body)), nil, nil); err != nil {
		t.Fatalf("pump: %v", err)
	}
	// Two upstream events plus one synthesised finish chunk (the stream had none).
	if got := host.count(methodHostStreamEmit); got != 3 {
		t.Fatalf("emits = %d, want 3", got)
	}
	host.mu.Lock()
	last := string(host.emits[len(host.emits)-1])
	host.mu.Unlock()
	if !strings.Contains(last, `"finish_reason":"stop"`) || !strings.Contains(last, `"id":"c1"`) {
		t.Fatalf("synthesised finish chunk = %s", last)
	}
}

func TestPumpStreamDoesNotDuplicateFinish(t *testing.T) {
	host := &fakeHost{}
	host.install(t)
	a := newTestApp(t)

	body := strings.Join([]string{
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"x"}}]}`,
		``,
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	if err := a.pumpStream("s1", "m", io.NopCloser(strings.NewReader(body)), nil, nil); err != nil {
		t.Fatalf("pump: %v", err)
	}
	if got := host.count(methodHostStreamEmit); got != 2 {
		t.Fatalf("emits = %d, want 2", got)
	}
}

func TestPumpStreamStopsWhenEmitFails(t *testing.T) {
	host := &fakeHost{emitErr: fmt.Errorf("stream gone")}
	host.install(t)
	a := newTestApp(t)

	body := strings.Join([]string{
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"x"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	err := a.pumpStream("s1", "m", io.NopCloser(strings.NewReader(body)), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "stream gone") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshCredentialIsSingleFlight(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(30 * time.Millisecond)
		fmt.Fprintf(w, `{"code":0,"message":"success","data":{"access_token":%q,"refresh_token":"r2"}}`,
			fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-1", "ses-1"))
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = server.URL
	a.cfg = cfg

	cred := &credential{ID: "raccoon2api-a", AccessToken: "old", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	a.upsertAccount(cred)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.refreshAccount(context.Background(), cred); err != nil {
				t.Errorf("refresh: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh endpoint called %d times, want 1", got)
	}
	if next := a.poolAccount("raccoon2api-a"); next == nil || next.AccessToken == "old" {
		t.Fatalf("credential not replaced: %+v", next)
	}
}

func TestRefreshCredentialSkipsWhenTokenAlreadyReplaced(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, `{"code":0,"data":{"access_token":"should-not-be-used"}}`)
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = server.URL
	a.cfg = cfg
	fresh := &credential{ID: "raccoon2api-a", AccessToken: "fresh", RefreshToken: "r1", ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli()}
	a.upsertAccount(fresh)

	next, err := a.refreshAccount(context.Background(), &credential{ID: "raccoon2api-a", AccessToken: "stale", RefreshToken: "r1"})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if next == nil || next.AccessToken != "fresh" {
		t.Fatalf("expected the in-memory token, got %+v", next)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("refresh endpoint called %d times, want 0", got)
	}
}

func TestCallUpstreamRefreshesAndRetriesWithSingleAttempt(t *testing.T) {
	var chatCalls int32
	var refreshCalls int32
	newToken := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-1", "ses-1")

	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&chatCalls, 1) {
		case 1:
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"code":200003,"message":"authorization_verify_error"}`)
		default:
			if got := r.Header.Get("Authorization"); got != "Bearer "+newToken {
				t.Errorf("retry used %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		}
	}))
	defer chatServer.Close()

	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshCalls, 1)
		fmt.Fprintf(w, `{"code":0,"data":{"access_token":%q,"refresh_token":"r2"}}`, newToken)
	}))
	defer authServer.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = chatServer.URL
	cfg.AuthBaseURL = authServer.URL
	cfg.MaxAttempts = 1 // the post-refresh retry must not consume the budget
	a.cfg = cfg

	// Still valid by the clock: the only refresh in this test is the 401-driven
	// one, which must not consume the single attempt budget.
	account := &credential{
		ID: "raccoon2api-a", Alias: "a", Priority: 10,
		AccessToken:  fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-1", "ses-1"),
		RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	a.upsertAccount(account)

	headers, payload, _, err := a.callUpstream(context.Background(), cfg, "sn-glm-5-3-flash",
		[]byte(`{"model":"sn-glm-5-3-flash","messages":[]}`), nil, account.ID)
	if err != nil {
		t.Fatalf("callUpstream: %v", err)
	}
	if !strings.Contains(string(payload), `"ok"`) {
		t.Fatalf("payload = %s", payload)
	}
	if headers == nil {
		t.Fatal("headers missing")
	}
	if got := atomic.LoadInt32(&chatCalls); got != 2 {
		t.Fatalf("chat calls = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestManagementRoutesSurviveRenamedPluginFile(t *testing.T) {
	cases := map[string]string{
		"/v0/management/raccoon2api/status":                         "/status",
		"/v0/resource/plugins/raccoon2api/console":                  "/console",
		"/v0/resource/plugins/raccoon2api-linux-amd64/console":      "/console",
		"/v0/management/raccoon2api-windows-amd64/accounts/refresh": "/accounts/refresh",
		"/v0/management/raccoon2api-windows-amd64/accounts":         "/accounts",
		"/v0/management/raccoon2api/login/complete":                 "/login/complete",
		"/v0/management/raccoon2api/logs":                           "/logs",
	}
	for input, want := range cases {
		if got := normalizeManagementPath(input); got != want {
			t.Fatalf("normalizeManagementPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPublishCredentialWritesOnce(t *testing.T) {
	host := &fakeHost{}
	host.install(t)
	a := newTestApp(t)
	account := &credential{
		ID:          "raccoon2api-default",
		Alias:       "default",
		AccessToken: fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-1", "ses-1"),
		Source:      "desktop",
		UpdatedAt:   time.Now().UnixMilli(),
	}
	a.publishAccount(account, false)
	if got := host.count(methodHostAuthSave); got != 1 {
		t.Fatalf("host.auth.save calls = %d, want 1", got)
	}
	a.publishAccount(account, false)
	if got := host.count(methodHostAuthSave); got != 1 {
		t.Fatalf("unchanged record rewritten: %d", got)
	}
	account.Priority++
	a.publishAccount(account, false)
	if got := host.count(methodHostAuthSave); got != 2 {
		t.Fatalf("changed record should be written: %d", got)
	}
}

func TestNormalizeChatPayloadClampsMaxTokens(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[]}`)
	out, _, err := normalizeChatPayload(raw, "m", 80000, 63999, false, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(out, &fields)
	if string(fields["max_tokens"]) != "63999" {
		t.Fatalf("max_tokens = %s, want the catalogue cap", fields["max_tokens"])
	}

	raw = []byte(`{"model":"m","messages":[],"max_tokens":100000}`)
	out, _, err = normalizeChatPayload(raw, "m", 80000, 63999, false, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	_ = json.Unmarshal(out, &fields)
	if string(fields["max_tokens"]) != "63999" {
		t.Fatalf("explicit max_tokens not clamped: %s", fields["max_tokens"])
	}

	raw = []byte(`{"model":"m","messages":[],"max_tokens":100}`)
	out, _, err = normalizeChatPayload(raw, "m", 80000, 63999, false, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	_ = json.Unmarshal(out, &fields)
	if string(fields["max_tokens"]) != "100" {
		t.Fatalf("smaller max_tokens must survive: %s", fields["max_tokens"])
	}
}

func TestAggregateChatStreamReportsErrorFrame(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"error":{"code":1000007,"message":"insufficient credits"}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	payload, failure := aggregateChatStream([]byte(stream))
	if failure == nil {
		t.Fatalf("expected a failure, got %s", payload)
	}
	if failure.Status != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", failure.Status)
	}
}

func TestCompletionToChunks(t *testing.T) {
	completion := []byte(`{"id":"c1","object":"chat.completion","created":7,"model":"m","usage":{"total_tokens":5},
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi","reasoning_content":"think"}}]}`)
	chunks := completionToChunks(completion)
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	last := string(chunks[len(chunks)-1])
	if !strings.Contains(last, `"finish_reason":"stop"`) || !strings.Contains(last, `"usage"`) {
		t.Fatalf("terminal chunk = %s", last)
	}
	if !strings.Contains(string(chunks[1]), "think") {
		t.Fatalf("reasoning chunk missing: %s", chunks[1])
	}
}

func TestPumpStreamSynthesisesToolCallsFinish(t *testing.T) {
	host := &fakeHost{}
	host.install(t)
	a := newTestApp(t)

	body := strings.Join([]string{
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	if err := a.pumpStream("s1", "m", io.NopCloser(strings.NewReader(body)), nil, nil); err != nil {
		t.Fatalf("pump: %v", err)
	}
	host.mu.Lock()
	last := string(host.emits[len(host.emits)-1])
	host.mu.Unlock()
	if !strings.Contains(last, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish reason after tool deltas = %s", last)
	}
}

func TestPumpStreamFailsOnErrorFrame(t *testing.T) {
	host := &fakeHost{}
	host.install(t)
	a := newTestApp(t)

	body := strings.Join([]string{
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		``,
		`data: {"code":1000007,"message":"insufficient credits"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	err := a.pumpStream("s1", "m", io.NopCloser(strings.NewReader(body)), nil, nil)
	if err == nil {
		t.Fatal("expected the error frame to abort the stream")
	}
	if failure, ok := err.(*upstreamFailure); !ok || failure.Status != http.StatusPaymentRequired {
		t.Fatalf("err = %v, want a 402 upstreamFailure", err)
	}
	// The partial content is forwarded; no synthetic finish may follow the error.
	if got := host.count(methodHostStreamEmit); got != 1 {
		t.Fatalf("emits = %d, want 1", got)
	}
}

func TestEnsureAuthRecordRepublishesDeletedRecord(t *testing.T) {
	host := &fakeHost{}
	host.install(t)
	a := newTestApp(t)

	cred := &credential{
		ID:          "raccoon2api-default",
		Alias:       "default",
		AccessToken: fakeJWT(time.Now().Add(time.Hour).Unix(), "pu-1", "ses-1"),
		Source:      "upload",
		UpdatedAt:   time.Now().UnixMilli(),
	}
	a.upsertAccount(cred)
	a.publishAccount(cred, false)
	if got := host.count(methodHostAuthSave); got != 1 {
		t.Fatalf("saves = %d, want 1", got)
	}
	// The host reports no records for this provider: the record must be rebuilt
	// even though the fingerprint is unchanged.
	a.ensureHostRecords()
	if got := host.count(methodHostAuthSave); got != 2 {
		t.Fatalf("saves after repair = %d, want 2", got)
	}
}

func TestRefreshFailureReleasesFlight(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"code":500,"message":"boom"}`)
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.AuthBaseURL = server.URL
	a.cfg = cfg
	cred := &credential{ID: "raccoon2api-a", AccessToken: "old", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	a.upsertAccount(cred)

	var wg sync.WaitGroup
	var failures int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.refreshAccount(context.Background(), cred); err != nil {
				atomic.AddInt32(&failures, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&failures); got != 4 {
		t.Fatalf("failures = %d, want 4", got)
	}
	// The flight must be cleared so a later attempt can run again.
	if _, err := a.refreshAccount(context.Background(), cred); err == nil {
		t.Fatal("expected the retry to fail as well")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("refresh calls after retry = %d, want 2", got)
	}
}
