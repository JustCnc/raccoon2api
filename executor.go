package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type upstreamFailure struct {
	Status    int
	Code      string
	Message   string
	Retryable bool
	// Quota marks "this account is out of credits": fail fast, never rotate.
	Quota bool
	// RetryAfter is the delay the upstream asked for, when it said so.
	RetryAfter time.Duration
}

func (e *upstreamFailure) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("upstream HTTP %d", e.Status)
}

func (a *app) execute(raw []byte) ([]byte, error) {
	req, err := decodeRequest[rpcExecutorRequest](raw)
	if err != nil {
		return nil, err
	}
	if format := normalizeFormat(req.Format); format != "" && format != formatChat {
		return upstreamError("raccoon2api 只接受 chat-completions 请求", http.StatusBadRequest, false), nil
	}
	model, errModel := a.resolveModel(firstNonEmpty(req.Model, payloadModel(req.Payload)))
	if errModel != nil {
		return upstreamError(errModel.Error(), http.StatusNotFound, false), nil
	}
	cfg := a.currentConfig()
	body, dropped, errNorm := normalizeChatPayload(req.Payload, model, cfg.MaxTokensDefault, a.modelMaxTokens(model), false, cfg.ReasoningEffort)
	if errNorm != nil {
		return upstreamError("请求体无效："+errNorm.Error(), http.StatusBadRequest, false), nil
	}
	if len(dropped) > 0 && cfg.DebugDump {
		a.log.warn("payload", "已丢弃上游不接受的字段："+strings.Join(dropped, ","))
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.requestTimeout())
	defer cancel()

	start := time.Now()
	entry := requestEntry{
		At:            start.UnixMilli(),
		Model:         model,
		UpstreamModel: model,
		Reasoning:     reasoningEffortOf(body),
	}
	headers, payload, accountID, errCall := a.callUpstream(ctx, cfg, model, body, req.Headers, preferredAccountID(req.executorRequest))
	entry.AccountID = accountID
	entry.Account = a.accountAlias(accountID)
	conv := a.conversationForRequest(accountID, req.Headers, body)
	entry.SessionID = conv.SessionID
	entry.TurnID = conv.TurnID
	latency := time.Since(start).Milliseconds()
	if errCall != nil {
		a.metrics.record(false, latency)
		failure := asFailure(errCall)
		entry.Status = failure.Status
		entry.TotalMs = latency
		entry.Error = failure.Message
		a.recordRequest(entry)
		a.log.error("upstream", fmt.Sprintf("model=%s HTTP=%d %s", model, failure.Status, failure.Message))
		return upstreamError(failure.Message, failure.Status, failure.Retryable), nil
	}

	outHeaders := http.Header{"Content-Type": []string{"application/json"}}
	if isEventStream(headers) {
		aggregated, failure := aggregateChatStream(payload)
		if failure != nil {
			a.metrics.record(false, latency)
			entry.Status = failure.Status
			entry.TotalMs = latency
			entry.Error = failure.Message
			a.recordRequest(entry)
			a.log.error("upstream", fmt.Sprintf("model=%s stream error: %s", model, failure.Message))
			return upstreamError(failure.Message, failure.Status, failure.Retryable), nil
		}
		payload = aggregated
	}
	entry.Status = http.StatusOK
	entry.TotalMs = elapsedMs(time.Since(start))
	entry.applyUsage(parseUsage(payload))
	seq := a.recordRequest(entry)
	if cfg.PointsLogging {
		go a.attachRequestPoints(seq, accountID, conv)
	}
	a.metrics.record(true, latency)
	a.log.info("upstream", fmt.Sprintf("model=%s %dms bytes=%d", model, latency, len(payload)))
	if cfg.DebugDump {
		a.log.debug("upstream", "response: "+truncateRunes(string(payload), 800))
	}
	return okEnvelope(executorResponse{Payload: payload, Headers: outHeaders})
}

func (a *app) executeStream(raw []byte) ([]byte, error) {
	req, err := decodeRequest[rpcExecutorRequest](raw)
	if err != nil {
		return nil, err
	}
	if format := normalizeFormat(req.Format); format != "" && format != formatChat {
		return upstreamError("raccoon2api 只接受 chat-completions 请求", http.StatusBadRequest, false), nil
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	model, errModel := a.resolveModel(firstNonEmpty(req.Model, payloadModel(req.Payload)))
	if errModel != nil {
		return upstreamError(errModel.Error(), http.StatusNotFound, false), nil
	}
	cfg := a.currentConfig()
	body, dropped, errNorm := normalizeChatPayload(req.Payload, model, cfg.MaxTokensDefault, a.modelMaxTokens(model), true, cfg.ReasoningEffort)
	if errNorm != nil {
		return upstreamError("请求体无效："+errNorm.Error(), http.StatusBadRequest, false), nil
	}
	if len(dropped) > 0 && cfg.DebugDump {
		a.log.warn("payload", "已丢弃上游不接受的字段："+strings.Join(dropped, ","))
	}

	// Open the upstream stream before answering the host so credential and
	// quota failures still reach the client as a real HTTP status.
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	entry := requestEntry{
		At:            start.UnixMilli(),
		Model:         model,
		UpstreamModel: model,
		Reasoning:     reasoningEffortOf(body),
		Stream:        true,
	}
	resp, reader, accountID, errOpen := a.openStream(ctx, cfg, model, body, req.Headers, preferredAccountID(req.executorRequest))
	entry.AccountID = accountID
	entry.Account = a.accountAlias(accountID)
	conv := a.conversationForRequest(accountID, req.Headers, body)
	entry.SessionID = conv.SessionID
	entry.TurnID = conv.TurnID
	if errOpen != nil {
		cancel()
		failure := asFailure(errOpen)
		a.metrics.record(false, time.Since(start).Milliseconds())
		entry.Status = failure.Status
		entry.TotalMs = elapsedMs(time.Since(start))
		entry.Error = failure.Message
		a.recordRequest(entry)
		a.log.error("stream", fmt.Sprintf("model=%s HTTP=%d %s", model, failure.Status, failure.Message))
		return upstreamError(failure.Message, failure.Status, failure.Retryable), nil
	}

	// The gateway answers a streaming request with SSE. If it ever answers with
	// a plain completion instead, replay it as chunks rather than closing an
	// empty stream (the host turns that into a trailing 502 for the client).
	if !isEventStream(resp.Header) {
		payload, errRead := readLimited(reader, 64<<20)
		reader.Close()
		cancel()
		if errRead != nil {
			return upstreamError("读取上游响应失败："+errRead.Error(), http.StatusBadGateway, true), nil
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			failure := classifyFailure(resp.StatusCode, payload)
			a.metrics.record(false, time.Since(start).Milliseconds())
			return upstreamError(failure.Message, failure.Status, failure.Retryable), nil
		}
		chunks := completionToChunks(payload)
		if len(chunks) == 0 {
			entry.Status = http.StatusBadGateway
			entry.TotalMs = time.Since(start).Milliseconds()
			entry.Error = "上游返回了无法解析的响应体"
			a.recordRequest(entry)
			return upstreamError("上游返回了无法解析的响应体", http.StatusBadGateway, true), nil
		}
		entry.Status = http.StatusOK
		entry.TotalMs = elapsedMs(time.Since(start))
		entry.TTFTms = entry.TotalMs
		entry.applyUsage(parseUsage(payload))
		seq := a.recordRequest(entry)
		if cfg.PointsLogging {
			go a.attachRequestPoints(seq, accountID, conv)
		}
		go func() {
			a.metrics.markStream()
			for _, chunk := range chunks {
				if errEmit := a.emit(streamID, chunk); errEmit != nil {
					a.metrics.record(false, time.Since(start).Milliseconds())
					a.closeStream(streamID, errEmit.Error())
					return
				}
			}
			a.metrics.record(true, time.Since(start).Milliseconds())
			a.closeStream(streamID, "")
		}()
		return okEnvelope(map[string]any{
			"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
		})
	}

	// A silent upstream would otherwise hold the goroutine and the connection
	// until the client gives up: every event resets this watchdog.
	idle := time.AfterFunc(cfg.requestTimeout(), cancel)
	go func() {
		defer cancel()
		defer idle.Stop()
		defer reader.Close()
		a.metrics.markStream()
		stats := streamStats{started: start}
		errPump := a.pumpStream(streamID, model, reader, func() { idle.Reset(cfg.requestTimeout()) }, &stats)
		latency := elapsedMs(time.Since(start))
		entry.TotalMs = latency
		entry.TTFTms = stats.ttftMs
		entry.applyUsage(stats.usage)
		if stats.id != "" {
			entry.ID = stats.id
		}
		if stats.model != "" {
			entry.UpstreamModel = stats.model
		}
		if errPump != nil {
			failure := asFailure(errPump)
			entry.Status = failure.Status
			entry.Error = failure.Message
			a.recordRequest(entry)
			a.metrics.record(false, latency)
			a.log.error("stream", fmt.Sprintf("model=%s %dms %s", model, latency, errPump.Error()))
			a.closeStream(streamID, errPump.Error())
			return
		}
		entry.Status = http.StatusOK
		seq := a.recordRequest(entry)
		if cfg.PointsLogging {
			go a.attachRequestPoints(seq, accountID, conv)
		}
		a.metrics.record(true, latency)
		a.log.info("stream", fmt.Sprintf("model=%s %dms", model, latency))
		a.closeStream(streamID, "")
	}()

	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func (a *app) countTokens(raw []byte) ([]byte, error) {
	req, err := decodeRequest[rpcExecutorRequest](raw)
	if err != nil {
		return nil, err
	}
	estimate := int64(len(req.Payload) / 3)
	if estimate < 1 {
		estimate = 1
	}
	return okEnvelope(map[string]any{"total_tokens": estimate})
}

// preferredAccountID is the account the host scheduled for this request.
func preferredAccountID(req executorRequest) string {
	if id := strings.TrimSpace(req.AuthID); id != "" {
		return id
	}
	if len(req.StorageJSON) == 0 {
		return ""
	}
	account, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(account.ID)
}

// conversationForRequest rebuilds the same conversation identity the upstream
// call used, so the request log can be matched to the bill row afterward.
func (a *app) conversationForRequest(accountID string, headers http.Header, body []byte) conversation {
	if accountID == "" {
		return conversation{}
	}
	account := a.poolAccount(accountID)
	if account == nil {
		return conversation{}
	}
	return conversationFrom(account, headers, mustFields(body))
}

// callUpstream performs a buffered request, rotating accounts on failures that
// another account could plausibly survive. It returns the upstream headers, the
// body, and the id of the account that produced them.
func (a *app) callUpstream(ctx context.Context, cfg Config, model string, body []byte, clientHeaders http.Header, preferred string) (http.Header, []byte, string, error) {
	fields := mustFields(body)
	excluded := map[string]bool{}
	var lastErr error
	refreshed := map[string]bool{}
	netFailures := 0

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		account := a.selectAccount(preferred, excluded, time.Now())
		if account == nil {
			if lastErr != nil {
				return nil, nil, "", lastErr
			}
			return nil, nil, "", &upstreamFailure{Status: http.StatusServiceUnavailable, Message: "没有可用的小浣熊账号（全部冷却或失效）"}
		}
		cred, errFresh := a.ensureAccountFresh(ctx, account)
		if errFresh != nil && cred == nil {
			excluded[account.ID] = true
			lastErr = &upstreamFailure{Status: http.StatusUnauthorized, Message: errFresh.Error()}
			continue
		}
		conv := conversationFrom(cred, clientHeaders, fields)

		resp, errSend := a.send(ctx, cfg, model, body, conv, cred, false)
		if errSend != nil {
			lastErr = errSend
			netFailures++
			if netFailures >= 2 {
				excluded[account.ID] = true
				netFailures = 0
			}
			if attempt < cfg.MaxAttempts && sleepCtx(ctx, backoff(attempt, cfg)) {
				continue
			}
			return nil, nil, "", errSend
		}
		headers := resp.Header.Clone()
		payload, errRead := readLimited(resp.Body, 64<<20)
		resp.Body.Close()
		if errRead != nil {
			lastErr = &upstreamFailure{Status: http.StatusBadGateway, Message: "读取上游响应失败：" + errRead.Error(), Retryable: true}
			excluded[account.ID] = true
			if attempt < cfg.MaxAttempts && sleepCtx(ctx, backoff(attempt, cfg)) {
				continue
			}
			return nil, nil, "", lastErr
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 && !isEventStream(headers) && isErrorEnvelope(payload) {
			// A request the gateway itself rejects will fail on every account.
			return nil, nil, account.ID, classifyFailure(http.StatusBadGateway, payload)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			a.noteAccountSuccess(account.ID)
			return headers, payload, account.ID, nil
		}

		failure := classifyFailure(resp.StatusCode, payload)
		a.noteAccountFailure(account.ID)
		failure.RetryAfter = retryAfter(headers)
		lastErr = failure

		switch {
		case failure.Quota:
			// Out of credits: rotating only burns the rest of the pool.
			a.coolDownAccount(account.ID, cfg.quotaCooldown(), failure.Message)
			return nil, nil, account.ID, failure
		case failure.Status == http.StatusUnauthorized:
			if refreshed[account.ID] {
				a.markAccountDead(account.ID, failure.Message)
				excluded[account.ID] = true
				continue
			}
			refreshed[account.ID] = true
			if next, errRefresh := a.refreshAccount(ctx, cred); errRefresh == nil && next != nil {
				attempt--
				continue
			}
			a.markAccountDead(account.ID, failure.Message)
			excluded[account.ID] = true
			if attempt < cfg.MaxAttempts {
				continue
			}
		case failure.Retryable:
			// Retry-After describes this account's cooldown, not a reason to
			// stall the client: rotate immediately and let the next account try.
			a.coolDownAccount(account.ID, cooldownFor(cfg, failure), failure.Message)
			excluded[account.ID] = true
			if attempt < cfg.MaxAttempts && sleepCtx(ctx, backoff(attempt, cfg)) {
				continue
			}
		}
		return nil, nil, account.ID, failure
	}
	if lastErr == nil {
		lastErr = &upstreamFailure{Status: http.StatusBadGateway, Message: "上游请求失败"}
	}
	return nil, nil, "", lastErr
}

// openStream starts a streaming request, rotating accounts before the first byte
// is delivered.
func (a *app) openStream(ctx context.Context, cfg Config, model string, body []byte, clientHeaders http.Header, preferred string) (*http.Response, io.ReadCloser, string, error) {
	fields := mustFields(body)
	excluded := map[string]bool{}
	var lastErr error
	refreshed := map[string]bool{}
	netFailures := 0

	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		account := a.selectAccount(preferred, excluded, time.Now())
		if account == nil {
			if lastErr != nil {
				return nil, nil, "", lastErr
			}
			return nil, nil, "", &upstreamFailure{Status: http.StatusServiceUnavailable, Message: "没有可用的小浣熊账号（全部冷却或失效）"}
		}
		cred, errFresh := a.ensureAccountFresh(ctx, account)
		if errFresh != nil && cred == nil {
			excluded[account.ID] = true
			lastErr = &upstreamFailure{Status: http.StatusUnauthorized, Message: errFresh.Error()}
			continue
		}
		conv := conversationFrom(cred, clientHeaders, fields)

		resp, errSend := a.send(ctx, cfg, model, body, conv, cred, true)
		if errSend != nil {
			lastErr = errSend
			netFailures++
			if netFailures >= 2 {
				excluded[account.ID] = true
				netFailures = 0
			}
			if attempt < cfg.MaxAttempts && sleepCtx(ctx, backoff(attempt, cfg)) {
				continue
			}
			return nil, nil, "", errSend
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			a.noteAccountSuccess(account.ID)
			return resp, resp.Body, account.ID, nil
		}
		payload, _ := readLimited(resp.Body, 1<<20)
		resp.Body.Close()
		failure := classifyFailure(resp.StatusCode, payload)
		a.noteAccountFailure(account.ID)
		failure.RetryAfter = retryAfter(resp.Header)
		lastErr = failure

		switch {
		case failure.Quota:
			a.coolDownAccount(account.ID, cfg.quotaCooldown(), failure.Message)
			return nil, nil, "", failure
		case failure.Status == http.StatusUnauthorized:
			if refreshed[account.ID] {
				a.markAccountDead(account.ID, failure.Message)
				excluded[account.ID] = true
				continue
			}
			refreshed[account.ID] = true
			if next, errRefresh := a.refreshAccount(ctx, cred); errRefresh == nil && next != nil {
				attempt--
				continue
			}
			a.markAccountDead(account.ID, failure.Message)
			excluded[account.ID] = true
			if attempt < cfg.MaxAttempts {
				continue
			}
		case failure.Retryable:
			// Retry-After describes this account's cooldown, not a reason to
			// stall the client: rotate immediately and let the next account try.
			a.coolDownAccount(account.ID, cooldownFor(cfg, failure), failure.Message)
			excluded[account.ID] = true
			if attempt < cfg.MaxAttempts && sleepCtx(ctx, backoff(attempt, cfg)) {
				continue
			}
		}
		return nil, nil, "", failure
	}
	if lastErr == nil {
		lastErr = &upstreamFailure{Status: http.StatusBadGateway, Message: "上游请求失败"}
	}
	return nil, nil, "", lastErr
}

func (a *app) send(ctx context.Context, cfg Config, model string, body []byte, conv conversation, cred *credential, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header = a.upstreamHeaders(cfg, cred, conv, stream)
	if cfg.DebugDump {
		a.log.debug("upstream", fmt.Sprintf("POST %s headers=%s body=%s", req.URL, dumpHeaders(req.Header), truncateRunes(string(body), 800)))
	}
	return a.httpClient().Do(req)
}

func (a *app) httpClient() *http.Client {
	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	if a.client != nil {
		return a.client
	}
	cfg := a.currentConfig()
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		// A total client timeout would cut long streams; only the wait for
		// response headers is bounded here.
		ResponseHeaderTimeout: cfg.requestTimeout(),
	}
	a.client = &http.Client{Transport: transport}
	return a.client
}

func (a *app) resetHTTPClient() {
	a.clientMu.Lock()
	if a.client != nil {
		if transport, ok := a.client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	a.client = nil
	a.clientMu.Unlock()
}

// pumpStream forwards each upstream event as a bare JSON payload; the host adds
// the "data: " framing and its own [DONE] terminator. touch is called on every
// event so the caller can keep an idle watchdog alive.
// streamStats is what the pump measures while it forwards chunks.
type streamStats struct {
	started time.Time
	ttftMs  int64
	usage   *usageInfo
	id      string
	model   string
}

func (a *app) pumpStream(streamID, model string, body io.ReadCloser, touch func(), stats *streamStats) error {
	reader := newSSEReader(body)
	sawFinish := false
	sawToolCalls := false
	sawContent := false
	var lastID, lastModel string
	var lastCreated int64
	started := time.Now()
	if stats != nil && !stats.started.IsZero() {
		started = stats.started
	}
	markFirstToken := func() {
		if stats != nil && stats.ttftMs == 0 {
			stats.ttftMs = elapsedMs(time.Since(started))
		}
	}

	finish := func(reason, errMsg string) {
		if chunk := finishChunk(lastID, lastModel, lastCreated, model, reason, errMsg); chunk != nil {
			_ = a.emit(streamID, chunk)
		}
	}

	for {
		event, err := reader.next()
		if err == io.EOF {
			if !sawFinish {
				// The host answers 502 when a stream ends without a chunk that
				// carries finish_reason; synthesise one so the client keeps the
				// content it already received.
				reason := "stop"
				if sawToolCalls {
					reason = "tool_calls"
				}
				finish(reason, "")
			}
			return nil
		}
		if err != nil {
			if sawContent {
				// Partial answer: close it politely instead of failing the client
				// after it already rendered text. The error travels in-band.
				reason := "stop"
				if sawToolCalls {
					reason = "tool_calls"
				}
				finish(reason, "上游流中断："+err.Error())
				return nil
			}
			return err
		}
		if len(event) == 0 {
			continue
		}
		if isErrorEnvelope(event) {
			// A business error frame must not be followed by a synthetic finish,
			// which would turn a failed turn into a truncated "success".
			return classifyFailure(http.StatusBadGateway, event)
		}
		if touch != nil {
			touch()
		}
		var probe struct {
			ID      string     `json:"id"`
			Model   string     `json:"model"`
			Created int64      `json:"created"`
			Usage   *usageInfo `json:"usage"`
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
				Delta        struct {
					Content          string            `json:"content"`
					ReasoningContent string            `json:"reasoning_content"`
					Reasoning        string            `json:"reasoning"`
					ToolCalls        []json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(event, &probe) == nil {
			if probe.ID != "" {
				lastID = probe.ID
			}
			if probe.Model != "" {
				lastModel = probe.Model
			}
			if probe.Created != 0 {
				lastCreated = probe.Created
			}
			if probe.Usage != nil && stats != nil {
				stats.usage = probe.Usage
			}
			for _, choice := range probe.Choices {
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					sawFinish = true
				}
				if len(choice.Delta.ToolCalls) > 0 {
					sawToolCalls = true
					sawContent = true
					markFirstToken()
				}
				if choice.Delta.Content != "" || choice.Delta.ReasoningContent != "" || choice.Delta.Reasoning != "" {
					sawContent = true
					markFirstToken()
				}
			}
			if stats != nil && stats.id == "" && lastID != "" {
				stats.id = lastID
				stats.model = lastModel
			}
		}
		if err := a.emit(streamID, event); err != nil {
			return err
		}
	}
}

// finishChunk builds the terminal chunk a well-behaved gateway would have sent.
// errMsg, when set, rides along in-band: the host only treats a frame without
// choices as terminal, so the client sees a normal finish plus the reason.
func finishChunk(id, model string, created int64, fallbackModel, reason, errMsg string) []byte {
	if created == 0 {
		created = time.Now().Unix()
	}
	chunk := map[string]any{
		"id":      firstNonEmpty(id, "chatcmpl-"+randomHex(12)),
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   firstNonEmpty(model, fallbackModel),
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	if errMsg != "" {
		chunk["error"] = map[string]any{"type": "upstream_stream_error", "message": errMsg}
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return encoded
}

// completionToChunks replays a buffered chat.completion as a chunk stream.
func completionToChunks(payload []byte) [][]byte {
	var completion struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Usage   any    `json:"usage"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []any  `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &completion) != nil || len(completion.Choices) == 0 {
		return nil
	}
	if completion.Created == 0 {
		completion.Created = time.Now().Unix()
	}
	build := func(delta map[string]any, finishReason string, usage any) []byte {
		choice := map[string]any{"index": 0, "delta": delta}
		if finishReason != "" {
			choice["finish_reason"] = finishReason
		}
		chunk := map[string]any{
			"id":      completion.ID,
			"object":  "chat.completion.chunk",
			"created": completion.Created,
			"model":   completion.Model,
			"choices": []any{choice},
		}
		if usage != nil {
			chunk["usage"] = usage
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return nil
		}
		return encoded
	}

	choice := completion.Choices[0]
	var chunks [][]byte
	if chunk := build(map[string]any{"role": "assistant"}, "", nil); chunk != nil {
		chunks = append(chunks, chunk)
	}
	if choice.Message.ReasoningContent != "" {
		if chunk := build(map[string]any{"reasoning_content": choice.Message.ReasoningContent}, "", nil); chunk != nil {
			chunks = append(chunks, chunk)
		}
	}
	if choice.Message.Content != "" {
		if chunk := build(map[string]any{"content": choice.Message.Content}, "", nil); chunk != nil {
			chunks = append(chunks, chunk)
		}
	}
	if len(choice.Message.ToolCalls) > 0 {
		if chunk := build(map[string]any{"tool_calls": choice.Message.ToolCalls}, "", nil); chunk != nil {
			chunks = append(chunks, chunk)
		}
	}
	finishReason := choice.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}
	if chunk := build(map[string]any{}, finishReason, completion.Usage); chunk != nil {
		chunks = append(chunks, chunk)
	}
	return chunks
}

func (a *app) emit(streamID string, payload []byte) error {
	_, err := hostCall(methodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

func (a *app) closeStream(streamID, errMsg string) {
	_, _ = hostCall(methodHostStreamClose, streamCloseRequest{StreamID: streamID, Error: errMsg})
}

// classifyFailure turns an upstream error body into a client-facing failure.
func classifyFailure(status int, body []byte) *upstreamFailure {
	message := strings.TrimSpace(string(body))
	code := ""
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Details string          `json:"details"`
		Error   *struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
			Type    string          `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		switch {
		case envelope.Error != nil && envelope.Error.Message != "":
			message = envelope.Error.Message
			code = trimJSONScalar(envelope.Error.Code)
		case envelope.Message != "":
			message = envelope.Message
			code = trimJSONScalar(envelope.Code)
			if envelope.Details != "" && envelope.Details != envelope.Message {
				message = message + "（" + envelope.Details + "）"
			}
		}
	}
	if message == "" {
		message = fmt.Sprintf("上游返回 HTTP %d", status)
	}
	message = truncateRunes(message, 500)

	failure := &upstreamFailure{Status: status, Code: code, Message: message}
	switch {
	case status < 300:
		// A 2xx carrying an error envelope: the gateway does this when the
		// backend call itself failed after the proxy answered.
		failure.Status = http.StatusBadGateway
	case status == http.StatusUnauthorized:
		failure.Status = http.StatusUnauthorized
	case status == http.StatusTooManyRequests:
		failure.Status = http.StatusTooManyRequests
		failure.Retryable = true
	case status >= 500:
		failure.Status = http.StatusBadGateway
		failure.Retryable = status >= 502
	case status == http.StatusRequestTimeout || status == 499:
		failure.Status = http.StatusBadGateway
		failure.Retryable = true
	}
	switch code {
	case "200001", "200002", "200003", "200009":
		failure.Status = http.StatusUnauthorized
		failure.Retryable = false
	case "1000007":
		failure.Status = http.StatusPaymentRequired
		failure.Retryable = false
		failure.Quota = true
	}
	if failure.Status != http.StatusUnauthorized && isQuotaMessage(message) {
		failure.Status = http.StatusPaymentRequired
		failure.Retryable = false
		failure.Quota = true
	}
	return failure
}

// isQuotaMessage recognises "out of credits" wording. The markers are phrases,
// not single words: a 400 about "balance" of some unrelated field must not be
// rewritten into a hard 402.
func isQuotaMessage(message string) bool {
	lowered := strings.ToLower(message)
	for _, marker := range []string{
		"insufficient credit", "insufficient_quota", "quota exceeded", "credit balance",
		"out of credits", "not enough credits",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	for _, marker := range []string{"余额不足", "积分不足", "额度不足", "余额已用完", "积分已用完"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// retryAfter reads the delay the upstream asked for, in any of the spellings the
// gateway family uses.
func retryAfter(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	if raw := strings.TrimSpace(headers.Get("Retry-After-Ms")); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms > 0 {
			return clampDelay(time.Duration(ms) * time.Millisecond)
		}
	}
	if raw := strings.TrimSpace(headers.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
			return clampDelay(time.Duration(seconds) * time.Second)
		}
		if when, err := http.ParseTime(raw); err == nil {
			return clampDelay(time.Until(when))
		}
	}
	if raw := strings.TrimSpace(headers.Get("X-Ratelimit-Reset")); raw != "" {
		if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil && epoch > 0 {
			return clampDelay(time.Until(time.Unix(epoch, 0)))
		}
	}
	return 0
}

func clampDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > 2*time.Hour {
		return 2 * time.Hour
	}
	if d < time.Second {
		return time.Second
	}
	return d
}

func cooldownFor(cfg Config, failure *upstreamFailure) time.Duration {
	if failure != nil && failure.RetryAfter > 0 {
		return failure.RetryAfter
	}
	return cfg.cooldown()
}

func (c Config) cooldown() time.Duration {
	return time.Duration(c.CooldownSeconds) * time.Second
}

func (c Config) quotaCooldown() time.Duration {
	return time.Duration(c.QuotaCooldownSeconds) * time.Second
}

func (a *app) coolDownAccount(id string, d time.Duration, reason string) {
	if id == "" || d <= 0 {
		return
	}
	until := time.Now().Add(d)
	a.setCooldown(id, until, reason)
	account := a.poolAccount(id)
	alias := id
	if account != nil {
		alias = account.Alias
	}
	a.log.warn("accounts", fmt.Sprintf("账号 %s 冷却 %s：%s", alias, d.Round(time.Second), truncateRunes(reason, 120)))
}

// noteAccountSuccess clears a stale error note after a good request and bumps
// the per-account usage counters the console shows.
func (a *app) noteAccountSuccess(id string) {
	now := time.Now().UnixMilli()
	a.credMu.Lock()
	account, ok := a.accounts[id]
	changed := false
	if ok {
		account.Requests++
		account.LastUsedAt = now
		if account.QuotaState != quotaOK {
			// A completed request proves the account can spend.
			account.QuotaState = quotaOK
			account.QuotaNote = ""
			account.QuotaCheckedAt = now
		}
		if account.LastError != "" && !account.cooling(time.Now()) {
			account.LastError = ""
		}
		changed = true
	}
	a.credMu.Unlock()
	if changed {
		a.savePool()
	}
}

// noteAccountFailure counts a failed attempt against one account.
func (a *app) noteAccountFailure(id string) {
	a.credMu.Lock()
	account, ok := a.accounts[id]
	if ok {
		account.Failures++
		account.LastUsedAt = time.Now().UnixMilli()
	}
	a.credMu.Unlock()
	if ok {
		a.savePool()
	}
}

func asFailure(err error) *upstreamFailure {
	if failure, ok := err.(*upstreamFailure); ok {
		return failure
	}
	return &upstreamFailure{Status: http.StatusBadGateway, Message: err.Error(), Retryable: true}
}

func trimJSONScalar(raw json.RawMessage) string {
	value := strings.TrimSpace(string(raw))
	value = strings.Trim(value, `"`)
	return value
}

func payloadModel(payload []byte) string {
	fields, err := decodeChatPayload(payload)
	if err != nil {
		return ""
	}
	var model string
	_ = json.Unmarshal(fields["model"], &model)
	return strings.TrimSpace(model)
}

func mustFields(payload []byte) map[string]json.RawMessage {
	fields, err := decodeChatPayload(payload)
	if err != nil {
		return map[string]json.RawMessage{}
	}
	return fields
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return data, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("上游响应超过 %d 字节上限", limit)
	}
	return data, nil
}

// backoff grows with the attempt and carries jitter so parallel requests do not
// retry in lockstep.
func backoff(attempt int, cfg Config) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := time.Duration(attempt) * 400 * time.Millisecond
	if base > 3*time.Second {
		base = 3 * time.Second
	}
	if !cfg.RetryJitter {
		return base
	}
	jitter := time.Duration(float64(base) * (0.2 * (randFraction()*2 - 1)))
	return base + jitter
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func dumpHeaders(headers http.Header) string {
	parts := make([]string, 0, len(headers))
	for _, key := range sortedKeys(headers) {
		value := strings.Join(headers[key], ",")
		if strings.EqualFold(key, "Authorization") {
			value = "Bearer <redacted>"
		}
		parts = append(parts, key+": "+value)
	}
	return strings.Join(parts, " | ")
}

// isErrorEnvelope reports whether a 2xx body is actually a gateway error: an
// OpenAI-shaped {"error":{...}} object, or a business envelope with a non-zero
// code and no completion payload.
func isErrorEnvelope(payload []byte) bool {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Code    json.RawMessage `json:"code"`
		Choices json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return false
	}
	if len(envelope.Choices) > 0 && string(envelope.Choices) != "null" {
		return false
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return true
	}
	code := trimJSONScalar(envelope.Code)
	return code != "" && code != "0" && code != "200"
}
