package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeJWT(exp int64, sub, sid string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{
		"exp": exp,
		"sub": sub,
		"sid": sid,
		"iss": "xiaohuanxiong.com/iam",
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func newTestApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	a := &app{log: newLogRing(50), metrics: newMetrics(), requests: newRequestLog(300)}
	store, err := newStateStore(dir)
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	a.state = store
	cfg := defaultConfig()
	cfg.DataDir = dir
	if err := cfg.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	a.cfg = cfg
	return a
}

func testAccount(id string, priority int, exp time.Time) *credential {
	return &credential{
		ID:          id,
		Alias:       aliasFromID(id),
		AccessToken: fakeJWT(exp.Unix(), "pu-"+id, "ses-"+id),
		Priority:    priority,
		Source:      "file",
		UpdatedAt:   time.Now().UnixMilli(),
	}
}

func TestDecodeDesktopAuthFile(t *testing.T) {
	exp := time.Now().Add(24 * time.Hour).Unix()
	token := fakeJWT(exp, "pu-123", "ses-abc")
	raw, _ := json.Marshal(map[string]string{
		"access_token":    token,
		"office_identity": "personal",
	})
	account, err := decodeCredential(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if account.AccessToken != token {
		t.Fatal("access token lost")
	}
	if account.Subject != "pu-123" || account.SessionID != "ses-abc" {
		t.Fatalf("claims not derived: %+v", account)
	}
	if account.ExpiresAt != exp*1000 {
		t.Fatalf("expires_at = %d, want %d", account.ExpiresAt, exp*1000)
	}
	if account.RefreshToken != "" {
		t.Fatal("unexpected refresh token")
	}
}

func TestDecodeCredentialAlternateTokenKeys(t *testing.T) {
	for _, key := range []string{"token", "auth_token"} {
		raw, _ := json.Marshal(map[string]string{key: "abc.def.ghi"})
		account, err := decodeCredential(raw)
		if err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
		if account.AccessToken != "abc.def.ghi" {
			t.Fatalf("%s not read: %+v", key, account)
		}
	}
}

func TestUpsertClearsCooldownOnNewToken(t *testing.T) {
	a := newTestApp(t)
	first := testAccount("raccoon2api-a", 10, time.Now().Add(time.Hour))
	a.upsertAccount(first)
	a.setCooldown(first.ID, time.Now().Add(10*time.Minute), "429")
	a.markAccountDead(first.ID, "401")

	updated := testAccount("raccoon2api-a", 10, time.Now().Add(2*time.Hour))
	a.upsertAccount(updated)

	account := a.poolAccount(first.ID)
	if account == nil {
		t.Fatal("account lost")
	}
	if account.CooldownUntil != 0 || account.Dead {
		t.Fatalf("new token must clear runtime state: %+v", account)
	}
	if account.AccessToken != updated.AccessToken {
		t.Fatal("token not replaced")
	}
}

func TestUpsertPreservesPriorityAndCooldown(t *testing.T) {
	a := newTestApp(t)
	first := testAccount("raccoon2api-a", 42, time.Now().Add(time.Hour))
	a.upsertAccount(first)
	until := time.Now().Add(5 * time.Minute)
	a.setCooldown(first.ID, until, "429")

	// Same token, no priority/cooldown fields: runtime state must survive.
	again := testAccount("raccoon2api-a", 0, time.Now().Add(time.Hour))
	again.AccessToken = first.AccessToken
	a.upsertAccount(again)

	account := a.poolAccount(first.ID)
	if account.Priority != 42 {
		t.Fatalf("priority = %d, want 42", account.Priority)
	}
	if !account.cooling(time.Now()) {
		t.Fatalf("cooldown lost: %+v", account)
	}
}

func TestSelectAccountPrefersPriorityThenSkipsCooling(t *testing.T) {
	a := newTestApp(t)
	now := time.Now()
	// CPA semantics: the higher number is used first.
	a.upsertAccount(testAccount("raccoon2api-a", 20, now.Add(time.Hour)))
	a.upsertAccount(testAccount("raccoon2api-b", 30, now.Add(time.Hour)))
	a.upsertAccount(testAccount("raccoon2api-c", 10, now.Add(time.Hour)))

	if got := a.selectAccount("", nil, now); got == nil || got.ID != "raccoon2api-b" {
		t.Fatalf("priority order wrong: %+v", got)
	}
	// The host-scheduled account wins when it is usable.
	if got := a.selectAccount("raccoon2api-c", nil, now); got == nil || got.ID != "raccoon2api-c" {
		t.Fatalf("preferred account not used: %+v", got)
	}
	a.setCooldown("raccoon2api-b", now.Add(time.Minute), "429")
	if got := a.selectAccount("", nil, now); got == nil || got.ID != "raccoon2api-a" {
		t.Fatalf("cooling account not skipped: %+v", got)
	}
	// Exclusions apply.
	if got := a.selectAccount("", map[string]bool{"raccoon2api-a": true}, now); got == nil || got.ID != "raccoon2api-c" {
		t.Fatalf("exclusion ignored: %+v", got)
	}
	// Everything cooling: the earliest to recover is still offered.
	if got := a.selectAccount("", map[string]bool{"raccoon2api-a": true, "raccoon2api-c": true}, now); got == nil || got.ID != "raccoon2api-b" {
		t.Fatalf("all-cooling fallback = %+v", got)
	}
}

func TestSelectAccountSkipsDisabledAndDead(t *testing.T) {
	a := newTestApp(t)
	now := time.Now()
	disabled := testAccount("raccoon2api-a", 10, now.Add(time.Hour))
	disabled.Disabled = true
	a.upsertAccount(disabled)
	dead := testAccount("raccoon2api-b", 20, now.Add(time.Hour))
	a.upsertAccount(dead)
	a.markAccountDead(dead.ID, "401")
	a.upsertAccount(testAccount("raccoon2api-c", 30, now.Add(time.Hour)))

	if got := a.selectAccount("", nil, now); got == nil || got.ID != "raccoon2api-c" {
		t.Fatalf("expected the healthy account, got %+v", got)
	}
}

func TestScanAccountsDirImportsAndRemoves(t *testing.T) {
	a := newTestApp(t)
	dir := t.TempDir()
	cfg := a.currentConfig()
	cfg.AccountsDir = dir
	a.cfg = cfg

	write := func(name, sub string) {
		token := fakeJWT(time.Now().Add(time.Hour).Unix(), sub, "ses-"+sub)
		body, _ := json.Marshal(map[string]string{"access_token": token})
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("work.json", "pu-work")
	write("team-a.json", "pu-team-a")
	a.scanAccountsDir(cfg)

	if got := a.poolSize(); got != 2 {
		t.Fatalf("pool size = %d, want 2", got)
	}
	work := a.poolAccount(accountIDForAlias("work"))
	if work == nil || work.Source != "file" {
		t.Fatalf("work account = %+v", work)
	}

	if err := os.Remove(filepath.Join(dir, "team-a.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	a.scanAccountsDir(cfg)
	if got := a.poolSize(); got != 1 {
		t.Fatalf("pool size after removal = %d, want 1", got)
	}
}

func TestPoolSurvivesRestart(t *testing.T) {
	a := newTestApp(t)
	account := testAccount("raccoon2api-a", 7, time.Now().Add(time.Hour))
	account.RefreshToken = "r1"
	a.upsertAccount(account)
	a.setCooldown(account.ID, time.Now().Add(3*time.Minute), "429")

	b := &app{log: newLogRing(10), metrics: newMetrics()}
	cfg := a.currentConfig()
	b.cfg = cfg
	b.loadPool()

	restored := b.poolAccount(account.ID)
	if restored == nil || restored.AccessToken != account.AccessToken {
		t.Fatalf("restored = %+v", restored)
	}
	if restored.Priority != 7 {
		t.Fatalf("priority = %d", restored.Priority)
	}
	if !restored.cooling(time.Now()) {
		t.Fatalf("cooldown not persisted: %+v", restored)
	}
}

func TestNextRefreshAfter(t *testing.T) {
	a := newTestApp(t)
	account := &credential{ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(), RefreshToken: "r"}
	due := a.nextRefreshAfter(account)
	if due.IsZero() {
		t.Fatal("expected a refresh deadline")
	}
	if time.Until(due) > 2*time.Hour {
		t.Fatalf("deadline too late: %v", due)
	}
	noRefresh := &credential{ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()}
	if !a.nextRefreshAfter(noRefresh).IsZero() {
		t.Fatal("no refresh token means no scheduled refresh")
	}
}

func TestOrgCodeUsable(t *testing.T) {
	cases := map[string]bool{"": false, "personal": false, "org-42": true, "团队": false}
	for input, want := range cases {
		if got := orgCodeUsable(input); got != want {
			t.Fatalf("orgCodeUsable(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestUpstreamHeadersMatchClientFingerprint(t *testing.T) {
	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.ClientDeviceID = "device-1"
	cred := &credential{AccessToken: "tok", OfficeIdentity: "personal"}
	conv := conversation{SessionID: "ses-1", TurnID: "ses-1-turn-1", Title: "hello"}
	headers := a.upstreamHeaders(cfg, cred, conv, false)

	want := map[string]string{
		"Accept":                      "application/json",
		"Content-Type":                "application/json",
		"User-Agent":                  "AsyncOpenAI/Python 2.8.0",
		"X-Stainless-Lang":            "python",
		"X-Stainless-Package-Version": "2.8.0",
		"X-Stainless-OS":              "Windows",
		"X-Stainless-Arch":            "x64",
		"X-Stainless-Runtime":         "CPython",
		"X-Stainless-Runtime-Version": "3.12.13",
		"X-Stainless-Retry-Count":     "0",
		"x-client-name":               "@raccoon/office-desktop",
		"x-client-platform":           "desktop-windows-x64",
		"x-client-version":            "v1.1.3",
		"x-client-channel":            "official",
		"x-client-device-id":          "device-1",
		"X-RACCOON-Session-ID":        "ses-1",
		"X-RACCOON-Turn-ID":           "ses-1-turn-1",
		"X-RACCOON-Title":             "hello",
		"Authorization":               "Bearer tok",
	}
	for key, value := range want {
		if got := headers.Get(key); got != value {
			t.Fatalf("%s = %q, want %q", key, got, value)
		}
	}
	if headers.Get("X-Org-Code") != "" {
		t.Fatal("personal identity must not send X-Org-Code")
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.BaseURL != defaultBaseURL {
		t.Fatalf("base url = %s", cfg.BaseURL)
	}
	if cfg.MaxTokensDefault != 80000 {
		t.Fatalf("max tokens default = %d", cfg.MaxTokensDefault)
	}
	if cfg.credentialFileName() != "raccoon2api-default.json" {
		t.Fatalf("credential file = %s", cfg.credentialFileName())
	}
	if cfg.CooldownSeconds != 600 || cfg.QuotaCooldownSeconds != 3600 {
		t.Fatalf("cooldowns = %d/%d", cfg.CooldownSeconds, cfg.QuotaCooldownSeconds)
	}
	if !cfg.RetryJitter {
		t.Fatal("jitter should default on")
	}
	if !cfg.PointsLogging {
		t.Fatal("points logging should default on")
	}
}

func TestParseConfigOverrides(t *testing.T) {
	cfg, err := parseConfig([]byte("base_url: https://example.test/api/web/llm/v2/\ncredential_alias: Team A\nclient_version: 1.2.3\ncooldown_seconds: 120\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.BaseURL != "https://example.test/api/web/llm/v2" {
		t.Fatalf("base url not trimmed: %s", cfg.BaseURL)
	}
	if cfg.CredentialAlias != "team-a" {
		t.Fatalf("alias = %s", cfg.CredentialAlias)
	}
	if cfg.ClientVersion != "v1.2.3" {
		t.Fatalf("version = %s", cfg.ClientVersion)
	}
	if cfg.CooldownSeconds != 120 {
		t.Fatalf("cooldown = %d", cfg.CooldownSeconds)
	}
}

func TestDeviceIDFallsBackToDesktopFile(t *testing.T) {
	a := newTestApp(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "desktop-device-identity.json")
	if err := os.WriteFile(path, []byte(`{"clientDeviceId":"03093a05-b2e3-45c7-9041-d4b0788e0e8f"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := a.currentConfig()
	cfg.DesktopDeviceFile = path
	if got := a.deviceID(cfg); got != "03093a05-b2e3-45c7-9041-d4b0788e0e8f" {
		t.Fatalf("device id = %s", got)
	}
}

func TestAccountIDMapping(t *testing.T) {
	cases := map[string]string{
		"work":             "raccoon2api-work",
		"Team A":           "raccoon2api-team-a",
		"raccoon2api-team": "raccoon2api-team", // already prefixed: idempotent
	}
	for alias, want := range cases {
		if got := accountIDForAlias(alias); got != want {
			t.Fatalf("accountIDForAlias(%q) = %q, want %q", alias, got, want)
		}
	}
	if got := aliasFromID("raccoon2api-work"); got != "work" {
		t.Fatalf("aliasFromID = %q", got)
	}
	if got := aliasFromFileName("raccoon2api-raccoon2api-work.json"); got != "work" {
		t.Fatalf("aliasFromFileName = %q", got)
	}
	entry := hostAuthEntry{Name: "raccoon2api-work.json"}
	if got := accountIDFromHostEntry(entry); got != "raccoon2api-work" {
		t.Fatalf("accountIDFromHostEntry = %q", got)
	}
}

func TestHeaderValueLookup(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Conversation-Id", "conv-9")
	if got := headerValue(headers, "X-Session-Id", "X-Conversation-Id"); got != "conv-9" {
		t.Fatalf("header lookup = %q", got)
	}
}

// CPA parses the published credential into its own Auth struct, where
// updated_at must be a JSON string. An integer there kills the whole file.
func TestPublishedCredentialOmitsNumericTimestamps(t *testing.T) {
	a := newTestApp(t)
	account := testAccount("raccoon2api-default", 10, time.Now().Add(time.Hour))
	account.UpdatedAt = time.Now().UnixMilli()
	raw := a.authRecord(account, "").StorageJSON
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["updated_at"]; ok {
		t.Fatalf("published credential carries updated_at, CPA rejects it: %s", raw)
	}
	if decoded["access_token"] == nil || decoded["access_token"] == "" {
		t.Fatalf("credential lost its token: %s", raw)
	}
}
