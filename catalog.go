package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type catalogModel struct {
	ID                string   `json:"id"`
	DisplayName       string   `json:"display_name"`
	Description       string   `json:"description"`
	Tags              []string `json:"tags"`
	ContextWindow     int64    `json:"context_window"`
	MaxTokens         int64    `json:"max_tokens"`
	BillingMultiplier float64  `json:"billing_multiplier"`
	Visible           bool     `json:"visible"`
	Default           bool     `json:"default"`
}

type catalogSnapshot struct {
	FetchedAt int64          `json:"fetched_at"`
	Source    string         `json:"source"`
	Default   string         `json:"default"`
	Models    []catalogModel `json:"models"`
}

func (s catalogSnapshot) find(model string) (catalogModel, bool) {
	for _, item := range s.Models {
		if item.ID == model {
			return item, true
		}
	}
	return catalogModel{}, false
}

// modelPrefix namespaces the models this plugin serves, so a client sees
// raccoon/sn-glm-5-3-flash and the bare upstream id never leaks into CPA's
// model list. Requests may still use either form.
const modelPrefix = "raccoon/"

// withModelPrefix adds the namespace once.
func withModelPrefix(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, modelPrefix) {
		return id
	}
	return modelPrefix + id
}

// trimModelPrefix accepts both the namespaced and the bare model id.
func trimModelPrefix(id string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(id), modelPrefix))
}

func (a *app) snapshotCatalog() catalogSnapshot {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	return a.catalog
}

func (a *app) setCatalog(snapshot catalogSnapshot) {
	previous := a.snapshotCatalog()
	a.catalogMu.Lock()
	a.catalog = snapshot
	a.catalogMu.Unlock()
	if !sameModelSet(previous, snapshot) {
		// The host asks the plugin for per-auth models once, when the auth is
		// loaded. Rewriting the record makes it reload and ask again, which is
		// how a credential ends up showing its models in CPA's UI.
		go a.republishAccounts()
	}
}

// sameModelSet reports whether both snapshots expose the same model ids.
func sameModelSet(left, right catalogSnapshot) bool {
	if len(left.Models) != len(right.Models) {
		return false
	}
	seen := make(map[string]struct{}, len(left.Models))
	for _, item := range left.Models {
		seen[item.ID] = struct{}{}
	}
	for _, item := range right.Models {
		if _, ok := seen[item.ID]; !ok {
			return false
		}
	}
	return true
}

// republishAccounts rewrites every CPA credential so the host re-registers the
// models attached to each one.
func (a *app) republishAccounts() {
	accounts := a.poolSnapshot()
	if len(accounts) == 0 {
		return
	}
	for _, account := range accounts {
		a.publishAccount(account, true)
	}
}

func (a *app) catalogPath() string {
	return filepath.Join(a.currentConfig().DataDir, "models.json")
}

func (a *app) loadCachedCatalog() (catalogSnapshot, bool) {
	raw, err := os.ReadFile(a.catalogPath())
	if err != nil {
		return catalogSnapshot{}, false
	}
	var snapshot catalogSnapshot
	if json.Unmarshal(raw, &snapshot) != nil || len(snapshot.Models) == 0 {
		return catalogSnapshot{}, false
	}
	snapshot.Source = "cache"
	return snapshot, true
}

func (a *app) saveCachedCatalog(snapshot catalogSnapshot) {
	path := a.catalogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
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

// refreshCatalog pulls the model catalogue with the same identity headers the
// desktop client uses. The catalogue is account-owned: the token decides which
// models are visible.
// verifyCredential checks one token against the catalog endpoint without
// touching the pool, so an import can fail loudly instead of parking a dead
// account.
func (a *app) verifyCredential(ctx context.Context, cred *credential) error {
	cfg := a.currentConfig()
	headers := a.upstreamHeaders(cfg, cred, conversation{}, false)
	headers.Del("Content-Type")
	headers.Set("Accept", "application/json")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/model_catalog", nil)
	if err != nil {
		return err
	}
	req.Header = headers

	resp, err := a.httpClient().Do(req)
	if err != nil {
		return &upstreamFailure{Status: http.StatusBadGateway, Message: "连接上游失败：" + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return classifyFailure(resp.StatusCode, body)
}

func (a *app) refreshCatalog(ctx context.Context) error {
	cfg := a.currentConfig()
	cred, errCred := a.anyUsableCredential(ctx)
	if errCred != nil && (cred == nil || cred.AccessToken == "") {
		return errCred
	}
	headers := a.upstreamHeaders(cfg, cred, conversation{}, false)
	headers.Del("Content-Type")
	headers.Set("Accept", "application/json")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BaseURL+"/model_catalog", nil)
	if err != nil {
		return err
	}
	req.Header = headers

	resp, err := a.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("model_catalog request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, 8<<20)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("model_catalog HTTP %d: %s", resp.StatusCode, truncateRunes(strings.TrimSpace(string(raw)), 200))
	}
	snapshot, err := parseCatalog(raw)
	if err != nil {
		return err
	}
	if len(snapshot.Models) == 0 {
		return fmt.Errorf("model_catalog returned no models")
	}
	snapshot.Source = "remote"
	snapshot.FetchedAt = time.Now().Unix()
	a.setCatalog(snapshot)
	a.saveCachedCatalog(snapshot)
	a.log.info("catalog", fmt.Sprintf("模型目录已更新：%d 个模型，默认 %s", len(snapshot.Models), snapshot.Default))
	return nil
}

// parseCatalog accepts the envelope shape the gateway returns today
// (data.categories[].models[]) and tolerates a flat data.models[] list.
func parseCatalog(raw []byte) (catalogSnapshot, error) {
	var envelope struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
		Data    struct {
			Categories []struct {
				Type         string `json:"type"`
				DefaultModel string `json:"default_model"`
				Models       []struct {
					Name               string   `json:"name"`
					ModelName          string   `json:"model_name"`
					Description        string   `json:"description"`
					DisplayDescription string   `json:"display_description"`
					Visible            bool     `json:"visible"`
					Tags               []string `json:"tags"`
					BillingMultiplier  float64  `json:"billing_multiplier"`
					BillingEffective   float64  `json:"billing_effective_multiplier"`
					Params             struct {
						ContextWindow int64 `json:"context_window"`
						MaxTokens     int64 `json:"max_tokens"`
					} `json:"params"`
				} `json:"models"`
			} `json:"categories"`
			DefaultModel string `json:"default_model"`
			Models       []struct {
				Name        string `json:"name"`
				ModelName   string `json:"model_name"`
				Description string `json:"description"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return catalogSnapshot{}, fmt.Errorf("decode model_catalog: %w", err)
	}
	if envelope.Code.String() != "" && envelope.Code.String() != "0" && envelope.Code.String() != "200" {
		return catalogSnapshot{}, fmt.Errorf("model_catalog rejected: code=%s %s", envelope.Code.String(), envelope.Message)
	}

	snapshot := catalogSnapshot{Default: strings.TrimSpace(envelope.Data.DefaultModel)}
	seen := map[string]bool{}
	for _, category := range envelope.Data.Categories {
		if category.Type != "" && category.Type != "chat" {
			continue
		}
		if snapshot.Default == "" {
			snapshot.Default = strings.TrimSpace(category.DefaultModel)
		}
		for _, item := range category.Models {
			id := firstNonEmpty(strings.TrimSpace(item.ModelName), strings.TrimSpace(item.Name))
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			multiplier := item.BillingEffective
			if multiplier == 0 {
				multiplier = item.BillingMultiplier
			}
			snapshot.Models = append(snapshot.Models, catalogModel{
				ID:                id,
				DisplayName:       firstNonEmpty(item.Description, id),
				Description:       firstNonEmpty(item.DisplayDescription, item.Description),
				Tags:              item.Tags,
				ContextWindow:     item.Params.ContextWindow,
				MaxTokens:         item.Params.MaxTokens,
				BillingMultiplier: multiplier,
				Visible:           item.Visible,
				Default:           id == snapshot.Default,
			})
		}
	}
	for _, item := range envelope.Data.Models {
		id := firstNonEmpty(strings.TrimSpace(item.ModelName), strings.TrimSpace(item.Name))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		snapshot.Models = append(snapshot.Models, catalogModel{
			ID:          id,
			DisplayName: firstNonEmpty(item.Description, id),
			Description: item.Description,
			Visible:     true,
		})
	}
	sort.SliceStable(snapshot.Models, func(i, j int) bool { return snapshot.Models[i].ID < snapshot.Models[j].ID })
	return snapshot, nil
}

// modelSelection applies the allow/deny lists to a catalogue entry.
func (a *app) modelSelected(cfg Config, id string) bool {
	if len(cfg.ModelAllowlist) > 0 {
		allowed := false
		for _, candidate := range cfg.ModelAllowlist {
			if strings.TrimSpace(candidate) == id {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	for _, candidate := range cfg.ModelDenylist {
		if strings.TrimSpace(candidate) == id {
			return false
		}
	}
	return true
}

func (a *app) modelInfoList() []modelInfo {
	cfg := a.currentConfig()
	snapshot := a.snapshotCatalog()
	created := snapshot.FetchedAt
	if created == 0 {
		created = time.Now().Unix()
	}
	out := make([]modelInfo, 0, len(snapshot.Models))
	for _, item := range snapshot.Models {
		if !a.modelSelected(cfg, item.ID) {
			continue
		}
		input := []string{"text"}
		if a.imageInputEnabled(cfg, item) {
			input = []string{"text", "image"}
		}
		contextLength := item.ContextWindow
		maxCompletion := item.MaxTokens
		info := modelInfo{
			ID:                         withModelPrefix(item.ID),
			Object:                     "model",
			Created:                    created,
			OwnedBy:                    providerKey,
			Type:                       "chat",
			DisplayName:                item.DisplayName,
			Name:                       withModelPrefix(item.ID),
			Description:                item.Description,
			InputTokenLimit:            contextLength,
			OutputTokenLimit:           maxCompletion,
			ContextLength:              contextLength,
			MaxCompletionTokens:        maxCompletion,
			SupportedGenerationMethods: []string{formatChat},
			SupportedParameters: []string{
				"messages", "stream", "max_tokens", "temperature", "top_p", "stop",
				"tools", "tool_choice", "reasoning_effort",
			},
			SupportedInputModalities:  input,
			SupportedOutputModalities: []string{"text"},
		}
		out = append(out, info)
	}
	return out
}

// registeredModels falls back to the cached catalogue when the live one has not
// been fetched yet, so a restart keeps serving the same model list.
func (a *app) registeredModels() []modelInfo {
	models := a.modelInfoList()
	if len(models) > 0 {
		return models
	}
	if cached, ok := a.loadCachedCatalog(); ok {
		a.setCatalog(cached)
		return a.modelInfoList()
	}
	return models
}

// imageInputEnabled decides whether a model advertises image input. The
// catalogue's vision tag is not reliable on its own: sn-glm-5-3 carries it but
// answers "I cannot see the image", while sn-glm-5-3-flash sees images without
// the tag. The denylist encodes what was measured.
func (a *app) imageInputEnabled(cfg Config, item catalogModel) bool {
	switch cfg.ImageModalities {
	case "all":
		return true
	case "none":
		return false
	}
	if !hasTag(item.Tags, "vision") {
		return false
	}
	for _, denied := range cfg.VisionDenylist {
		if strings.TrimSpace(denied) == item.ID {
			return false
		}
	}
	return true
}

func hasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if strings.EqualFold(strings.TrimSpace(tag), want) {
			return true
		}
	}
	return false
}

// modelMaxTokens returns the catalogue's output ceiling for a model, or 0 when
// the catalogue does not know it.
func (a *app) modelMaxTokens(model string) int64 {
	if item, ok := a.snapshotCatalog().find(model); ok && item.MaxTokens > 0 {
		return item.MaxTokens
	}
	return 0
}

// resolveModel validates the requested model against the catalogue. An empty
// catalogue means discovery has not succeeded yet, so the request is passed
// through untouched rather than rejected.
func (a *app) resolveModel(requested string) (string, error) {
	cfg := a.currentConfig()
	model := trimModelPrefix(requested)
	snapshot := a.snapshotCatalog()
	if len(snapshot.Models) == 0 {
		if model == "" {
			return snapshot.Default, nil
		}
		return model, nil
	}
	if model == "" {
		model = snapshot.Default
	}
	if model == "" {
		model = snapshot.Models[0].ID
	}
	if _, ok := snapshot.find(model); !ok {
		return "", fmt.Errorf("模型 %q 不在小浣熊目录里；可用：%s", model, strings.Join(a.modelIDs(), ", "))
	}
	if !a.modelSelected(cfg, model) {
		return "", fmt.Errorf("模型 %q 已被插件配置禁用", model)
	}
	return model, nil
}

func (a *app) modelIDs() []string {
	snapshot := a.snapshotCatalog()
	ids := make([]string, 0, len(snapshot.Models))
	for _, item := range snapshot.Models {
		ids = append(ids, withModelPrefix(item.ID))
	}
	return ids
}

func (a *app) catalogLoop(ctx context.Context) {
	for {
		interval := a.currentConfig().refreshInterval()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := a.refreshCatalog(refreshCtx); err != nil {
			a.log.warn("catalog", "模型目录刷新失败："+err.Error())
		}
		cancel()
	}
}
