package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// sseReader yields the JSON payload of each server-sent event. The gateway
// answers a streaming request with plain `data: {...}` frames terminated by
// `data: [DONE]`; nothing else is emitted.
type sseReader struct {
	scanner *bufio.Scanner
	data    []byte
}

func newSSEReader(r io.Reader) *sseReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	return &sseReader{scanner: scanner}
}

// next returns the next event payload, or io.EOF when the stream ends.
func (s *sseReader) next() ([]byte, error) {
	for s.scanner.Scan() {
		line := bytes.TrimRight(s.scanner.Bytes(), "\r")
		if len(line) == 0 {
			if len(s.data) == 0 {
				continue
			}
			payload := s.data
			s.data = nil
			return payload, nil
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimPrefix(line, []byte("data:"))
		value = bytes.TrimPrefix(value, []byte(" "))
		if bytes.Equal(value, []byte("[DONE]")) {
			return nil, io.EOF
		}
		if len(s.data) > 0 {
			s.data = append(s.data, '\n')
		}
		s.data = append(s.data, value...)
	}
	if err := s.scanner.Err(); err != nil {
		return nil, err
	}
	if len(s.data) > 0 {
		payload := s.data
		s.data = nil
		return payload, nil
	}
	return nil, io.EOF
}

func isEventStream(headers http.Header) bool {
	if headers == nil {
		return false
	}
	return strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/event-stream")
}

type toolCallAccumulator struct {
	index     int
	id        string
	name      string
	arguments strings.Builder
}

type chatStreamAccumulator struct {
	id           string
	model        string
	created      int64
	content      strings.Builder
	reasoning    strings.Builder
	finishReason string
	usage        json.RawMessage
	extra        map[string]json.RawMessage
	toolCalls    map[int]*toolCallAccumulator
	order        []int
}

// aggregateChatStream folds a chat-completions SSE stream into one
// chat.completion object. It only runs when the client asked for a single
// response and the gateway streamed anyway. An error frame inside the stream is
// reported as a failure instead of being folded into an empty "success".
func aggregateChatStream(payload []byte) ([]byte, *upstreamFailure) {
	acc := &chatStreamAccumulator{toolCalls: map[int]*toolCallAccumulator{}}
	reader := newSSEReader(bytes.NewReader(payload))
	for {
		event, err := reader.next()
		if err == io.EOF || err != nil {
			break
		}
		if isErrorEnvelope(event) {
			return nil, classifyFailure(http.StatusBadGateway, event)
		}
		var chunk struct {
			ID      string                     `json:"id"`
			Model   string                     `json:"model"`
			Created int64                      `json:"created"`
			Usage   json.RawMessage            `json:"usage"`
			Extra   map[string]json.RawMessage `json:"-"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(event, &chunk) != nil {
			continue
		}
		if chunk.ID != "" {
			acc.id = chunk.ID
		}
		if chunk.Model != "" {
			acc.model = chunk.Model
		}
		if chunk.Created != 0 {
			acc.created = chunk.Created
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			acc.usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			acc.content.WriteString(choice.Delta.Content)
			acc.content.WriteString(choice.Message.Content)
			if choice.Delta.ReasoningContent != "" {
				acc.reasoning.WriteString(choice.Delta.ReasoningContent)
			} else if choice.Delta.Reasoning != "" {
				acc.reasoning.WriteString(choice.Delta.Reasoning)
			}
			if choice.FinishReason != "" {
				acc.finishReason = choice.FinishReason
			}
			for _, call := range choice.Delta.ToolCalls {
				acc.addToolCall(call.Index, call.ID, call.Function.Name, call.Function.Arguments)
			}
		}
	}
	if acc.finishReason == "" && len(acc.order) > 0 {
		acc.finishReason = "tool_calls"
	}
	if acc.finishReason == "" {
		acc.finishReason = "stop"
	}
	return acc.marshal(), nil
}

func (a *chatStreamAccumulator) addToolCall(index int, id, name, arguments string) {
	call, ok := a.toolCalls[index]
	if !ok {
		call = &toolCallAccumulator{index: index}
		a.toolCalls[index] = call
		a.order = append(a.order, index)
	}
	if id != "" {
		call.id = id
	}
	if name != "" {
		call.name += name
	}
	call.arguments.WriteString(arguments)
}

func (a *chatStreamAccumulator) marshal() []byte {
	message := map[string]any{"role": "assistant", "content": a.content.String()}
	if a.reasoning.Len() > 0 {
		message["reasoning_content"] = a.reasoning.String()
	}
	if len(a.order) > 0 {
		calls := make([]any, 0, len(a.order))
		for i, index := range a.order {
			call := a.toolCalls[index]
			id := call.id
			if id == "" {
				id = "chatcmpl-tool-" + randomHex(8)
			}
			calls = append(calls, map[string]any{
				"id":    id,
				"index": i,
				"type":  "function",
				"function": map[string]any{
					"name":      call.name,
					"arguments": call.arguments.String(),
				},
			})
		}
		message["tool_calls"] = calls
	}
	out := map[string]any{
		"id":      firstNonEmpty(a.id, "chatcmpl-"+randomHex(12)),
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": a.finishReason,
		}},
	}
	if a.created == 0 {
		out["created"] = time.Now().Unix()
	}
	if len(a.usage) > 0 {
		out["usage"] = a.usage
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return []byte(`{"object":"chat.completion","choices":[]}`)
	}
	return encoded
}
