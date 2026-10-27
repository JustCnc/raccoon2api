package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveUpstream exercises the real gateway. It is skipped unless
// RACCOON2API_LIVE=1 and a desktop auth file is available, because it spends
// account credits and needs network access.
//
//	RACCOON2API_LIVE=1 RACCOON2API_AUTH_FILE=~/.office-raccoon/agent/config/auth.json \
//	  CGO_ENABLED=0 go test -run TestLiveUpstream -v
func liveApp(t *testing.T) *app {
	t.Helper()
	if os.Getenv("RACCOON2API_LIVE") != "1" {
		t.Skip("set RACCOON2API_LIVE=1 to hit the real gateway")
	}
	authFile := os.Getenv("RACCOON2API_AUTH_FILE")
	if authFile == "" {
		t.Skip("set RACCOON2API_AUTH_FILE to the desktop auth.json path")
	}
	a := newTestApp(t)
	cfg := a.currentConfig()
	if deviceFile := os.Getenv("RACCOON2API_DEVICE_FILE"); deviceFile != "" {
		cfg.DesktopDeviceFile = deviceFile
	}
	a.cfg = cfg
	// The plugin itself only takes accounts from uploads; the live test reads a
	// captured credential file and feeds it through the same import path.
	raw, errRead := os.ReadFile(authFile)
	if errRead != nil {
		t.Fatalf("read auth file: %v", errRead)
	}
	account, errDecode := decodeCredential(raw)
	if errDecode != nil || account.AccessToken == "" {
		t.Fatalf("auth file not importable: %v", errDecode)
	}
	account.Alias = "live"
	account.ID = accountIDForAlias("live")
	account.Source = "upload"
	a.upsertAccount(account)
	return a
}

func TestLiveUpstream(t *testing.T) {
	a := liveApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := a.refreshCatalog(ctx); err != nil {
		t.Fatalf("model_catalog: %v", err)
	}
	snapshot := a.snapshotCatalog()
	if len(snapshot.Models) == 0 {
		t.Fatal("catalogue is empty")
	}
	t.Logf("catalogue: %d models, default=%s", len(snapshot.Models), snapshot.Default)

	model := snapshot.Default
	for _, candidate := range snapshot.Models {
		if candidate.ID == "sn-glm-5-3-flash" {
			model = candidate.ID
		}
	}

	// Non-streaming call through the plugin entry point.
	request := map[string]any{
		"model":      model,
		"messages":   []map[string]any{{"role": "user", "content": "只回复两个字：你好"}},
		"max_tokens": 64,
	}
	raw, _ := json.Marshal(request)
	rpcRequest, _ := json.Marshal(map[string]any{
		"Payload": raw,
		"Model":   model,
		"Format":  "openai",
	})
	out, err := a.execute(rpcRequest)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Payload []byte `json:"Payload"`
		} `json:"result"`
		Error *envelopeError `json:"error"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !envelope.OK {
		t.Fatalf("upstream error: %+v", envelope.Error)
	}
	preview := string(envelope.Result.Payload)
	if !strings.Contains(preview, "chat.completion") {
		t.Fatalf("unexpected payload: %s", preview)
	}
	t.Logf("non-stream ok: %s", truncateRunes(preview, 200))

	// Streaming call: pump into a fake host stream and count events.
	var events []string
	previous := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case methodHostStreamEmit:
			var req streamEmitRequest
			rawPayload, _ := json.Marshal(payload)
			_ = json.Unmarshal(rawPayload, &req)
			events = append(events, string(req.Payload))
		case methodHostStreamClose:
			var req streamCloseRequest
			rawPayload, _ := json.Marshal(payload)
			_ = json.Unmarshal(rawPayload, &req)
			if req.Error != "" {
				t.Logf("stream closed with error: %s", req.Error)
			}
		}
		return json.RawMessage(`{}`), nil
	}
	defer func() { hostCall = previous }()

	streamRequest := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "数到三"}},
		"stream":   true,
	}
	rawStream, _ := json.Marshal(streamRequest)

	rpc := map[string]any{
		"Payload":   rawStream,
		"Model":     model,
		"Format":    "openai",
		"Stream":    true,
		"stream_id": "test-stream",
	}
	rpcRaw, _ := json.Marshal(rpc)
	if _, err := a.executeStream(rpcRaw); err != nil {
		t.Fatalf("executeStream: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if len(events) > 0 && strings.Contains(events[len(events)-1], "usage") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(events) < 3 {
		t.Fatalf("stream produced %d events", len(events))
	}
	last := events[len(events)-1]
	if !strings.Contains(last, "usage") {
		t.Logf("last event: %s", truncateRunes(last, 300))
	}
	t.Logf("stream ok: %d events, first=%s", len(events), truncateRunes(events[0], 160))
}

// TestLiveUnknownModelRejected makes sure the catalogue gate answers before the
// gateway silently falls back to its default model.
func TestLiveUnknownModelRejected(t *testing.T) {
	a := liveApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := a.refreshCatalog(ctx); err != nil {
		t.Fatalf("model_catalog: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"model":    "definitely-not-a-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	raw, _ := json.Marshal(map[string]any{
		"Payload": body,
		"Model":   "definitely-not-a-model",
		"Format":  "openai",
	})
	out, err := a.execute(raw)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var envelope struct {
		OK    bool `json:"ok"`
		Error *envelopeError
	}
	if json.Unmarshal(out, &envelope) != nil {
		t.Fatalf("decode: %s", out)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != 404 {
		t.Fatalf("expected a 404 envelope, got %s", out)
	}
}

// TestLiveNonASCIITitle checks that a UTF-8 conversation title survives the Go
// transport. The official runtime sends the title as raw UTF-8 bytes; if the
// transport refuses, the plugin must drop the header instead of failing.
func TestLiveNonASCIITitle(t *testing.T) {
	a := liveApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cred, err := a.anyUsableCredential(ctx)
	if err != nil && (cred == nil || cred.AccessToken == "") {
		t.Fatalf("credential: %v", err)
	}
	body := []byte(`{"model":"sn-glm-5-3-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
	conv := conversation{SessionID: "ses-00000000-0000-7000-8000-000000000000", TurnID: "ses-00000000-0000-7000-8000-000000000000-turn-1", Title: "中文标题测试"}
	resp, err := a.send(ctx, a.currentConfig(), "sn-glm-5-3-flash", body, conv, cred, false)
	if err != nil {
		t.Fatalf("transport rejected the UTF-8 title: %v", err)
	}
	defer resp.Body.Close()
	t.Logf("status=%d", resp.StatusCode)
	if resp.StatusCode != 200 {
		payload, _ := readLimited(resp.Body, 1<<16)
		t.Fatalf("unexpected status %d: %s", resp.StatusCode, payload)
	}
}
