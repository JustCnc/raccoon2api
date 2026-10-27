package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Credits come from the account's own points API (what the desktop client shows
// as 积分), so a check costs nothing and reports real numbers:
//
//	GET /api/web/points/v1/balance   {"available_points":6210,"daily_points":210,...}
//	GET /api/web/points/v2/bills     paging.limit / type / cursor
//
// The one-token completion stays as a fallback probe: if the points API ever
// disappears, "can this account still spend" is still answerable.
const (
	quotaOK        = "ok"
	quotaExhausted = "exhausted"
	quotaUnknown   = "unknown"

	quotaProbePrompt  = "1"
	pointsBalancePath = "/api/web/points/v1/balance"
	pointsBillsPath   = "/api/web/points/v2/bills"
)

// pointsBalance is the shape of /api/web/points/v1/balance.
type pointsBalance struct {
	AvailablePoints       int64 `json:"available_points"`
	DailyPoints           int64 `json:"daily_points"`
	MonthlyPoints         int64 `json:"monthly_points"`
	RewardPoints          int64 `json:"reward_points"`
	TopupPoints           int64 `json:"topup_points"`
	EnterpriseTopupPoints int64 `json:"enterprise_topup_points"`
	TotalChargeablePoints int64 `json:"total_chargeable_points"`
	EnterpriseReserved    bool  `json:"enterprise_reserved"`
	TopupFrozen           bool  `json:"topup_frozen"`
}

// authOrigin strips the auth path off AuthBaseURL: the points endpoints hang off
// the site root, not off /api/web/auth/v1.
func authOrigin(cfg Config) string {
	if parsed, err := url.Parse(cfg.AuthBaseURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return cfg.AuthBaseURL
}

// fetchPointsBalance reads the credits of one account.
func (a *app) fetchPointsBalance(ctx context.Context, account *credential) (*pointsBalance, error) {
	cfg := a.currentConfig()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authOrigin(cfg)+pointsBalancePath, nil)
	if err != nil {
		return nil, err
	}
	req.Header = a.upstreamHeaders(cfg, account, conversation{}, false)
	req.Header.Set("Content-Type", "")

	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("积分查询失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 1<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("积分查询被拒（HTTP %d），凭据可能已失效", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("积分查询失败：HTTP %d %s", resp.StatusCode, truncateRunes(string(raw), 200))
	}
	var envelope struct {
		Code    json.Number   `json:"code"`
		Message string        `json:"message"`
		Data    pointsBalance `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("积分响应不是 JSON：%w", err)
	}
	switch envelope.Code.String() {
	case "", "0", "200":
	default:
		return nil, fmt.Errorf("积分查询失败：code=%s %s", envelope.Code.String(), envelope.Message)
	}
	balance := envelope.Data
	return &balance, nil
}

// pointsBill is one row of GET /api/web/points/v2/bills. Desktop/electron
// turns use biz_id = "<sessionId>:<turnId>", which is exactly the pair the
// proxy sends as X-RACCOON-Session-ID / X-RACCOON-Turn-ID.
type pointsBill struct {
	BizID      string `json:"biz_id"`
	BizType    string `json:"biz_type"`
	ClientType string `json:"client_type"`
	EventName  string `json:"event_name"`
	Points     int64  `json:"points"`
	CreatedAt  string `json:"created_at"`
}

type pointsBillsPage struct {
	Items      []pointsBill `json:"items"`
	NextCursor string       `json:"next_cursor"`
	HasMore    bool         `json:"has_more"`
}

// fetchPointsBills reads the account's billing history. It is a read-only GET
// and does not consume credits.
func (a *app) fetchPointsBills(ctx context.Context, account *credential, limit int) ([]pointsBill, error) {
	if account == nil || account.AccessToken == "" {
		return nil, fmt.Errorf("账号没有 access_token")
	}
	if limit <= 0 {
		limit = 50
	}
	cfg := a.currentConfig()
	endpoint := authOrigin(cfg) + pointsBillsPath + "?paging.limit=" + fmt.Sprintf("%d", limit) + "&type=all"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header = a.upstreamHeaders(cfg, account, conversation{}, false)
	req.Header.Set("Content-Type", "")

	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("积分账单查询失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 4<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("积分账单查询被拒（HTTP %d），凭据可能已失效", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("积分账单查询失败：HTTP %d %s", resp.StatusCode, truncateRunes(string(raw), 200))
	}
	var envelope struct {
		Code    json.Number     `json:"code"`
		Message string          `json:"message"`
		Data    pointsBillsPage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("积分账单响应不是 JSON：%w", err)
	}
	switch envelope.Code.String() {
	case "", "0", "200":
	default:
		return nil, fmt.Errorf("积分账单查询失败：code=%s %s", envelope.Code.String(), envelope.Message)
	}
	return envelope.Data.Items, nil
}

// billMatchesTurn matches the stable turn suffix. Desktop bills encode the turn
// as "<session>:<turn>"; accepting a plain suffix also tolerates a server-side
// session prefix different from the one this proxy derived.
func billMatchesTurn(bizID, sessionID, turnID string) bool {
	bizID = strings.TrimSpace(bizID)
	sessionID = strings.TrimSpace(sessionID)
	turnID = strings.TrimSpace(turnID)
	if bizID == "" || turnID == "" {
		return false
	}
	if sessionID != "" && bizID == sessionID+":"+turnID {
		return true
	}
	return strings.HasSuffix(bizID, ":"+turnID)
}

// pointsForTurn sums every bill row booked to one turn. Multiple rows are
// normal (input and output are billed separately); daily gifts are not request
// consumption and are ignored even if they somehow share the turn id.
func pointsForTurn(bills []pointsBill, sessionID, turnID string) (int64, bool) {
	if turnID == "" {
		return 0, false
	}
	var total int64
	var found bool
	for _, bill := range bills {
		if bill.BizType == "daily_grant" || bill.BizType == "daily_expire" {
			continue
		}
		if !billMatchesTurn(bill.BizID, sessionID, turnID) {
			continue
		}
		total += bill.Points
		found = true
	}
	return total, found
}

// fetchPointsForTurn retries briefly: the billing row is written asynchronously
// and usually appears a moment after the completion response.
func (a *app) fetchPointsForTurn(ctx context.Context, account *credential, sessionID, turnID string) (int64, bool) {
	for attempt := 0; attempt < 3; attempt++ {
		bills, err := a.fetchPointsBills(ctx, account, 50)
		if err != nil {
			a.log.debug("points", "本次请求积分暂不可用："+err.Error())
			return 0, false
		}
		if points, ok := pointsForTurn(bills, sessionID, turnID); ok {
			return points, true
		}
		if attempt == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return 0, false
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return 0, false
}

// attachRequestPoints fills the request-log row once the upstream bill is
// visible. It runs off the request path so a slow points API cannot delay the
// client response.
func (a *app) attachRequestPoints(seq int64, accountID string, conv conversation) {
	if seq == 0 || accountID == "" || conv.TurnID == "" {
		return
	}
	account := a.poolAccount(accountID)
	if account == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	points, ok := a.fetchPointsForTurn(ctx, account, conv.SessionID, conv.TurnID)
	if !ok {
		return
	}
	a.requests.setPoints(seq, points)
	if points < 0 {
		a.log.info("points", fmt.Sprintf("请求 %s 消耗 %d 积分", conv.TurnID, -points))
	} else if points > 0 {
		a.log.info("points", fmt.Sprintf("请求 %s 返还 %d 积分", conv.TurnID, points))
	}
}

// checkQuota prefers the points API and falls back to the spending probe.
func (a *app) checkQuota(ctx context.Context, account *credential) (string, string) {
	if account == nil || account.AccessToken == "" {
		return quotaUnknown, "账号没有 access_token"
	}
	balance, err := a.fetchPointsBalance(ctx, account)
	if err != nil {
		// Keep the probe as the safety net: it answers "can this account spend"
		// even when the points API is unavailable.
		a.log.debug("quota", "积分接口不可用，回退到消费探测："+err.Error())
		return a.probeQuota(ctx, account)
	}
	state, note := quotaOK, ""
	switch {
	case balance.TotalChargeablePoints <= 0 && balance.AvailablePoints <= 0:
		state = quotaExhausted
		note = "积分已用尽"
		if balance.TopupFrozen {
			note = "积分已用尽（充值额度被冻结）"
		}
	case balance.TopupFrozen:
		state = quotaUnknown
		note = "充值额度被冻结"
	}
	a.recordQuotaPoints(account.ID, state, note, balance)
	return state, note
}

// probeQuota spends one token through one account and records the outcome. It
// deliberately bypasses the rotating executor: a probe must not touch, cool or
// blame any other account.
func (a *app) probeQuota(ctx context.Context, account *credential) (string, string) {
	if account == nil || account.AccessToken == "" {
		return quotaUnknown, "账号没有 access_token"
	}
	cfg := a.currentConfig()
	model := a.quotaProbeModel()
	body := quotaProbePayload(model)

	conv := conversationFrom(account, nil, nil)
	resp, errSend := a.send(ctx, cfg, model, body, conv, account, false)
	if errSend != nil {
		state, note := quotaUnknown, "连接上游失败："+errSend.Error()
		a.recordQuota(account.ID, state, note)
		return state, note
	}
	payload, _ := readLimited(resp.Body, 1<<20)
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		a.recordQuota(account.ID, quotaOK, "")
		return quotaOK, ""
	}
	failure := classifyFailure(resp.StatusCode, payload)
	state, note := quotaUnknown, firstNonEmpty(failure.Message, "探测失败")
	if failure.Status == http.StatusPaymentRequired || failure.Quota || failure.Code == "1000007" {
		state, note = quotaExhausted, firstNonEmpty(failure.Message, "额度不足")
	}
	a.recordQuota(account.ID, state, note)
	return state, note
}

// quotaProbeModel picks the fastest model the catalogue exposes: the probe
// waits for a full completion, and the flash tier answers in a second or two.
func (a *app) quotaProbeModel() string {
	// Bare ids: the probe goes straight to the gateway, which does not know the
	// raccoon/ namespace this plugin adds for CPA.
	snapshot := a.snapshotCatalog()
	for _, hint := range []string{"flash", "lite", "turbo", "mini"} {
		for _, model := range snapshot.Models {
			if strings.Contains(strings.ToLower(model.ID), hint) {
				return model.ID
			}
		}
	}
	if len(snapshot.Models) > 0 {
		return snapshot.Models[0].ID
	}
	if snapshot.Default != "" {
		return snapshot.Default
	}
	return defaultProbeModel
}

const defaultProbeModel = "sn-glm-5-3-flash"

// quotaProbePayload builds the smallest request the gateway accepts.
func quotaProbePayload(model string) []byte {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": quotaProbePrompt},
		},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"1"}],"max_tokens":1}`)
	}
	return body
}

// recordQuotaPoints stores a points reading together with the state.
func (a *app) recordQuotaPoints(id, state, note string, balance *pointsBalance) {
	a.recordQuota(id, state, note)
	if balance == nil {
		return
	}
	a.credMu.Lock()
	account, ok := a.accounts[id]
	if ok {
		account.PointsAvailable = balance.AvailablePoints
		account.PointsDaily = balance.DailyPoints
		account.PointsReward = balance.RewardPoints
		account.PointsTopup = balance.TopupPoints + balance.EnterpriseTopupPoints
		account.PointsChargeable = balance.TotalChargeablePoints
	}
	a.credMu.Unlock()
	if ok {
		a.savePool()
	}
}

// recordQuota stores the probe result and lets a dead quota cool the account
// down so the scheduler stops picking it.
func (a *app) recordQuota(id, state, note string) {
	now := time.Now()
	a.credMu.Lock()
	account, ok := a.accounts[id]
	if ok {
		account.QuotaState = state
		account.QuotaCheckedAt = now.UnixMilli()
		account.QuotaNote = note
		if state == quotaExhausted {
			account.CooldownUntil = now.Add(a.currentConfig().quotaCooldown()).UnixMilli()
			account.LastError = firstNonEmpty(note, "额度不足")
		}
	}
	a.credMu.Unlock()
	if ok {
		a.savePool()
	}
}

// probeQuotaAll probes the given accounts (all when ids is empty).
func (a *app) probeQuotaAll(ctx context.Context, ids []string) map[string]any {
	targets := a.poolSnapshot()
	if len(ids) > 0 {
		wanted := make(map[string]bool, len(ids))
		for _, id := range ids {
			wanted[id] = true
		}
		filtered := targets[:0]
		for _, account := range targets {
			if wanted[account.ID] {
				filtered = append(filtered, account)
			}
		}
		targets = filtered
	}
	// Probes are slow (a full completion each), so run a few in parallel.
	type probeResult struct {
		id, alias, state, note string
	}
	out := make([]probeResult, len(targets))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for index, account := range targets {
		wg.Add(1)
		go func(index int, account *credential) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cred, errFresh := a.ensureAccountFresh(ctx, account)
			if errFresh != nil && cred == nil {
				cred = account
			}
			state, note := a.checkQuota(ctx, cred)
			out[index] = probeResult{account.ID, account.Alias, state, note}
		}(index, account)
	}
	wg.Wait()

	results := make([]map[string]any, 0, len(out))
	ok, exhausted, unknown := 0, 0, 0
	for _, item := range out {
		switch item.state {
		case quotaOK:
			ok++
		case quotaExhausted:
			exhausted++
		default:
			unknown++
		}
		results = append(results, map[string]any{
			"id": item.id, "alias": item.alias, "state": item.state, "note": item.note,
		})
	}
	return map[string]any{
		"ok":        true,
		"probed":    len(results),
		"healthy":   ok,
		"exhausted": exhausted,
		"unknown":   unknown,
		"results":   results,
	}
}

// quotaLoop probes stale accounts on a timer.
func (a *app) quotaLoop(ctx context.Context) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			a.claimDailyPointsAll(ctx, nil)
			interval := a.currentConfig().quotaProbeInterval()
			if interval > 0 {
				a.probeStaleQuotas(ctx, interval)
			}
			if interval <= 0 {
				timer.Reset(time.Minute)
			} else {
				timer.Reset(interval)
			}
		}
	}
}

// probeStaleQuotas probes accounts whose last check is older than interval.
func (a *app) probeStaleQuotas(ctx context.Context, interval time.Duration) {
	cutoff := time.Now().Add(-interval).UnixMilli()
	for _, account := range a.poolSnapshot() {
		if account.Disabled || account.AccessToken == "" {
			continue
		}
		if account.QuotaCheckedAt >= cutoff {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		state, note := a.checkQuota(probeCtx, account)
		cancel()
		if state == quotaExhausted {
			a.log.warn("quota", "账号 "+account.Alias+" 额度不足："+strings.TrimSpace(note))
		}
	}
}

// quotaSummary counts quota states across the pool.
func (a *app) quotaSummary() map[string]any {
	ok, exhausted, unknown := 0, 0, 0
	var lastCheck, available, chargeable int64
	for _, account := range a.poolSnapshot() {
		available += account.PointsAvailable
		chargeable += account.PointsChargeable
		switch account.QuotaState {
		case quotaOK:
			ok++
		case quotaExhausted:
			exhausted++
		default:
			unknown++
		}
		if account.QuotaCheckedAt > lastCheck {
			lastCheck = account.QuotaCheckedAt
		}
	}
	summary := map[string]any{
		"healthy": ok, "exhausted": exhausted, "unknown": unknown,
		"points_available": available, "points_chargeable": chargeable,
	}
	if lastCheck > 0 {
		summary["checked_at"] = time.UnixMilli(lastCheck).UTC().Format(time.RFC3339)
	}
	return summary
}
