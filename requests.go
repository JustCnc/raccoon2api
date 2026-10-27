package main

import (
	"encoding/json"
	"sync"
	"time"
)

// requestEntry is one completed call, reduced to what an operator actually looks
// at when something feels slow or expensive.
type requestEntry struct {
	// Seq is the in-memory key the console uses to attach points after the
	// upstream billing record becomes visible; it is never serialized.
	Seq              int64   `json:"-"`
	At               int64   `json:"at"` // unix milliseconds, request start
	ID               string  `json:"id,omitempty"`
	Account          string  `json:"account,omitempty"`
	AccountID        string  `json:"account_id,omitempty"`
	SessionID        string  `json:"session_id,omitempty"`
	TurnID           string  `json:"turn_id,omitempty"`
	Model            string  `json:"model"`
	UpstreamModel    string  `json:"upstream_model,omitempty"`
	Reasoning        string  `json:"reasoning,omitempty"`
	Stream           bool    `json:"stream"`
	Status           int     `json:"status"`
	TTFTms           int64   `json:"ttft_ms"`
	TotalMs          int64   `json:"total_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CacheRate        float64 `json:"cache_rate"`
	// Points is the signed bill total for this turn: negative means the request
	// spent credits, positive means a refund or grant was booked to the turn.
	Points *int64 `json:"points,omitempty"`
	Error  string `json:"error,omitempty"`
}

// usageInfo mirrors the OpenAI usage block, including the cache and reasoning
// details the gateway reports.
type usageInfo struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// applyUsage copies the counters into an entry and derives the cache hit rate.
func (e *requestEntry) applyUsage(usage *usageInfo) {
	if usage == nil {
		return
	}
	e.PromptTokens = usage.PromptTokens
	e.CompletionTokens = usage.CompletionTokens
	e.TotalTokens = usage.TotalTokens
	e.CachedTokens = usage.PromptTokensDetails.CachedTokens
	e.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	if e.PromptTokens > 0 {
		e.CacheRate = float64(e.CachedTokens) / float64(e.PromptTokens)
	}
}

// parseUsage reads the usage block out of a completion or a stream chunk.
func parseUsage(payload []byte) *usageInfo {
	if len(payload) == 0 {
		return nil
	}
	var probe struct {
		Usage *usageInfo `json:"usage"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return nil
	}
	return probe.Usage
}

// reasoningEffortOf reads the effort that actually went upstream, so the log
// shows the effective value rather than what the client asked for.
func reasoningEffortOf(body []byte) string {
	var probe struct {
		ReasoningEffort json.RawMessage `json:"reasoning_effort"`
		Thinking        json.RawMessage `json:"thinking"`
		EnableThinking  json.RawMessage `json:"enable_thinking"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	if len(probe.ReasoningEffort) > 0 {
		var effort string
		if json.Unmarshal(probe.ReasoningEffort, &effort) == nil {
			return effort
		}
	}
	if raw := firstNonEmpty(string(probe.Thinking), string(probe.EnableThinking)); raw != "" {
		var enabled bool
		if json.Unmarshal([]byte(raw), &enabled) == nil {
			if enabled {
				return "on"
			}
			return "off"
		}
	}
	return ""
}

// requestLog keeps the most recent calls in memory, newest first.
type requestLog struct {
	mu       sync.Mutex
	entries  []requestEntry
	capacity int
	nextSeq  int64
}

func newRequestLog(capacity int) *requestLog {
	if capacity <= 0 {
		capacity = 200
	}
	return &requestLog{capacity: capacity}
}

func (l *requestLog) add(entry requestEntry) int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextSeq++
	entry.Seq = l.nextSeq
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.capacity {
		l.entries = append([]requestEntry(nil), l.entries[len(l.entries)-l.capacity:]...)
	}
	return entry.Seq
}

// setPoints attaches the bill total to an already-recorded request. If the
// entry has already fallen out of the ring, the update is a no-op.
func (l *requestLog) setPoints(seq int64, points int64) {
	if l == nil || seq == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for index := range l.entries {
		if l.entries[index].Seq == seq {
			value := points
			l.entries[index].Points = &value
			return
		}
	}
}

// tail returns the newest entries first.
func (l *requestLog) tail(limit int) []requestEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.entries) {
		limit = len(l.entries)
	}
	out := make([]requestEntry, 0, limit)
	for index := len(l.entries) - 1; index >= 0 && len(out) < limit; index-- {
		out = append(out, l.entries[index])
	}
	return out
}

// summary aggregates the visible window: volume, latency and cache behaviour.
func (l *requestLog) summary() map[string]any {
	entries := l.tail(0)
	summary := map[string]any{"total": len(entries)}
	if len(entries) == 0 {
		return summary
	}
	var ttftSum, totalSum, prompt, cached, failures, streams int64
	var points int64
	var pointsKnown bool
	var ttftCount int64
	for _, entry := range entries {
		if entry.TTFTms > 0 {
			ttftSum += entry.TTFTms
			ttftCount++
		}
		totalSum += entry.TotalMs
		prompt += entry.PromptTokens
		cached += entry.CachedTokens
		if entry.Points != nil {
			points += *entry.Points
			pointsKnown = true
		}
		if entry.Status >= 400 {
			failures++
		}
		if entry.Stream {
			streams++
		}
	}
	if ttftCount > 0 {
		summary["avg_ttft_ms"] = ttftSum / ttftCount
	}
	summary["avg_total_ms"] = totalSum / int64(len(entries))
	summary["prompt_tokens"] = prompt
	summary["cached_tokens"] = cached
	if prompt > 0 {
		summary["cache_rate"] = float64(cached) / float64(prompt)
	}
	summary["failures"] = failures
	summary["streams"] = streams
	if pointsKnown {
		summary["points"] = points
	}
	return summary
}

// recordRequest stores an entry built by the executor and returns the in-memory
// sequence used to attach the bill total once upstream exposes it.
func (a *app) recordRequest(entry requestEntry) int64 {
	if entry.At == 0 {
		entry.At = time.Now().UnixMilli()
	}
	if entry.Model != "" {
		entry.Model = withModelPrefix(entry.Model)
	}
	return a.requests.add(entry)
}

// elapsedMs reports a duration in milliseconds, never rounding a real call down
// to zero: a sub-millisecond answer is still an answer, and the log uses 0 to
// mean "not measured".
func elapsedMs(d time.Duration) int64 {
	if ms := d.Milliseconds(); ms > 0 {
		return ms
	}
	if d > 0 {
		return 1
	}
	return 0
}

// accountAlias resolves an account id to the alias shown in the console.
func (a *app) accountAlias(id string) string {
	if id == "" {
		return ""
	}
	if account := a.poolAccount(id); account != nil {
		return account.Alias
	}
	return id
}
