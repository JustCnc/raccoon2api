package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// credential is one upstream account. The pool holds many; the field names
// double as the on-disk auth record format so records stay inspectable by hand.
type credential struct {
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token,omitempty"`
	ExpiresAt      int64  `json:"expires_at,omitempty"` // unix milliseconds
	OfficeIdentity string `json:"office_identity,omitempty"`
	Subject        string `json:"subject,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	DeviceID       string `json:"device_id,omitempty"`
	Source         string `json:"source,omitempty"`
	UpdatedAt      int64  `json:"updated_at,omitempty"`

	// Pool bookkeeping. Priority follows CPA's convention: higher wins.
	ID            string `json:"id,omitempty"`
	Alias         string `json:"alias,omitempty"`
	Priority      int    `json:"priority"`
	Disabled      bool   `json:"disabled,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	Dead          bool   `json:"dead,omitempty"`
	LastError     string `json:"last_error,omitempty"`

	// Quota comes from the account's points API (/api/web/points/v1/balance);
	// a one-token completion is only the fallback when that route is gone.
	QuotaState     string `json:"quota_state,omitempty"` // "", "ok", "exhausted", "unknown"
	QuotaCheckedAt int64  `json:"quota_checked_at,omitempty"`
	QuotaNote      string `json:"quota_note,omitempty"`

	// Credits as reported upstream, in 积分.
	PointsAvailable  int64 `json:"points_available,omitempty"`
	PointsDaily      int64 `json:"points_daily,omitempty"`
	PointsReward     int64 `json:"points_reward,omitempty"`
	PointsTopup      int64 `json:"points_topup,omitempty"`
	PointsChargeable int64 `json:"points_chargeable,omitempty"`

	// Daily gift bookkeeping (POST /api/web/desktop/v1/login/points/grant).
	DailyGrantDay    string `json:"daily_grant_day,omitempty"`    // YYYY-MM-DD in +08:00
	DailyGrantAt     int64  `json:"daily_grant_at,omitempty"`     // unix milliseconds
	DailyGrantPoints int64  `json:"daily_grant_points,omitempty"` // credits actually handed out

	// Usage counters, kept per account so the console can show who is working.
	Requests   int64 `json:"requests,omitempty"`
	Failures   int64 `json:"failures,omitempty"`
	LastUsedAt int64 `json:"last_used_at,omitempty"`
}

func (c *credential) clone() *credential {
	if c == nil {
		return nil
	}
	copied := *c
	return &copied
}

func (c *credential) expiresAtTime() time.Time {
	if c == nil || c.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.ExpiresAt)
}

func (c *credential) needsRefresh(margin time.Duration) bool {
	if c == nil || c.AccessToken == "" {
		return true
	}
	if c.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(margin).After(c.expiresAtTime())
}

func (c *credential) expired() bool {
	if c == nil || c.ExpiresAt <= 0 {
		return false
	}
	return time.Now().After(c.expiresAtTime())
}

func (c *credential) cooling(now time.Time) bool {
	return c != nil && c.CooldownUntil > now.UnixMilli()
}

func (c *credential) cooldownRemaining(now time.Time) time.Duration {
	if c == nil || c.CooldownUntil <= 0 {
		return 0
	}
	remaining := time.Until(time.UnixMilli(c.CooldownUntil))
	if remaining < 0 {
		return 0
	}
	return remaining
}

// credentialFile is the on-disk shape of both the plugin's copy and the CPA auth
// record.
type credentialFile struct {
	Type           string `json:"type"`
	ID             string `json:"id,omitempty"`
	Label          string `json:"label,omitempty"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token,omitempty"`
	ExpiresAt      int64  `json:"expires_at,omitempty"`
	OfficeIdentity string `json:"office_identity,omitempty"`
	Subject        string `json:"subject,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	DeviceID       string `json:"device_id,omitempty"`
	Source         string `json:"source,omitempty"`
	// UpdatedAt is only used by the export/import format. CPA decodes the
	// published credential into its own Auth struct, where updated_at is a
	// time.Time, and an integer there fails the whole file with
	// "Time.UnmarshalJSON: input is not a JSON string" — so publishAccount and
	// authRecord leave it zero (omitempty drops it).
	UpdatedAt int64  `json:"updated_at,omitempty"`
	Priority  int    `json:"priority,omitempty"`
	Alias     string `json:"alias,omitempty"`

	QuotaState     string `json:"quota_state,omitempty"`
	QuotaCheckedAt int64  `json:"quota_checked_at,omitempty"`
	QuotaNote      string `json:"quota_note,omitempty"`
	Requests       int64  `json:"requests,omitempty"`
	Failures       int64  `json:"failures,omitempty"`
	LastUsedAt     int64  `json:"last_used_at,omitempty"`
}

// credentialInput also accepts the desktop client's own field names and a
// one-time authorization code.
type credentialInput struct {
	credentialFile
	Token             string `json:"token"`
	AuthToken         string `json:"auth_token"`
	AuthorizationCode string `json:"authorization_code"`
	Code              string `json:"code"`
}

// decodeCredential accepts the plugin's own record shape, the desktop file
// shape, and anything that merely carries a bearer token.
func decodeCredential(raw []byte) (*credential, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("empty credential")
	}
	var file credentialInput
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode credential: %w", err)
	}
	cred := &credential{
		AccessToken:    firstNonEmpty(file.AccessToken, file.Token, file.AuthToken),
		RefreshToken:   file.RefreshToken,
		ExpiresAt:      file.ExpiresAt,
		OfficeIdentity: file.OfficeIdentity,
		Subject:        file.Subject,
		SessionID:      file.SessionID,
		DeviceID:       file.DeviceID,
		Source:         file.Source,
		UpdatedAt:      file.UpdatedAt,
		ID:             file.ID,
		Alias:          file.Alias,
		Priority:       file.Priority,
		QuotaState:     file.QuotaState,
		QuotaCheckedAt: file.QuotaCheckedAt,
		QuotaNote:      file.QuotaNote,
		Requests:       file.Requests,
		Failures:       file.Failures,
		LastUsedAt:     file.LastUsedAt,
	}
	cred.normalize()
	return cred, nil
}

// normalize fills the derived fields that can be recovered from the token.
func (c *credential) normalize() {
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.OfficeIdentity = strings.TrimSpace(c.OfficeIdentity)
	c.ID = strings.TrimSpace(c.ID)
	c.Alias = strings.TrimSpace(c.Alias)
	if exp, sub, sid, ok := jwtClaims(c.AccessToken); ok {
		if c.ExpiresAt <= 0 {
			c.ExpiresAt = exp * 1000
		}
		if c.Subject == "" {
			c.Subject = sub
		}
		if c.SessionID == "" {
			c.SessionID = sid
		}
	}
}

// jwtClaims extracts exp (unix seconds), sub and sid. The gateway issues HS256
// tokens; only the payload is inspected, never verified.
func jwtClaims(token string) (exp int64, sub, sid string, ok bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return 0, "", "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return 0, "", "", false
		}
	}
	var claims struct {
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
		Sid string `json:"sid"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return 0, "", "", false
	}
	return claims.Exp, claims.Sub, claims.Sid, true
}

// --- pool ------------------------------------------------------------------

func (a *app) primaryID() string {
	return a.currentConfig().credentialID()
}

// poolSnapshot returns clones of every account, ordered by priority then id.
func (a *app) poolSnapshot() []*credential {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	out := make([]*credential, 0, len(a.accounts))
	for _, account := range a.accounts {
		out = append(out, account.clone())
	}
	sortAccounts(out)
	return out
}

// sortAccounts orders the pool the way CPA orders credentials: higher priority
// first, then by id for a stable tie-break.
func sortAccounts(accounts []*credential) {
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Priority != accounts[j].Priority {
			return accounts[i].Priority > accounts[j].Priority
		}
		return accounts[i].ID < accounts[j].ID
	})
}

func (a *app) poolAccount(id string) *credential {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	if account, ok := a.accounts[id]; ok {
		return account.clone()
	}
	return nil
}

func (a *app) poolSize() int {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	return len(a.accounts)
}

// upsertAccount merges an account into the pool. Runtime state (cooldown,
// priority, disabled) survives a token refresh; a new token clears a dead flag.
func (a *app) upsertAccount(next *credential) bool {
	if next == nil || strings.TrimSpace(next.AccessToken) == "" {
		return false
	}
	if next.ID == "" {
		next.ID = accountIDForAlias(firstNonEmpty(next.Alias, a.currentConfig().CredentialAlias))
	}
	if next.Alias == "" {
		next.Alias = aliasFromID(next.ID)
	}
	next.normalize()

	a.credMu.Lock()
	if a.accounts == nil {
		a.accounts = map[string]*credential{}
	}
	current := a.accounts[next.ID]
	changed := false
	if current == nil {
		changed = true
	} else {
		if current.AccessToken != next.AccessToken {
			changed = true
		}
		// Preserve runtime state the caller may not know about.
		if next.CooldownUntil == 0 {
			next.CooldownUntil = current.CooldownUntil
		}
		if next.Priority == 0 {
			next.Priority = current.Priority
		}
		if !next.Disabled {
			next.Disabled = current.Disabled
		}
		if !next.Dead {
			next.Dead = current.Dead
		}
		if current.AccessToken == next.AccessToken {
			// Same credential: keep the quota probe and usage counters, which a
			// host round-trip or a rescan cannot know about.
			if next.QuotaState == "" {
				next.QuotaState = current.QuotaState
			}
			if next.QuotaCheckedAt == 0 {
				next.QuotaCheckedAt = current.QuotaCheckedAt
			}
			if next.QuotaNote == "" {
				next.QuotaNote = current.QuotaNote
			}
			if next.PointsChargeable == 0 {
				next.PointsAvailable = current.PointsAvailable
				next.PointsDaily = current.PointsDaily
				next.PointsReward = current.PointsReward
				next.PointsTopup = current.PointsTopup
				next.PointsChargeable = current.PointsChargeable
			}
			if next.Requests == 0 {
				next.Requests = current.Requests
			}
			if next.Failures == 0 {
				next.Failures = current.Failures
			}
			if next.LastUsedAt == 0 {
				next.LastUsedAt = current.LastUsedAt
			}
		}
	}
	if current == nil || current.AccessToken != next.AccessToken {
		// A fresh token means the account works again.
		next.Dead = false
		next.CooldownUntil = 0
		next.LastError = ""
	}
	if current != nil && current.AccessToken != next.AccessToken {
		// A rotated token makes the old quota verdict meaningless.
		next.QuotaState = ""
		next.QuotaCheckedAt = 0
		next.QuotaNote = ""
		next.PointsAvailable, next.PointsDaily, next.PointsReward = 0, 0, 0
		next.PointsTopup, next.PointsChargeable = 0, 0
	}
	a.accounts[next.ID] = next
	a.credMu.Unlock()

	if changed || current == nil {
		a.savePool()
	}
	return changed
}

func (a *app) deleteAccount(id string) {
	a.credMu.Lock()
	account, existed := a.accounts[id]
	delete(a.accounts, id)
	a.credMu.Unlock()
	a.mu.Lock()
	delete(a.published, id)
	path := a.publishedPath[id]
	delete(a.publishedPath, id)
	a.mu.Unlock()

	if path != "" {
		// The host has no auth.delete callback; the plugin runs in the host
		// process, so removing the file is what actually drops the credential.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			a.log.warn("accounts", "删除 CPA 凭据文件失败："+err.Error())
		}
	}
	// An account that came from the accounts directory would be re-imported on
	// the next scan, so deleting it has to delete its source file too.
	if account != nil && account.Source == "file" {
		a.removeAccountFile(account)
	}
	if existed {
		a.savePool()
	}
}

// savedAuthPath extracts the file path from a host.auth.save response.
func savedAuthPath(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var resp struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return ""
	}
	return strings.TrimSpace(resp.Path)
}

// setPriority changes an account's scheduling priority and persists it.
// removeAccountFile deletes the accounts-directory file behind an account.
func (a *app) removeAccountFile(account *credential) {
	dir := strings.TrimSpace(a.currentConfig().AccountsDir)
	if dir == "" || account == nil || account.Alias == "" {
		return
	}
	path := filepath.Join(dir, account.Alias+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		a.log.warn("accounts", "删除账号文件失败："+err.Error())
	}
}

func (a *app) setPriority(id string, priority int) {
	a.credMu.Lock()
	account, ok := a.accounts[id]
	if ok {
		account.Priority = priority
		account.UpdatedAt = time.Now().UnixMilli()
	}
	a.credMu.Unlock()
	if ok {
		a.savePool()
	}
}

func (a *app) setCooldown(id string, until time.Time, reason string) {
	a.credMu.Lock()
	account, ok := a.accounts[id]
	if ok {
		account.CooldownUntil = until.UnixMilli()
		account.LastError = reason
		if account.CooldownUntil <= time.Now().UnixMilli() {
			account.CooldownUntil = 0
		}
	}
	a.credMu.Unlock()
	if ok {
		a.savePool()
	}
}

func (a *app) markAccountDead(id, reason string) {
	cooldown := a.currentConfig().cooldown()
	a.credMu.Lock()
	account, ok := a.accounts[id]
	if ok {
		account.Dead = true
		account.LastError = reason
		// Without a cooldown a dead account would keep being scheduled first and
		// burn a refresh token on every request.
		until := time.Now().Add(cooldown).UnixMilli()
		if until > account.CooldownUntil {
			account.CooldownUntil = until
		}
	}
	a.credMu.Unlock()
	if ok {
		a.savePool()
	}
}

func (a *app) resetAccount(id string) bool {
	a.credMu.Lock()
	account, ok := a.accounts[id]
	if ok {
		account.CooldownUntil = 0
		account.Dead = false
		account.Disabled = false
		account.LastError = ""
	}
	a.credMu.Unlock()
	if ok {
		a.savePool()
	}
	return ok
}

// selectAccount picks the next account to try. preferred is the account the host
// scheduled (or empty); excluded holds ids already attempted for this request.
// Accounts in cooldown are skipped unless nothing else is left, in which case the
// one whose cooldown ends first is used rather than failing the request.
func (a *app) selectAccount(preferred string, excluded map[string]bool, now time.Time) *credential {
	accounts := a.poolSnapshot()
	if len(accounts) == 0 {
		return nil
	}
	var usable, cooling, dead []*credential
	for _, account := range accounts {
		if excluded[account.ID] || account.Disabled || account.AccessToken == "" {
			continue
		}
		if account.Dead {
			dead = append(dead, account)
			continue
		}
		if account.cooling(now) {
			cooling = append(cooling, account)
			continue
		}
		usable = append(usable, account)
	}
	if preferred != "" {
		for _, account := range usable {
			if account.ID == preferred {
				return account
			}
		}
	}
	if len(usable) > 0 {
		return usable[0]
	}
	if len(cooling) > 0 {
		sort.SliceStable(cooling, func(i, j int) bool { return cooling[i].CooldownUntil < cooling[j].CooldownUntil })
		return cooling[0]
	}
	if len(dead) > 0 {
		// Nothing healthy is left: retrying a dead account beats failing outright,
		// and it is the only path that can recover a rotated credential.
		return dead[0]
	}
	return nil
}

// --- persistence ------------------------------------------------------------

func (a *app) poolPath() string {
	return filepath.Join(a.currentConfig().DataDir, "accounts.json")
}

type poolFile struct {
	Accounts []*credential `json:"accounts"`
}

func (a *app) loadPool() {
	raw, err := os.ReadFile(a.poolPath())
	if err != nil {
		return
	}
	var file poolFile
	if err := json.Unmarshal(raw, &file); err != nil {
		a.log.warn("accounts", "账号池文件损坏，已备份为 accounts.json.corrupt："+err.Error())
		_ = os.Rename(a.poolPath(), a.poolPath()+".corrupt")
		return
	}
	a.credMu.Lock()
	if a.accounts == nil {
		a.accounts = map[string]*credential{}
	}
	for _, account := range file.Accounts {
		if account == nil || account.ID == "" {
			continue
		}
		account.normalize()
		a.accounts[account.ID] = account
	}
	a.credMu.Unlock()
	if len(file.Accounts) > 0 {
		a.log.info("accounts", fmt.Sprintf("已载入 %d 个账号", len(file.Accounts)))
	}
}

func (a *app) savePool() {
	accounts := a.poolSnapshot()
	raw, err := json.MarshalIndent(poolFile{Accounts: accounts}, "", "  ")
	if err != nil {
		return
	}
	path := a.poolPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// --- publishing -------------------------------------------------------------

// publishAccount mirrors an account into its CPA auth record.
func (a *app) publishAccount(account *credential, force bool) {
	if account == nil || account.AccessToken == "" {
		return
	}
	file := credentialFile{
		Type:           providerKey,
		ID:             account.ID,
		Label:          credentialLabel(account),
		Alias:          account.Alias,
		AccessToken:    account.AccessToken,
		RefreshToken:   account.RefreshToken,
		ExpiresAt:      account.ExpiresAt,
		OfficeIdentity: account.OfficeIdentity,
		Subject:        account.Subject,
		SessionID:      account.SessionID,
		DeviceID:       account.DeviceID,
		Source:         account.Source,
		Priority:       account.Priority,
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return
	}
	digest := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(digest[:])
	a.mu.Lock()
	if a.published == nil {
		a.published = map[string]string{}
	}
	if a.publishedPath == nil {
		a.publishedPath = map[string]string{}
	}
	unchanged := a.published[account.ID] == fingerprint
	a.mu.Unlock()
	if unchanged && !force {
		return
	}
	saved, err := hostCall(methodHostAuthSave, hostAuthSaveRequest{
		Name: account.ID + ".json",
		JSON: json.RawMessage(raw),
	})
	if err != nil {
		a.log.warn("accounts", "写入 CPA 凭据失败："+err.Error())
		return
	}
	a.mu.Lock()
	a.published[account.ID] = fingerprint
	if path := savedAuthPath(saved); path != "" {
		a.publishedPath[account.ID] = path
	}
	a.mu.Unlock()
}

func credentialLabel(account *credential) string {
	if account == nil {
		return "Raccoon"
	}
	if account.Alias != "" {
		return "Raccoon " + account.Alias
	}
	subject := strings.TrimSpace(account.Subject)
	if subject == "" {
		return "Raccoon Desktop"
	}
	if len(subject) > 12 {
		return "Raccoon " + subject[len(subject)-12:]
	}
	return "Raccoon " + subject
}

// authRecord returns the authData the host should build from an account.
func (a *app) authRecord(account *credential, fileName string) authData {
	cfg := a.currentConfig()
	id := account.ID
	if id == "" {
		id = cfg.credentialID()
	}
	if fileName == "" {
		fileName = id + ".json"
	}
	file := credentialFile{
		Type:           providerKey,
		ID:             id,
		Label:          credentialLabel(account),
		Alias:          account.Alias,
		AccessToken:    account.AccessToken,
		RefreshToken:   account.RefreshToken,
		ExpiresAt:      account.ExpiresAt,
		OfficeIdentity: account.OfficeIdentity,
		Subject:        account.Subject,
		SessionID:      account.SessionID,
		DeviceID:       account.DeviceID,
		Source:         account.Source,
		Priority:       account.Priority,
	}
	raw, _ := json.Marshal(file)
	metadata := map[string]any{
		"type":     providerKey,
		"kind":     "raccoon",
		"source":   account.Source,
		"priority": account.Priority,
	}
	if account.RefreshToken != "" {
		metadata["refresh_token"] = account.RefreshToken
	}
	if account.ExpiresAt > 0 {
		metadata["expires_at"] = account.ExpiresAt
	}
	if account.OfficeIdentity != "" {
		metadata["office_identity"] = account.OfficeIdentity
	}
	if account.Subject != "" {
		metadata["subject"] = account.Subject
	}
	return authData{
		Provider:         providerKey,
		ID:               id,
		FileName:         fileName,
		Label:            credentialLabel(account),
		StorageJSON:      raw,
		Metadata:         metadata,
		NextRefreshAfter: a.nextRefreshAfter(account),
	}
}

// nextRefreshAfter is when the host should ask for a refresh: the expiry minus
// the configured margin, never in the past for a token that is still valid.
func (a *app) nextRefreshAfter(account *credential) time.Time {
	if account == nil || account.ExpiresAt <= 0 || account.RefreshToken == "" {
		return time.Time{}
	}
	margin := a.currentConfig().refreshMargin()
	due := account.expiresAtTime().Add(-margin)
	if due.Before(time.Now()) {
		return time.Now().Add(time.Minute)
	}
	return due
}

// --- credential sources -----------------------------------------------------

// credentialFromConfig builds the account described by explicit config values.
func credentialFromConfig(cfg Config) *credential {
	if strings.TrimSpace(cfg.AccessToken) == "" {
		return nil
	}
	account := &credential{
		AccessToken:    strings.TrimSpace(cfg.AccessToken),
		RefreshToken:   strings.TrimSpace(cfg.RefreshToken),
		OfficeIdentity: strings.TrimSpace(cfg.OfficeIdentity),
		Source:         "config",
		UpdatedAt:      time.Now().UnixMilli(),
		ID:             cfg.credentialID(),
		Alias:          cfg.CredentialAlias,
		Priority:       cfg.CredentialPriority,
	}
	account.normalize()
	return account
}

// scanAccountsDir imports every *.json in the configured directory as an
// account. The file name becomes the alias; a file that disappears removes its
// account.
func (a *app) scanAccountsDir(cfg Config) {
	dir := strings.TrimSpace(cfg.AccountsDir)
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		alias := sanitizeAlias(strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
		if alias == "" {
			continue
		}
		id := accountIDForAlias(alias)
		seen[id] = true
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		account, err := decodeCredential(raw)
		if err != nil || account.AccessToken == "" {
			continue
		}
		account.ID = id
		account.Alias = alias
		account.Source = "file"
		account.UpdatedAt = time.Now().UnixMilli()
		if current := a.poolAccount(id); current != nil && current.AccessToken == account.AccessToken {
			account.UpdatedAt = current.UpdatedAt
		}
		if account.Priority == 0 {
			account.Priority = fallbackPriority(cfg.CredentialPriority, 5)
		}
		a.upsertAccount(account)
		a.publishAccount(a.poolAccount(id), false)
	}
	for _, account := range a.poolSnapshot() {
		if account.Source == "file" && !seen[account.ID] {
			a.deleteAccount(account.ID)
			a.log.info("accounts", "账号文件已移除："+account.ID)
		}
	}
}

// syncAccountsFromHost mirrors the CPA auth records of this provider into the
// pool, so records added or deleted in the host UI are honoured.
func (a *app) syncAccountsFromHost() {
	entries, err := a.hostAuths()
	if err != nil {
		a.log.warn("accounts", "读取 CPA 凭据失败："+err.Error())
		return
	}
	if len(entries) == 0 {
		// The host has not indexed this provider yet (fresh start, reload in
		// flight): deleting the pool here would wipe every account.
		return
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		id := accountIDFromHostEntry(entry)
		if id == "" {
			continue
		}
		seen[id] = true
		index := firstNonEmpty(entry.AuthIndex, entry.ID)
		if index == "" {
			continue
		}
		raw, errGet := hostCall(methodHostAuthGet, map[string]string{"auth_index": index})
		if errGet != nil {
			continue
		}
		var resp hostAuthGetResponse
		if json.Unmarshal(raw, &resp) != nil || len(resp.JSON) == 0 {
			continue
		}
		account, errDecode := decodeCredential(resp.JSON)
		if errDecode != nil || account.AccessToken == "" {
			continue
		}
		if account.ID == "" {
			account.ID = id
		}
		seen[account.ID] = true
		seen[id] = true
		if account.Alias == "" {
			account.Alias = aliasFromID(account.ID)
		}
		if account.Source == "" {
			account.Source = "auth-file"
		}
		if account.Priority == 0 {
			account.Priority = fallbackPriority(a.currentConfig().CredentialPriority, 5)
		}
		a.upsertAccount(account)
	}
	// Records the user deleted disappear from the pool; the config source is
	// recreated by its own loop.
	for _, account := range a.poolSnapshot() {
		if seen[account.ID] {
			continue
		}
		if account.Source == "config" {
			continue
		}
		a.deleteAccount(account.ID)
		a.log.info("accounts", "CPA 凭据已删除，账号移出池："+account.ID)
	}
}

// fallbackPriority gives a secondary account a lower priority than the primary
// one, so the known-good account keeps being used first.
func fallbackPriority(base, delta int) int {
	value := base - delta
	if value < 1 {
		value = 1
	}
	return value
}

func accountIDFromHostEntry(entry hostAuthEntry) string {
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		name = strings.TrimSpace(entry.ID)
	}
	if name == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(name), ".json") {
		name = name[:len(name)-len(".json")]
	}
	if !strings.HasPrefix(name, providerKey) {
		name = providerKey + "-" + name
	}
	return name
}

func accountIDForAlias(alias string) string {
	alias = sanitizeAlias(alias)
	if alias == "" {
		alias = "default"
	}
	// Idempotent: a record read back from the host already carries the prefix.
	if strings.HasPrefix(alias, providerKey+"-") {
		return alias
	}
	return providerKey + "-" + alias
}

// aliasFromFileName turns an auth record file name into a pool alias without
// letting the provider prefix accumulate on every reload.
func aliasFromFileName(name string) string {
	base := strings.TrimSuffix(strings.TrimSpace(name), ".json")
	for strings.HasPrefix(base, providerKey+"-") {
		base = strings.TrimPrefix(base, providerKey+"-")
	}
	return sanitizeAlias(base)
}

func aliasFromID(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), providerKey+"-")
}

// --- freshness --------------------------------------------------------------

// anyUsableCredential returns a token for read-only calls (catalogue, tests),
// preferring the primary account and falling back to the first usable one.
func (a *app) anyUsableCredential(ctx context.Context) (*credential, error) {
	cfg := a.currentConfig()
	if account := a.selectAccount(cfg.credentialID(), nil, time.Now()); account != nil {
		return a.ensureAccountFresh(ctx, account)
	}
	if account := a.poolAccount(cfg.credentialID()); account != nil && account.AccessToken != "" {
		return account, nil
	}
	return nil, fmt.Errorf("池子里没有可用账号：请先在控制台添加一个账号")
}

// ensureAccountFresh refreshes an account whose token is about to expire. Each
// account has its own single-flight: the refresh token is single-use, so two
// concurrent refreshes for the same account would invalidate one of them.
func (a *app) ensureAccountFresh(ctx context.Context, account *credential) (*credential, error) {
	if account == nil {
		return nil, fmt.Errorf("没有可用账号")
	}
	cfg := a.currentConfig()
	if !account.needsRefresh(cfg.refreshMargin()) {
		return account, nil
	}
	if account.RefreshToken == "" {
		return account, fmt.Errorf("账号 %s 没有 refresh_token（IAM 令牌无法续期），请重新上传一份新的凭据 JSON", account.Alias)
	}
	next, err := a.refreshAccount(ctx, account)
	if err != nil {
		return account, err
	}
	return next, nil
}

type refreshFlight struct {
	done chan struct{}
	cred *credential
	err  error
}

// refreshAccount performs a single-flight refresh for one account.
func (a *app) refreshAccount(ctx context.Context, account *credential) (*credential, error) {
	if account == nil {
		return nil, fmt.Errorf("没有可刷新的账号")
	}
	if account.RefreshToken == "" {
		return nil, fmt.Errorf("账号 %s 没有 refresh_token", account.Alias)
	}

	a.credMu.Lock()
	// Another caller already replaced this token: refreshing again would spend
	// the single-use refresh token twice.
	if current, ok := a.accounts[account.ID]; ok && current.AccessToken != "" &&
		account.AccessToken != "" && current.AccessToken != account.AccessToken {
		cloned := current.clone()
		a.credMu.Unlock()
		return cloned, nil
	}
	if a.refresh == nil {
		a.refresh = map[string]*refreshFlight{}
	}
	if flight, ok := a.refresh[account.ID]; ok {
		a.credMu.Unlock()
		select {
		case <-flight.done:
			return flight.cred, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &refreshFlight{done: make(chan struct{})}
	a.refresh[account.ID] = flight
	a.credMu.Unlock()

	next, err := func() (*credential, error) {
		// The deferred cleanup below releases waiters even if a panic escapes.
		defer func() {
			a.credMu.Lock()
			if a.refresh != nil && a.refresh[account.ID] == flight {
				delete(a.refresh, account.ID)
			}
			a.credMu.Unlock()
			close(flight.done)
		}()
		refreshed, errRefresh := a.doRefresh(ctx, account)
		if errRefresh == nil && refreshed != nil {
			refreshed.ID = account.ID
			refreshed.Alias = account.Alias
			refreshed.Priority = account.Priority
			refreshed.Source = firstNonEmpty(account.Source, refreshed.Source)
			a.upsertAccount(refreshed)
			a.publishAccount(refreshed, false)
		}
		a.credMu.Lock()
		flight.cred = refreshed
		flight.err = errRefresh
		a.credMu.Unlock()
		return refreshed, errRefresh
	}()

	if err != nil {
		a.log.warn("accounts", fmt.Sprintf("账号 %s 刷新失败：%s", account.Alias, err.Error()))
		return nil, err
	}
	a.log.info("accounts", fmt.Sprintf("账号 %s 已刷新，有效期至 %s", next.Alias, next.expiresAtTime().Local().Format("2006-01-02 15:04:05")))
	return next, nil
}

func (a *app) doRefresh(ctx context.Context, account *credential) (*credential, error) {
	cfg := a.currentConfig()
	body, _ := json.Marshal(map[string]string{"refresh_token": account.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.AuthBaseURL+"/refresh", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 1<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh HTTP %d: %s", resp.StatusCode, truncateRunes(strings.TrimSpace(string(raw)), 200))
	}
	var envelope struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
		Data    struct {
			AccessToken    string `json:"access_token"`
			RefreshToken   string `json:"refresh_token"`
			OfficeIdentity string `json:"office_identity"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("refresh response is not JSON: %w", err)
	}
	if envelope.Code.String() != "" && envelope.Code.String() != "0" && envelope.Code.String() != "200" {
		return nil, fmt.Errorf("refresh rejected: code=%s %s", envelope.Code.String(), envelope.Message)
	}
	if strings.TrimSpace(envelope.Data.AccessToken) == "" {
		return nil, fmt.Errorf("refresh response carried no access_token")
	}
	next := &credential{
		AccessToken:    strings.TrimSpace(envelope.Data.AccessToken),
		RefreshToken:   firstNonEmpty(envelope.Data.RefreshToken, account.RefreshToken),
		OfficeIdentity: firstNonEmpty(envelope.Data.OfficeIdentity, account.OfficeIdentity),
		Subject:        account.Subject,
		DeviceID:       account.DeviceID,
		Source:         account.Source,
		UpdatedAt:      time.Now().UnixMilli(),
	}
	next.normalize()
	return next, nil
}
