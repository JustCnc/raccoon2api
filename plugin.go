package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type app struct {
	mu      sync.Mutex
	cfg     Config
	stop    context.CancelFunc
	done    chan struct{}
	started bool

	state    *stateStore
	log      *logRing
	metrics  *metrics
	requests *requestLog

	// published maps an account id to the fingerprint last written to its CPA
	// auth record, so an unchanged record is not rewritten on every tick.
	// publishedPath is the file the host reported for it, used on delete.
	published     map[string]string
	publishedPath map[string]string

	catalogMu sync.Mutex
	catalog   catalogSnapshot

	clientMu sync.Mutex
	client   *http.Client

	// account pool
	credMu   sync.Mutex
	accounts map[string]*credential
	refresh  map[string]*refreshFlight

	deviceMu sync.Mutex
	device   string

	loginMu       sync.Mutex
	logins        map[string]*loginSession
	loginCbMu     sync.Mutex
	loginCb       *http.Server
	completedCode map[string]*credential

	// saveMu serializes accounts.json writers.
	saveMu sync.Mutex
}

var plugin = &app{log: newLogRing(600), metrics: newMetrics(), requests: newRequestLog(300)}

func (a *app) currentConfig() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// stateStore returns the private data directory handle under the same lock that
// guards configuration, so a reconfigure cannot swap it under a reader.
func (a *app) stateStore() *stateStore {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

func (a *app) configure(raw []byte) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	a.mu.Lock()
	previous := a.cfg
	a.cfg = cfg
	a.mu.Unlock()

	a.resetHTTPClient()

	a.mu.Lock()
	if a.state == nil || previous.DataDir != cfg.DataDir {
		store, errStore := newStateStore(cfg.DataDir)
		if errStore != nil {
			a.mu.Unlock()
			return fmt.Errorf("init state store: %w", errStore)
		}
		a.state = store
	}
	a.mu.Unlock()

	a.credMu.Lock()
	loaded := len(a.accounts) > 0
	a.credMu.Unlock()
	if !loaded {
		a.loadPool()
	}

	// Load the catalogue before touching the auth records: the host asks the
	// plugin for an auth's models the moment the record is written, and an empty
	// answer leaves that credential with "no models" in CPA's UI forever.
	if len(a.snapshotCatalog().Models) == 0 {
		if cached, ok := a.loadCachedCatalog(); ok {
			a.setCatalog(cached)
		}
	}
	if len(a.snapshotCatalog().Models) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if errCatalog := a.refreshCatalog(ctx); errCatalog != nil {
			a.log.warn("catalog", "首次模型发现失败："+errCatalog.Error())
		}
		cancel()
	}

	// Publish whatever is already known without touching the network beyond the
	// catalogue fetch above; the maintenance loop rescans the accounts directory
	// and refreshes stale tokens a second later.
	if configured := credentialFromConfig(cfg); configured != nil {
		a.upsertAccount(configured)
	}
	a.scanAccountsDir(cfg)
	for _, account := range a.poolSnapshot() {
		a.publishAccount(account, false)
	}
	a.ensureHostRecords()

	a.startBackground()
	return nil
}

// startBackground launches the maintenance goroutines once per lifecycle. It is
// safe to call again after shutdown, which matters because the host may
// reconfigure a plugin it just quiesced.
func (a *app) startBackground() {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.stop = cancel
	a.done = make(chan struct{})
	a.started = true
	done := a.done
	a.mu.Unlock()

	go a.catalogLoop(ctx)
	go a.maintenanceLoop(ctx)
	go a.quotaLoop(ctx)
	a.startLoginCallback()
	go func() {
		<-ctx.Done()
		close(done)
	}()
}

func (a *app) shutdown() {
	a.stopLoginCallback()
	a.mu.Lock()
	cancel := a.stop
	done := a.done
	a.stop = nil
	a.started = false
	a.done = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			a.log.warn("plugin", "后台任务未在 5 秒内退出，继续关闭")
		}
	}
	a.resetHTTPClient()
}

// maintenanceLoop keeps the pool, the auth records and the catalogue in sync.
func (a *app) maintenanceLoop(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			interval := a.currentConfig().importInterval()
			a.maintainOnce(ctx)
			timer.Reset(interval)
		}
	}
}

func (a *app) maintainOnce(ctx context.Context) {
	cfg := a.currentConfig()
	a.scanAccountsDir(cfg)
	a.syncAccountsFromHost()
	a.refreshStaleAccounts(ctx)
	a.ensureHostRecords()

	// The cached catalogue serves the first requests; the first maintenance tick
	// replaces it with a live one so a restart never serves stale models for long.
	if snapshot := a.snapshotCatalog(); len(snapshot.Models) == 0 || snapshot.Source != "remote" {
		refreshCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := a.refreshCatalog(refreshCtx); err != nil {
			a.log.warn("catalog", "模型目录刷新失败："+err.Error())
		}
		cancel()
	}
}

// refreshStaleAccounts renews tokens that are about to expire, one account at a
// time so a single bad credential cannot stall the loop.
func (a *app) refreshStaleAccounts(ctx context.Context) {
	margin := a.currentConfig().refreshMargin()
	for _, account := range a.poolSnapshot() {
		if account.Dead || account.Disabled || !account.needsRefresh(margin) {
			continue
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if _, err := a.ensureAccountFresh(refreshCtx, account); err != nil {
			a.log.warn("accounts", fmt.Sprintf("账号 %s 主动刷新失败：%s", account.Alias, err.Error()))
		}
		cancel()
	}
}

// ensureHostRecords recreates any pool account whose CPA credential is missing
// from the host: without the record the host rejects the request with
// auth_not_found before the plugin ever sees it.
func (a *app) ensureHostRecords() {
	accounts := a.poolSnapshot()
	if len(accounts) == 0 {
		return
	}
	entries, err := a.hostAuths()
	if err != nil {
		a.log.warn("accounts", "读取 CPA 凭据失败："+err.Error())
		return
	}
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if id := accountIDFromHostEntry(entry); id != "" {
			present[id] = true
		}
	}
	for _, account := range accounts {
		if account.AccessToken == "" || present[account.ID] {
			continue
		}
		// Force the write: the record is missing, so an unchanged fingerprint
		// must not suppress the repair.
		a.publishAccount(account, true)
		a.log.info("accounts", "已把账号 "+account.Alias+" 重新写入 CPA 凭据")
	}
}

func (a *app) hostAuths() ([]hostAuthEntry, error) {
	raw, err := hostCall(methodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp hostAuthListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}
	out := make([]hostAuthEntry, 0, len(resp.Files))
	for _, entry := range resp.Files {
		if entry.Provider == providerKey || entry.Type == providerKey {
			out = append(out, entry)
		}
	}
	return out, nil
}

func (a *app) registration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             "Raccoon Office (xiaohuanxiong)",
			Version:          pluginVersion,
			Author:           "raccoon2api",
			GitHubRepository: "https://github.com/",
			Logo:             "",
			ConfigFields:     pluginConfigFields(),
		},
		Capabilities: registrationCapability{
			ModelRegistrar:        true,
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    "both",
			ExecutorInputFormats:  []string{formatChat},
			ExecutorOutputFormats: []string{formatChat},
			ManagementAPI:         true,
		},
	}
}

func decodeRequest[T any](raw []byte) (T, error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode %T: %w", out, err)
	}
	return out, nil
}

// handleMethod is the plugin's JSON-RPC dispatch, invoked from the C ABI.
func (a *app) handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		lifecycle, err := decodeRequest[lifecycleRequest](request)
		if err != nil {
			return nil, err
		}
		if err := a.configure(lifecycle.ConfigYAML); err != nil {
			return errorEnvelope("config_error", err.Error()), nil
		}
		if method == methodPluginRegister {
			a.log.info("plugin", "插件已注册")
		} else {
			a.log.info("plugin", "配置已更新")
		}
		return okEnvelope(a.registration())
	case methodPluginQuiesce:
		return okEnvelope(map[string]any{})
	case methodPluginShutdown:
		a.shutdown()
		a.log.info("plugin", "插件已停止")
		return okEnvelope(map[string]any{})
	case methodModelRegister, methodModelStatic, methodModelForAuth:
		return okEnvelope(modelRegistrationResponse{Provider: providerKey, Models: a.registeredModels()})
	case methodAuthIdentifier, methodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerKey})
	case methodAuthParse:
		return a.parseAuth(request)
	case methodAuthLoginStart:
		return a.startLogin(request)
	case methodAuthLoginPoll:
		return a.pollLogin(request)
	case methodAuthRefresh:
		return a.refreshAuth(request)
	case methodExecutorExecute:
		return a.execute(request)
	case methodExecutorExecuteStream:
		return a.executeStream(request)
	case methodExecutorCountTokens:
		return a.countTokens(request)
	case methodManagementRegister:
		return okEnvelope(a.managementRegistration())
	case methodManagementHandle:
		req, err := decodeRequest[managementRequest](request)
		if err != nil {
			return nil, err
		}
		return okEnvelope(a.handleManagement(req))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// parseAuth teaches the host how to read this plugin's auth records. A record
// may also carry a one-time authorization code, which is exchanged right here.
func (a *app) parseAuth(raw []byte) ([]byte, error) {
	req, err := decodeRequest[authParseRequest](raw)
	if err != nil {
		return nil, err
	}
	if len(req.RawJSON) == 0 {
		return okEnvelope(authParseResponse{Handled: false})
	}
	var file credentialInput
	if json.Unmarshal(req.RawJSON, &file) != nil {
		return okEnvelope(authParseResponse{Handled: false})
	}

	// Only claim files that belong to this provider: the host walks every auth
	// provider when the request carries no provider hint, and an unrelated JSON
	// with a "code" field must not be swallowed here.
	ourProvider := strings.EqualFold(strings.TrimSpace(req.Provider), providerKey)
	untyped := strings.TrimSpace(file.Type) == ""
	if file.Type != providerKey && !(untyped && ourProvider) {
		return okEnvelope(authParseResponse{Handled: false})
	}

	if code := firstNonEmpty(file.AuthorizationCode, file.Code); code != "" {
		if cached := a.completedCodeAccount(code); cached != nil {
			auth := a.authRecord(cached, req.FileName)
			return okEnvelope(authParseResponse{Handled: true, Auth: auth})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		account, errLogin := a.completeLogin(ctx, "", code, aliasFromFileName(req.FileName))
		if errLogin != nil {
			return errorEnvelope("auth_error", errLogin.Error()), nil
		}
		a.rememberCompletedCode(code, account)
		auth := a.authRecord(account, req.FileName)
		a.log.info("auth", "已用授权码创建账号："+account.Alias)
		return okEnvelope(authParseResponse{Handled: true, Auth: auth})
	}

	if untyped && file.AccessToken == "" {
		return okEnvelope(authParseResponse{Handled: false})
	}
	account, errDecode := decodeCredential(req.RawJSON)
	if errDecode != nil || account.AccessToken == "" {
		return okEnvelope(authParseResponse{Handled: false})
	}
	alias := aliasFromFileName(firstNonEmpty(req.FileName, account.Alias))
	if alias == "" {
		alias = aliasFromFileName(account.Alias)
	}
	if alias == "" {
		alias = "auth-" + randomHex(3)
	}
	account.ID = accountIDForAlias(alias)
	account.Alias = alias
	account.Source = firstNonEmpty(account.Source, "auth-file")
	if account.Priority == 0 {
		account.Priority = fallbackPriority(a.currentConfig().CredentialPriority, 5)
	}
	account.UpdatedAt = time.Now().UnixMilli()
	a.upsertAccount(account)
	auth := a.authRecord(account, firstNonEmpty(req.FileName, account.ID+".json"))
	a.log.info("auth", "解析到凭据文件："+alias)
	return okEnvelope(authParseResponse{Handled: true, Auth: auth})
}

// refreshAuth is called by the host on a 401 or from the management UI.
func (a *app) refreshAuth(raw []byte) ([]byte, error) {
	req, err := decodeRequest[authRefreshRequest](raw)
	if err != nil {
		return nil, err
	}
	account, errCred := decodeCredential(req.StorageJSON)
	if errCred != nil || account.AccessToken == "" {
		account = nil
	}
	if account != nil && account.ID == "" {
		account.ID = strings.TrimSpace(req.AuthID)
	}
	if account == nil && req.AuthID != "" {
		account = a.poolAccount(req.AuthID)
	}
	if account == nil {
		return errorEnvelope("auth_error", "没有可刷新的登录态"), nil
	}
	// The host may hold an older snapshot; a valid token in the pool wins.
	if current := a.poolAccount(account.ID); current != nil && current.AccessToken != "" &&
		current.AccessToken != account.AccessToken && !current.needsRefresh(a.currentConfig().refreshMargin()) {
		auth := a.authRecord(current, "")
		return okEnvelope(authRefreshResponse{Auth: auth, NextRefreshAfter: auth.NextRefreshAfter})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	next, errRefresh := a.refreshAccount(ctx, account)
	if errRefresh != nil {
		return errorEnvelope("auth_refresh_failed", errRefresh.Error()), nil
	}
	auth := a.authRecord(next, "")
	return okEnvelope(authRefreshResponse{Auth: auth, NextRefreshAfter: auth.NextRefreshAfter})
}
