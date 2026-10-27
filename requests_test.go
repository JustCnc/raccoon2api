package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseUsageReadsCacheAndReasoning(t *testing.T) {
	raw := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140,
		"prompt_tokens_details":{"cached_tokens":80},
		"completion_tokens_details":{"reasoning_tokens":30}}}`)
	usage := parseUsage(raw)
	if usage == nil {
		t.Fatal("usage not parsed")
	}
	var entry requestEntry
	entry.applyUsage(usage)
	if entry.PromptTokens != 100 || entry.CompletionTokens != 40 || entry.TotalTokens != 140 {
		t.Fatalf("counters: %+v", entry)
	}
	if entry.CachedTokens != 80 || entry.ReasoningTokens != 30 {
		t.Fatalf("details: %+v", entry)
	}
	if entry.CacheRate != 0.8 {
		t.Fatalf("cache rate = %v, want 0.8", entry.CacheRate)
	}
}

func TestParseUsageMissingBlock(t *testing.T) {
	if usage := parseUsage([]byte(`{"choices":[]}`)); usage != nil {
		t.Fatalf("usage = %+v, want nil", usage)
	}
	if usage := parseUsage(nil); usage != nil {
		t.Fatalf("usage = %+v, want nil", usage)
	}
}

func TestReasoningEffortOfReadsEffectiveValue(t *testing.T) {
	cases := map[string]string{
		`{"reasoning_effort":"low"}`:  "low",
		`{"reasoning_effort":"high"}`: "high",
		`{"thinking":true}`:           "on",
		`{"enable_thinking":false}`:   "off",
		`{"model":"m"}`:               "",
	}
	for body, want := range cases {
		if got := reasoningEffortOf([]byte(body)); got != want {
			t.Fatalf("reasoningEffortOf(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestRequestLogKeepsNewestFirstAndBounds(t *testing.T) {
	log := newRequestLog(3)
	for index := 0; index < 5; index++ {
		log.add(requestEntry{At: int64(index), Model: "m"})
	}
	entries := log.tail(0)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want the 3-entry cap", len(entries))
	}
	if entries[0].At != 4 || entries[2].At != 2 {
		t.Fatalf("order wrong: %+v", entries)
	}
}

func TestRequestLogSummaryAverages(t *testing.T) {
	log := newRequestLog(10)
	log.add(requestEntry{Status: 200, TTFTms: 100, TotalMs: 900, PromptTokens: 100, CachedTokens: 50, Stream: true})
	log.add(requestEntry{Status: 500, TTFTms: 300, TotalMs: 1100, PromptTokens: 100, CachedTokens: 50})
	summary := log.summary()
	if summary["total"] != 2 {
		t.Fatalf("total = %v", summary["total"])
	}
	if summary["avg_ttft_ms"] != int64(200) || summary["avg_total_ms"] != int64(1000) {
		t.Fatalf("latency summary = %+v", summary)
	}
	if summary["cache_rate"] != 0.5 || summary["failures"] != int64(1) || summary["streams"] != int64(1) {
		t.Fatalf("summary = %+v", summary)
	}
}

// A full buffered call must show up with its model, effort, latency and usage.
func TestExecuteRecordsRequestEntry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"sn-glm-5-3-flash","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25,"prompt_tokens_details":{"cached_tokens":10}}}`))
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = server.URL
	a.cfg = cfg
	account := testAccount("raccoon2api-live", 10, time.Now().Add(time.Hour))
	a.upsertAccount(account)
	snapshot, err := parseCatalog([]byte(catalogSample))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	a.setCatalog(snapshot)

	payload := `{"model":"sn-glm-5-3-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"reasoning_effort":"low"}`
	raw, _ := json.Marshal(rpcExecutorRequest{executorRequest: executorRequest{
		Format:  formatChat,
		Model:   "sn-glm-5-3-flash",
		Payload: []byte(payload),
	}})
	if _, err := a.execute(raw); err != nil {
		t.Fatalf("execute: %v", err)
	}
	entries := a.requests.tail(1)
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	entry := entries[0]
	if entry.Model != "raccoon/sn-glm-5-3-flash" {
		t.Fatalf("model = %q, want the namespaced id", entry.Model)
	}
	if entry.Reasoning != "low" {
		t.Fatalf("reasoning = %q", entry.Reasoning)
	}
	if entry.Status != http.StatusOK || entry.Account != "live" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.PromptTokens != 20 || entry.CachedTokens != 10 || entry.CacheRate != 0.5 {
		t.Fatalf("usage = %+v", entry)
	}
	if entry.TotalMs <= 0 {
		t.Fatalf("total = %d", entry.TotalMs)
	}
}

// A streaming call must report first-token latency and the usage trailer.
func TestStreamRecordsTTFTAndUsage(t *testing.T) {
	chunks := strings.Join([]string{
		`data: {"id":"chatcmpl-2","model":"sn-glm-5-3-flash","created":1,"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"id":"chatcmpl-2","model":"sn-glm-5-3-flash","created":1,"choices":[{"index":0,"delta":{"content":"O"}}]}`,
		`data: {"id":"chatcmpl-2","model":"sn-glm-5-3-flash","created":1,"choices":[{"index":0,"delta":{"content":"K"}}]}`,
		`data: {"id":"chatcmpl-2","model":"sn-glm-5-3-flash","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"chatcmpl-2","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":30,"completion_tokens":2,"total_tokens":32,"prompt_tokens_details":{"cached_tokens":15}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chunks))
	}))
	defer server.Close()

	host := &fakeHost{}
	host.install(t)
	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = server.URL
	a.cfg = cfg
	a.upsertAccount(testAccount("raccoon2api-stream", 10, time.Now().Add(time.Hour)))
	snapshot, err := parseCatalog([]byte(catalogSample))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	a.setCatalog(snapshot)

	raw, _ := json.Marshal(rpcExecutorRequest{
		StreamID: "stream-1",
		executorRequest: executorRequest{
			Format:  formatChat,
			Model:   "sn-glm-5-3-flash",
			Payload: []byte(`{"model":"sn-glm-5-3-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`),
		},
	})
	if _, err := a.executeStream(raw); err != nil {
		t.Fatalf("executeStream: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(a.requests.tail(1)) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	entries := a.requests.tail(1)
	if len(entries) != 1 {
		t.Fatal("stream request was not recorded")
	}
	entry := entries[0]
	if !entry.Stream || entry.Status != http.StatusOK {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.TTFTms < 0 || entry.TotalMs <= 0 {
		t.Fatalf("latency = %+v", entry)
	}
	if entry.PromptTokens != 30 || entry.CachedTokens != 15 || entry.CacheRate != 0.5 {
		t.Fatalf("usage = %+v", entry)
	}
	if entry.ID != "chatcmpl-2" {
		t.Fatalf("id = %q", entry.ID)
	}
}

func TestRequestsEndpointServesEntries(t *testing.T) {
	a := newTestApp(t)
	a.recordRequest(requestEntry{Model: "sn-glm-5-3-flash", Status: 200, TotalMs: 42})
	response := a.handleManagement(managementRequest{Method: "GET", Path: "/requests"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var body struct {
		Summary map[string]any `json:"summary"`
		Entries []requestEntry `json:"entries"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 1 || body.Entries[0].Model != "raccoon/sn-glm-5-3-flash" {
		t.Fatalf("entries = %+v", body.Entries)
	}
	if total, _ := body.Summary["total"].(float64); total != 1 {
		t.Fatalf("summary = %+v", body.Summary)
	}
}

var _ = context.Background

func TestRequestLogAttachesPoints(t *testing.T) {
	log := newRequestLog(10)
	seq := log.add(requestEntry{Model: "m"})
	log.setPoints(seq, -12)
	entries := log.tail(1)
	if len(entries) != 1 || entries[0].Points == nil || *entries[0].Points != -12 {
		t.Fatalf("entry = %+v", entries)
	}
	if summary := log.summary(); summary["points"] != int64(-12) {
		t.Fatalf("summary = %+v", summary)
	}
}

// The executor must attribute the bill row to the same session/turn it sent
// upstream, and the console entry must pick up the signed points total.
func TestExecuteAttachesPointsFromBills(t *testing.T) {
	payload := `{"model":"sn-glm-5-3-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"reasoning_effort":"low"}`
	account := testAccount("raccoon2api-points-exec", 10, time.Now().Add(time.Hour))

	a := newTestApp(t)
	cfg := a.currentConfig()
	a.cfg = cfg
	a.upsertAccount(account)
	// Use the pool copy: upsertAccount derives Subject from the JWT, which is
	// part of the stable session id.
	stored := a.poolAccount(account.ID)
	conv := conversationFrom(stored, nil, mustFields([]byte(payload)))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == pointsBillsPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"biz_id":"` + conv.SessionID + `:` + conv.TurnID + `","biz_type":"chat","points":-6}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl-p","model":"sn-glm-5-3-flash","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer server.Close()

	cfg = a.currentConfig()
	cfg.BaseURL = server.URL
	cfg.AuthBaseURL = server.URL + "/api/web/auth/v1"
	a.cfg = cfg
	snapshot, err := parseCatalog([]byte(catalogSample))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	a.setCatalog(snapshot)

	raw, _ := json.Marshal(rpcExecutorRequest{executorRequest: executorRequest{
		Format:  formatChat,
		Model:   "sn-glm-5-3-flash",
		Payload: []byte(payload),
	}})
	if _, err := a.execute(raw); err != nil {
		t.Fatalf("execute: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		entries := a.requests.tail(1)
		if len(entries) == 1 && entries[0].Points != nil {
			if *entries[0].Points != -6 {
				t.Fatalf("points = %d, want -6", *entries[0].Points)
			}
			if entries[0].SessionID != conv.SessionID || entries[0].TurnID != conv.TurnID {
				t.Fatalf("session/turn = %q/%q, want %q/%q", entries[0].SessionID, entries[0].TurnID, conv.SessionID, conv.TurnID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("points were not attached: %+v", entries)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
