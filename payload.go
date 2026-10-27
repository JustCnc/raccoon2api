package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// The gateway validates keyword arguments strictly: an unknown top-level field
// comes back as HTTP 500 "Unexpected keyword argument". Only well-known chat
// completions fields are forwarded; anything else is dropped and reported.
var chatFieldAllowlist = map[string]bool{
	"messages":              true,
	"model":                 true,
	"stream":                true,
	"stream_options":        true,
	"max_tokens":            true,
	"max_completion_tokens": true,
	"temperature":           true,
	"top_p":                 true,
	"n":                     true,
	"stop":                  true,
	"presence_penalty":      true,
	"frequency_penalty":     true,
	"logit_bias":            true,
	"logprobs":              true,
	"top_logprobs":          true,
	"user":                  true,
	"seed":                  true,
	"response_format":       true,
	"tools":                 true,
	"tool_choice":           true,
	"parallel_tool_calls":   true,
	"service_tier":          true,
	"store":                 true,
	"metadata":              true,
	"modalities":            true,
	"audio":                 true,
	"prediction":            true,
	"reasoning_effort":      true,
	"reasoning":             true,
	"web_search_options":    true,
	"functions":             true,
	"function_call":         true,
	// Provider extensions the official runtime sends through the SDK's
	// extra_body; they arrive as top-level fields on the wire.
	"thinking":        true,
	"enable_thinking": true,
}

// chatFieldOrder mirrors the key order the official runtime produces, so the
// outbound body looks like a capture instead of an alphabetised map.
var chatFieldOrder = []string{
	"messages",
	"max_tokens",
	"model",
	"tools",
	"tool_choice",
	"reasoning_effort",
	"thinking",
	"enable_thinking",
	"stream",
	"stream_options",
}

var supportedRoles = map[string]string{
	"system":    "system",
	"assistant": "assistant",
	"user":      "user",
	"tool":      "tool",
	"developer": "developer",
}

type chatMessage struct {
	Role       string
	Text       string
	ToolCallID string
	Name       string
	Raw        map[string]json.RawMessage
}

// decodeChatPayload splits the request body into top-level fields without
// reordering or rewriting them.
func decodeChatPayload(raw []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("empty request body")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("request body is not a JSON object: %w", err)
	}
	return fields, nil
}

func decodeMessages(fields map[string]json.RawMessage) ([]chatMessage, bool) {
	raw, ok := fields["messages"]
	if !ok {
		return nil, false
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	out := make([]chatMessage, 0, len(items))
	for _, item := range items {
		message := chatMessage{Raw: item}
		_ = json.Unmarshal(item["role"], &message.Role)
		message.Role = strings.TrimSpace(message.Role)
		_ = json.Unmarshal(item["tool_call_id"], &message.ToolCallID)
		_ = json.Unmarshal(item["name"], &message.Name)
		message.Text = messageText(item["content"])
		out = append(out, message)
	}
	return out, true
}

// messageText flattens either a string or a multimodal content array.
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		var kind string
		_ = json.Unmarshal(part["type"], &kind)
		if kind != "text" && kind != "input_text" {
			continue
		}
		var value string
		if json.Unmarshal(part["text"], &value) == nil && value != "" {
			b.WriteString(value)
		}
	}
	return b.String()
}

// normalizeChatPayload rewrites the request body into what the gateway accepts:
// known fields only, supported roles only, a max_tokens floor, and the usage
// flag the gateway needs to emit a final usage chunk.
func normalizeChatPayload(raw []byte, model string, maxTokensDefault int, maxTokensCap int64, stream bool, reasoningEffort string) ([]byte, []string, error) {
	fields, err := decodeChatPayload(raw)
	if err != nil {
		return nil, nil, err
	}
	var dropped []string

	// The SDK-only extra_body envelope never reaches the wire; hoist the
	// provider extensions it carries.
	if extra, ok := fields["extra_body"]; ok {
		delete(fields, "extra_body")
		var nested map[string]json.RawMessage
		if json.Unmarshal(extra, &nested) == nil {
			for _, key := range []string{"thinking", "enable_thinking"} {
				if value, ok := nested[key]; ok {
					if _, exists := fields[key]; !exists {
						fields[key] = value
					}
				}
			}
		}
	}

	for key := range fields {
		if !chatFieldAllowlist[key] {
			delete(fields, key)
			dropped = append(dropped, key)
		}
	}

	if _, ok := fields["model"]; !ok || model != "" {
		encoded, err := json.Marshal(model)
		if err == nil {
			fields["model"] = encoded
		}
	}

	if value, ok := fields["max_completion_tokens"]; ok {
		if _, exists := fields["max_tokens"]; !exists {
			fields["max_tokens"] = value
		}
		delete(fields, "max_completion_tokens")
	}
	limit := int64(maxTokensDefault)
	if maxTokensCap > 0 && (limit <= 0 || limit > maxTokensCap) {
		limit = maxTokensCap
	}
	if _, ok := fields["max_tokens"]; !ok && limit > 0 {
		fields["max_tokens"] = json.RawMessage(fmt.Sprintf("%d", limit))
	} else if ok && maxTokensCap > 0 {
		// Never ask for more than the catalogue says the model can emit.
		var requested int64
		if json.Unmarshal(fields["max_tokens"], &requested) == nil && requested > maxTokensCap {
			fields["max_tokens"] = json.RawMessage(fmt.Sprintf("%d", maxTokensCap))
		}
	}

	if stream {
		fields["stream"] = json.RawMessage("true")
		fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}

	if reasoningEffort != "" {
		// The configured effort is a default, not an override: a client that
		// already controls thinking keeps its own setting.
		_, hasEffort := fields["reasoning_effort"]
		_, hasThinking := fields["thinking"]
		_, hasEnableThinking := fields["enable_thinking"]
		if !hasEffort && !hasThinking && !hasEnableThinking {
			if encoded, err := json.Marshal(reasoningEffort); err == nil {
				fields["reasoning_effort"] = encoded
			}
		}
	}
	if value, ok := fields["reasoning_effort"]; ok {
		fields["reasoning_effort"] = normalizeReasoningEffort(model, value)
	}

	if messages, ok := fields["messages"]; ok {
		normalized, changed := normalizeMessages(messages)
		if changed {
			fields["messages"] = normalized
		}
	}

	return marshalOrdered(fields, chatFieldOrder), dropped, nil
}

// normalizeReasoningEffort keeps the value within what the model family accepts.
// GLM and Kimi only understand low/medium/high; "off"/"none"/"minimal" would be
// rejected outright.
func normalizeReasoningEffort(model string, raw json.RawMessage) json.RawMessage {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "off", "none", "disabled", "minimal":
		normalized = "low"
	case "low", "medium", "high":
	default:
		return raw
	}
	lowered := strings.ToLower(model)
	if strings.Contains(lowered, "glm") || strings.Contains(lowered, "kimi") {
		if normalized == "medium" {
			normalized = "high"
		}
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return raw
	}
	return encoded
}

// normalizeMessages maps roles onto the set the gateway supports and rewrites
// legacy function-role messages.
func normalizeMessages(raw json.RawMessage) (json.RawMessage, bool) {
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return raw, false
	}
	changed := false
	for _, item := range items {
		var role string
		_ = json.Unmarshal(item["role"], &role)
		role = strings.ToLower(strings.TrimSpace(role))
		mapped, ok := supportedRoles[role]
		if !ok {
			switch role {
			case "function":
				var toolCallID string
				_ = json.Unmarshal(item["tool_call_id"], &toolCallID)
				if strings.TrimSpace(toolCallID) != "" {
					mapped = "tool"
				} else {
					mapped = "user"
				}
			default:
				mapped = "user"
			}
			encoded, err := json.Marshal(mapped)
			if err == nil {
				item["role"] = encoded
				changed = true
			}
		} else if mapped != role {
			encoded, err := json.Marshal(mapped)
			if err == nil {
				item["role"] = encoded
				changed = true
			}
		}
	}
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

// marshalOrdered emits the fields in the preferred order first and the rest
// alphabetically, which keeps the body byte-comparable between runs.
func marshalOrdered(fields map[string]json.RawMessage, order []string) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	written := make(map[string]bool, len(fields))
	first := true
	write := func(key string) {
		value, ok := fields[key]
		if !ok || written[key] {
			return
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		encodedKey, _ := json.Marshal(key)
		buf.Write(encodedKey)
		buf.WriteByte(':')
		buf.Write(bytes.TrimSpace(value))
		written[key] = true
	}
	for _, key := range order {
		write(key)
	}
	for _, key := range sortedKeys(fields) {
		write(key)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}
