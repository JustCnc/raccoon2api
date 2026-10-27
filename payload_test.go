package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeChatPayloadDropsUnknownFields(t *testing.T) {
	raw := []byte(`{
		"model":"sn-glm-5-3-flash",
		"messages":[{"role":"user","content":"hi"}],
		"conversation_id":"abc",
		"cache_control":{"type":"ephemeral"},
		"prompt_cache_key":"k",
		"temperature":0.4
	}`)
	out, dropped, err := normalizeChatPayload(raw, "sn-glm-5-3-flash", 80000, 0, false, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"conversation_id", "cache_control", "prompt_cache_key"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("%s should have been dropped", key)
		}
	}
	if _, ok := fields["temperature"]; !ok {
		t.Fatal("temperature should survive")
	}
	if string(fields["max_tokens"]) != "80000" {
		t.Fatalf("max_tokens default not injected: %s", fields["max_tokens"])
	}
	if len(dropped) != 3 {
		t.Fatalf("dropped = %v, want 3 entries", dropped)
	}
}

func TestNormalizeChatPayloadRenamesMaxCompletionTokens(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[],"max_completion_tokens":128}`)
	out, _, err := normalizeChatPayload(raw, "m", 80000, 0, false, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if strings.Contains(string(out), "max_completion_tokens") {
		t.Fatalf("max_completion_tokens leaked: %s", out)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(out, &fields)
	if string(fields["max_tokens"]) != "128" {
		t.Fatalf("max_tokens = %s, want 128", fields["max_tokens"])
	}
}

func TestNormalizeChatPayloadStreamOptions(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":false}}`)
	out, _, err := normalizeChatPayload(raw, "m", 0, 0, true, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(out, &fields)
	if string(fields["stream"]) != "true" {
		t.Fatalf("stream = %s", fields["stream"])
	}
	if !strings.Contains(string(fields["stream_options"]), "include_usage") {
		t.Fatalf("stream_options = %s", fields["stream_options"])
	}
}

func TestNormalizeChatPayloadRoles(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[
		{"role":"function","name":"f","content":"x"},
		{"role":"function","tool_call_id":"call_1","content":"y"},
		{"role":"system","content":"s"}
	]}`)
	out, _, err := normalizeChatPayload(raw, "m", 0, 0, false, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var fields struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := []string{fields.Messages[0].Role, fields.Messages[1].Role, fields.Messages[2].Role}
	want := []string{"user", "tool", "system"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("roles = %v, want %v", got, want)
		}
	}
}

func TestNormalizeReasoningEffortForGLM(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[],"reasoning_effort":"off"}`)
	out, _, err := normalizeChatPayload(raw, "sn-glm-5-3-flash", 0, 0, false, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !strings.Contains(string(out), `"reasoning_effort":"low"`) {
		t.Fatalf("reasoning_effort not normalized: %s", out)
	}
}

func TestNormalizeChatPayloadHoistsExtraBody(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[],"extra_body":{"thinking":{"type":"disabled"},"ignored":1}}`)
	out, _, err := normalizeChatPayload(raw, "m", 0, 0, false, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if strings.Contains(string(out), "extra_body") {
		t.Fatalf("extra_body leaked: %s", out)
	}
	if !strings.Contains(string(out), `"thinking":{"type":"disabled"}`) {
		t.Fatalf("thinking not hoisted: %s", out)
	}
}

func TestNormalizeChatPayloadFieldOrder(t *testing.T) {
	raw := []byte(`{"temperature":0.1,"model":"m","messages":[],"max_tokens":5}`)
	out, _, err := normalizeChatPayload(raw, "m", 0, 0, false, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	text := string(out)
	if !strings.HasPrefix(text, `{"messages":[],"max_tokens":5,"model":"m"`) {
		t.Fatalf("unexpected field order: %s", text)
	}
}

func TestConversationStability(t *testing.T) {
	cred := &credential{Subject: "pu-1"}
	payload := map[string]json.RawMessage{
		"messages": json.RawMessage(`[
			{"role":"system","content":"be nice"},
			{"role":"user","content":"第一个问题"},
			{"role":"assistant","content":"回答"},
			{"role":"user","content":"第二个问题"}
		]`),
	}
	first := conversationFrom(cred, nil, payload)
	second := conversationFrom(cred, nil, payload)
	if first.SessionID != second.SessionID {
		t.Fatalf("session id not stable: %s vs %s", first.SessionID, second.SessionID)
	}
	if !strings.HasPrefix(first.SessionID, "ses-") {
		t.Fatalf("session id shape: %s", first.SessionID)
	}
	if !strings.HasSuffix(first.TurnID, "-turn-2") {
		t.Fatalf("turn id = %s, want suffix -turn-2", first.TurnID)
	}
	if first.Title != "第一个问题" {
		t.Fatalf("title = %q", first.Title)
	}

	ascii := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"hello world"}]`),
	}
	if got := conversationFrom(cred, nil, ascii).Title; got != "hello world" {
		t.Fatalf("title = %q", got)
	}
}

func TestConversationUsesClientSessionHeader(t *testing.T) {
	headers := map[string][]string{"X-Session-Id": {"client-session-1"}}
	conv := conversationFrom(&credential{Subject: "pu-1"}, headers, map[string]json.RawMessage{})
	other := conversationFrom(&credential{Subject: "pu-2"}, headers, map[string]json.RawMessage{})
	if conv.SessionID == other.SessionID {
		t.Fatal("different accounts must not share a session id")
	}
}

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
	}{
		{401, `{"code":200003,"message":"authorization_verify_error"}`, 401},
		{500, `{"code":1000007,"message":"insufficient credits"}`, 402},
		{200, `{"error":{"code":"500","message":"Unexpected keyword argument 'x'."}}`, 502},
		{429, `{"message":"rate limited"}`, 429},
		{400, `{"error":{"code":"400","message":"do not support role function"}}`, 400},
	}
	for _, tc := range cases {
		failure := classifyFailure(tc.status, []byte(tc.body))
		if failure.Status != tc.want {
			t.Fatalf("status %d body %s => %d, want %d", tc.status, tc.body, failure.Status, tc.want)
		}
	}
}

func TestAggregateChatStream(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		``,
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
		``,
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		``,
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		``,
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"total_tokens":9}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	out, failure := aggregateChatStream([]byte(stream))
	if failure != nil {
		t.Fatalf("aggregate failed: %+v", failure)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(out, &completion); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if completion.Choices[0].Message.Content != "hello" {
		t.Fatalf("content = %q", completion.Choices[0].Message.Content)
	}
	if completion.Choices[0].Message.ReasoningContent != "think" {
		t.Fatalf("reasoning = %q", completion.Choices[0].Message.ReasoningContent)
	}
	if completion.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", completion.Choices[0].FinishReason)
	}
	if len(completion.Choices[0].Message.ToolCalls) != 1 || completion.Choices[0].Message.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool calls = %+v", completion.Choices[0].Message.ToolCalls)
	}
	if completion.Usage["total_tokens"] != float64(9) {
		t.Fatalf("usage = %v", completion.Usage)
	}
}

func TestReasoningEffortDefaultAndClientOverride(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[]}`)
	out, _, err := normalizeChatPayload(raw, "m", 0, 0, false, "low")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !strings.Contains(string(out), `"reasoning_effort":"low"`) {
		t.Fatalf("configured default not injected: %s", out)
	}

	raw = []byte(`{"model":"m","messages":[],"reasoning_effort":"high"}`)
	out, _, err = normalizeChatPayload(raw, "m", 0, 0, false, "low")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !strings.Contains(string(out), `"reasoning_effort":"high"`) {
		t.Fatalf("client effort must win: %s", out)
	}

	raw = []byte(`{"model":"m","messages":[],"thinking":{"type":"enabled"}}`)
	out, _, err = normalizeChatPayload(raw, "m", 0, 0, false, "low")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if strings.Contains(string(out), "reasoning_effort") {
		t.Fatalf("thinking control should suppress the default: %s", out)
	}
}

func TestConversationTitleKeepsUnicode(t *testing.T) {
	payload := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"中文 标题\n测试"}]`),
	}
	if got := conversationTitle(payload); got != "中文 标题 测试" {
		t.Fatalf("title = %q", got)
	}
}
