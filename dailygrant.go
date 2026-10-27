package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The desktop client tops every account up once a day: opening it fires
//
//	POST /api/web/desktop/v1/login/points/grant
//	headers: X-Client-Platform, X-Client-Version, X-Org-Code (non-personal only)
//	→ {"code":0,"data":{"granted":true,"popup":{"source":"desktopLogin","points":300,…}}}
//
// The grant is idempotent per day — a second call answers granted:false — and the
// points it hands out expire at 23:59:59 the same day (bills: biz_type
// "daily_grant" then "daily_expire"). So the plugin claims them itself instead of
// waiting for somebody to open the desktop app.
const pointsGrantPath = "/api/web/desktop/v1/login/points/grant"

// dailyGrantZone is the upstream day boundary: grant_at is 00:00 and expire_at
// 23:59:59 in +08:00, whatever the host's local zone is.
var dailyGrantZone = time.FixedZone("CST", 8*3600)

// grantDay renders the upstream service day of t.
func grantDay(t time.Time) string { return t.In(dailyGrantZone).Format("2006-01-02") }

type grantPopup struct {
	Source   string `json:"source"`
	Points   int64  `json:"points"`
	ExpireAt string `json:"expire_at"`
}

type grantEnvelope struct {
	Code    json.Number `json:"code"`
	Message string      `json:"message"`
	Data    struct {
		Granted bool        `json:"granted"`
		Popup   *grantPopup `json:"popup"`
	} `json:"data"`
}

// dailyGrantResult is one claim attempt, as the console shows it.
type dailyGrantResult struct {
	ID      string `json:"id"`
	Alias   string `json:"alias,omitempty"`
	Day     string `json:"day"`
	Granted bool   `json:"granted"`
	Points  int64  `json:"points"`
	Skipped bool   `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}

// claimDailyPoints asks upstream for today's gift on one account. The day is
// recorded only when upstream actually answered: a network error or a rejected
// token must be retried on the next tick instead of being written off.
func (a *app) claimDailyPoints(ctx context.Context, account *credential) dailyGrantResult {
	day := grantDay(time.Now())
	result := dailyGrantResult{ID: account.ID, Alias: account.Alias, Day: day}
	if account.AccessToken == "" {
		result.Error = "账号没有 access_token"
		return result
	}

	cfg := a.currentConfig()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authOrigin(cfg)+pointsGrantPath, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	req.Header = a.upstreamHeaders(cfg, account, conversation{}, false)
	req.Header.Set("Content-Type", "")
	// The desktop sends the platform without the architecture suffix here.
	req.Header.Set("x-client-platform", strings.TrimSuffix(cfg.ClientPlatform, "-x64"))

	resp, err := a.httpClient().Do(req)
	if err != nil {
		result.Error = "领取失败：" + err.Error()
		return result
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 1<<20)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		result.Error = fmt.Sprintf("领取被拒（HTTP %d），凭据可能已失效", resp.StatusCode)
		return result
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Error = fmt.Sprintf("领取失败：HTTP %d %s", resp.StatusCode, truncateRunes(string(raw), 200))
		return result
	}
	var envelope grantEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		result.Error = "领取响应不是 JSON：" + err.Error()
		return result
	}
	switch envelope.Code.String() {
	case "", "0", "200":
	default:
		result.Error = fmt.Sprintf("领取失败：code=%s %s", envelope.Code.String(), envelope.Message)
		return result
	}

	result.Granted = envelope.Data.Granted
	if popup := envelope.Data.Popup; popup != nil {
		result.Points = popup.Points
	}

	a.recordDailyGrant(account.ID, day, result.Points, result.Granted)
	if result.Granted {
		a.log.info("points", fmt.Sprintf("账号 %s 领取每日积分 +%d（%s）", account.Alias, result.Points, day))
		// Refresh the balance so the console shows the new daily total right away.
		if balance, err := a.fetchPointsBalance(ctx, account); err == nil {
			a.recordQuotaPoints(account.ID, quotaOK, "", balance)
		}
	} else {
		a.log.debug("points", fmt.Sprintf("账号 %s 今日积分已领取过（%s）", account.Alias, day))
	}
	return result
}

// recordDailyGrant stores the claim on the account so the console can show it and
// the next tick can skip the account until tomorrow.
func (a *app) recordDailyGrant(id, day string, points int64, granted bool) {
	a.credMu.Lock()
	account, ok := a.accounts[id]
	if ok {
		account.DailyGrantDay = day
		account.DailyGrantAt = time.Now().UnixMilli()
		account.DailyGrantPoints = 0
		if granted {
			account.DailyGrantPoints = points
		}
	}
	a.credMu.Unlock()
	if ok {
		a.savePool()
	}
}

// claimDailyPointsAll claims today's points for every account that has not been
// claimed yet, skipping the ones explicitly asked for in skip.
func (a *app) claimDailyPointsAll(ctx context.Context, skip map[string]bool) []dailyGrantResult {
	if !a.currentConfig().DailyGrantEnabled {
		return nil
	}
	day := grantDay(time.Now())
	results := make([]dailyGrantResult, 0, 4)
	for _, account := range a.poolSnapshot() {
		if account.Disabled || account.AccessToken == "" || account.DailyGrantDay == day {
			continue
		}
		if skip[account.ID] {
			continue
		}
		results = append(results, a.claimDailyPoints(ctx, account))
	}
	return results
}

// dailyGrantSummary counts the results for the console toast.
func dailyGrantSummary(results []dailyGrantResult) map[string]any {
	granted, points, failed := 0, int64(0), 0
	for _, result := range results {
		switch {
		case result.Error != "":
			failed++
		case result.Granted:
			granted++
			points += result.Points
		}
	}
	return map[string]any{
		"granted": granted,
		"points":  points,
		"failed":  failed,
		"checked": len(results),
		"results": results,
	}
}
