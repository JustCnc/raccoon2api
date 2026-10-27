package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the plugin-owned subtree of plugins.configs.raccoon2api.
// Host-owned keys (enabled, priority) are ignored here.
type Config struct {
	DataDir string `yaml:"data_dir"`

	// Upstream endpoints. The three prefixes are independent on purpose: the
	// gateway serves the model catalogue and chat under /api/web/llm/v2, token
	// refresh under /api/web/auth/v1, and nothing guarantees they stay siblings.
	BaseURL     string `yaml:"base_url"`
	AuthBaseURL string `yaml:"auth_base_url"`

	// Credential sources, in priority order: explicit config values, accounts
	// uploaded through the console, then whatever an earlier run persisted.
	DesktopDeviceFile  string `yaml:"desktop_device_file"`
	AccessToken        string `yaml:"access_token"`
	RefreshToken       string `yaml:"refresh_token"`
	OfficeIdentity     string `yaml:"office_identity"`
	CredentialAlias    string `yaml:"credential_alias"`
	CredentialPriority int    `yaml:"credential_priority"`
	// AccountsDir imports every *.json in a directory as one account.
	AccountsDir    string `yaml:"accounts_dir"`
	ImportInterval int    `yaml:"import_interval_seconds"`
	RefreshMargin  int    `yaml:"refresh_margin_seconds"`
	// DailyGrantEnabled claims the desktop client's daily login gift (300 积分)
	// on every account once per service day. The grant is idempotent, so a
	// repeated call only answers "already granted".
	DailyGrantEnabled bool `yaml:"daily_grant_enabled"`

	// QuotaProbeSeconds is how often each account's quota is probed upstream.
	// The public API has no balance endpoint, so the probe is a one-token
	// completion; 0 disables it.
	QuotaProbeSeconds int `yaml:"quota_probe_seconds"`

	// PointsLogging attaches the bill total of each completed request to the
	// console's request log. The bills API is read-only; it costs one GET per
	// request and does not consume credits.
	PointsLogging bool `yaml:"points_logging"`

	// Client fingerprint sent upstream. The defaults mirror the desktop build
	// that produced the captured wire traffic; update them after a client
	// upgrade so the simulation keeps matching.
	ClientName      string `yaml:"client_name"`
	ClientPlatform  string `yaml:"client_platform"`
	ClientVersion   string `yaml:"client_version"`
	ClientOSVersion string `yaml:"client_os_version"`
	ClientChannel   string `yaml:"client_channel"`
	ClientDeviceID  string `yaml:"client_device_id"`
	UserAgent       string `yaml:"user_agent"`
	SDKVersion      string `yaml:"sdk_version"`
	PythonVersion   string `yaml:"python_version"`

	// Model catalogue.
	RefreshSeconds int      `yaml:"refresh_seconds"`
	ModelDenylist  []string `yaml:"model_denylist"`
	ModelAllowlist []string `yaml:"model_allowlist"`
	// ImageModalities decides which models advertise image input:
	// auto (catalogue tags), all, or none.
	ImageModalities string `yaml:"image_modalities"`
	// VisionDenylist removes models that the catalogue tags as vision-capable
	// but that answer "I cannot see the image". Measured, not guessed.
	VisionDenylist []string `yaml:"vision_denylist"`

	// Execution.
	MaxAttempts          int    `yaml:"max_attempts"`
	RetryJitter          bool   `yaml:"retry_jitter"`
	CooldownSeconds      int    `yaml:"cooldown_seconds"`
	QuotaCooldownSeconds int    `yaml:"quota_cooldown_seconds"`
	AppName              string `yaml:"app_name"`
	LoginTimeoutSeconds  int    `yaml:"login_timeout_seconds"`
	// LoginCallbackPort is the loopback port the authorize page is told to
	// return to. 0 disables it and every login falls back to the deep link.
	LoginCallbackPort     int    `yaml:"login_callback_port"`
	RequestTimeoutSeconds int    `yaml:"request_timeout_seconds"`
	MaxTokensDefault      int    `yaml:"max_tokens_default"`
	ReasoningEffort       string `yaml:"reasoning_effort"`
	DebugDump             bool   `yaml:"debug_dump"`
}

const (
	defaultDataDir     = "plugin-data/raccoon2api"
	defaultBaseURL     = "https://xiaohuanxiong.com/api/web/llm/v2"
	defaultAuthBaseURL = "https://xiaohuanxiong.com/api/web/auth/v1"
	defaultClientName  = "@raccoon/office-desktop"
	defaultUserAgent   = "AsyncOpenAI/Python 2.8.0"
	// The page rejects the loopback flow unless the port is 1024-99999.
	defaultLoginCallbackPort = 18763
)

func defaultConfig() Config {
	return Config{
		DataDir:               defaultDataDir,
		BaseURL:               defaultBaseURL,
		AuthBaseURL:           defaultAuthBaseURL,
		CredentialAlias:       "default",
		CredentialPriority:    10,
		AppName:               "办公小浣熊客户端",
		CooldownSeconds:       600,
		QuotaCooldownSeconds:  3600,
		LoginTimeoutSeconds:   600,
		LoginCallbackPort:     defaultLoginCallbackPort,
		ImportInterval:        30,
		DailyGrantEnabled:     true,
		QuotaProbeSeconds:     600,
		PointsLogging:         true,
		RefreshMargin:         600,
		ClientName:            defaultClientName,
		ClientPlatform:        "desktop-windows-x64",
		ClientVersion:         "v1.1.3",
		ClientOSVersion:       "10.0.26100",
		ClientChannel:         "official",
		UserAgent:             defaultUserAgent,
		SDKVersion:            "2.8.0",
		PythonVersion:         "3.12.13",
		RefreshSeconds:        1800,
		ImageModalities:       "auto",
		VisionDenylist:        []string{"sn-glm-5-3"},
		MaxAttempts:           3,
		RetryJitter:           true,
		RequestTimeoutSeconds: 1200,
		MaxTokensDefault:      80000,
		ReasoningEffort:       "low",
	}
}

func parseConfig(raw []byte) (Config, error) {
	cfg := defaultConfig()
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("invalid plugin config: %w", err)
		}
	}
	if err := cfg.normalize(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) normalize() error {
	c.DataDir = strings.TrimSpace(c.DataDir)
	if c.DataDir == "" {
		c.DataDir = defaultDataDir
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL
	}
	if _, err := url.Parse(c.BaseURL); err != nil {
		return fmt.Errorf("invalid base_url: %w", err)
	}
	c.AuthBaseURL = strings.TrimRight(strings.TrimSpace(c.AuthBaseURL), "/")
	if c.AuthBaseURL == "" {
		c.AuthBaseURL = defaultAuthBaseURL
	}
	if _, err := url.Parse(c.AuthBaseURL); err != nil {
		return fmt.Errorf("invalid auth_base_url: %w", err)
	}
	c.DesktopDeviceFile = strings.TrimSpace(c.DesktopDeviceFile)
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.OfficeIdentity = strings.TrimSpace(c.OfficeIdentity)

	c.CredentialAlias = sanitizeAlias(c.CredentialAlias)
	if c.CredentialAlias == "" {
		c.CredentialAlias = "default"
	}
	c.AccountsDir = strings.TrimSpace(c.AccountsDir)
	if c.CredentialPriority <= 0 {
		c.CredentialPriority = 10
	}
	if c.CooldownSeconds <= 0 {
		c.CooldownSeconds = 600
	}
	if c.CooldownSeconds > 7200 {
		c.CooldownSeconds = 7200
	}
	if c.QuotaCooldownSeconds <= 0 {
		c.QuotaCooldownSeconds = 3600
	}
	c.AppName = strings.TrimSpace(c.AppName)
	if c.AppName == "" {
		c.AppName = "办公小浣熊客户端"
	}
	if c.LoginTimeoutSeconds <= 0 {
		c.LoginTimeoutSeconds = 600
	}
	if c.LoginCallbackPort < 0 || (c.LoginCallbackPort > 0 && c.LoginCallbackPort < 1024) || c.LoginCallbackPort > 99999 {
		c.LoginCallbackPort = 0
	}
	if c.QuotaProbeSeconds < 0 {
		c.QuotaProbeSeconds = 0
	}
	if c.QuotaProbeSeconds > 86400 {
		c.QuotaProbeSeconds = 86400
	}
	if c.ImportInterval <= 0 {
		c.ImportInterval = 30
	}
	if c.ImportInterval > 3600 {
		c.ImportInterval = 3600
	}
	if c.RefreshMargin <= 0 {
		c.RefreshMargin = 600
	}

	c.ClientName = strings.TrimSpace(c.ClientName)
	if c.ClientName == "" {
		c.ClientName = defaultClientName
	}
	c.ClientPlatform = strings.TrimSpace(c.ClientPlatform)
	if c.ClientPlatform == "" {
		c.ClientPlatform = "desktop-windows-x64"
	}
	c.ClientVersion = normalizeClientVersion(c.ClientVersion)
	if c.ClientVersion == "" {
		c.ClientVersion = "v1.1.3"
	}
	c.ClientOSVersion = strings.TrimSpace(c.ClientOSVersion)
	if c.ClientOSVersion == "" {
		c.ClientOSVersion = "10.0.26100"
	}
	c.ClientChannel = strings.TrimSpace(c.ClientChannel)
	if c.ClientChannel == "" {
		c.ClientChannel = "official"
	}
	c.ClientDeviceID = strings.TrimSpace(c.ClientDeviceID)
	c.UserAgent = strings.TrimSpace(c.UserAgent)
	if c.UserAgent == "" {
		c.UserAgent = defaultUserAgent
	}
	c.SDKVersion = strings.TrimSpace(c.SDKVersion)
	if c.SDKVersion == "" {
		c.SDKVersion = "2.8.0"
	}
	c.PythonVersion = strings.TrimSpace(c.PythonVersion)
	if c.PythonVersion == "" {
		c.PythonVersion = "3.12.13"
	}

	if c.RefreshSeconds <= 0 {
		c.RefreshSeconds = 1800
	}
	c.ImageModalities = strings.ToLower(strings.TrimSpace(c.ImageModalities))
	switch c.ImageModalities {
	case "auto", "all", "none":
	default:
		c.ImageModalities = "auto"
	}

	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.MaxAttempts > 8 {
		c.MaxAttempts = 8
	}
	if c.RequestTimeoutSeconds <= 0 {
		c.RequestTimeoutSeconds = 1200
	}
	if c.MaxTokensDefault < 0 {
		c.MaxTokensDefault = 0
	}
	c.ReasoningEffort = strings.ToLower(strings.TrimSpace(c.ReasoningEffort))
	switch c.ReasoningEffort {
	case "low", "medium", "high":
	default:
		c.ReasoningEffort = ""
	}
	return nil
}

func (c Config) requestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
}

func (c Config) refreshInterval() time.Duration {
	return time.Duration(c.RefreshSeconds) * time.Second
}

func (c Config) importInterval() time.Duration {
	return time.Duration(c.ImportInterval) * time.Second
}

func (c Config) refreshMargin() time.Duration {
	return time.Duration(c.RefreshMargin) * time.Second
}

// quotaProbeInterval is the per-account quota probe period; 0 disables it.
func (c Config) quotaProbeInterval() time.Duration {
	if c.QuotaProbeSeconds <= 0 {
		return 0
	}
	return time.Duration(c.QuotaProbeSeconds) * time.Second
}

// credentialFileName is the auth record name the host persists.
func (c Config) credentialFileName() string {
	return providerKey + "-" + c.CredentialAlias + ".json"
}

func (c Config) credentialID() string {
	return providerKey + "-" + c.CredentialAlias
}

func sanitizeAlias(alias string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(alias)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func normalizeClientVersion(version string) string {
	normalized := strings.TrimSpace(version)
	if normalized == "" {
		return ""
	}
	if !strings.HasPrefix(normalized, "v") {
		normalized = "v" + normalized
	}
	return normalized
}

// desktopAuthCandidates lists the places the official desktop client keeps its
// token, in probe order. The container path comes first because the compose
// file mounts the client config directory there.
func desktopAuthCandidates(configured string) []string {
	if strings.TrimSpace(configured) != "" {
		return []string{strings.TrimSpace(configured)}
	}
	out := []string{"/raccoon-desktop/auth.json"}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		out = append(out,
			filepath.Join(home, ".office-raccoon", "agent", "config", "auth.json"),
			filepath.Join(home, ".box-agent", "config", "auth.json"),
		)
	}
	return out
}

func desktopDeviceCandidates(configured string) []string {
	if strings.TrimSpace(configured) != "" {
		return []string{strings.TrimSpace(configured)}
	}
	out := []string{"/raccoon-desktop/desktop-device-identity.json"}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		out = append(out, filepath.Join(appData, "office-raccoon", "desktop-device-identity.json"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out,
			filepath.Join(home, "AppData", "Roaming", "office-raccoon", "desktop-device-identity.json"),
			filepath.Join(home, ".office-raccoon", "desktop-device-identity.json"),
		)
	}
	return out
}

func pluginConfigFields() []configField {
	return []configField{
		{Name: "data_dir", Type: "string", Default: defaultDataDir, Description: "插件私有数据目录（凭据副本、模型缓存）"},
		{Name: "base_url", Type: "string", Default: defaultBaseURL, Description: "LLM 网关基址"},
		{Name: "auth_base_url", Type: "string", Default: defaultAuthBaseURL, Description: "鉴权网关基址（token 刷新）"},
		{Name: "desktop_device_file", Type: "string", Default: "", Description: "桌面端设备指纹文件路径，留空则自动探测"},
		{Name: "access_token", Type: "string", Default: "", Description: "手工指定 access token（优先于桌面端文件）"},
		{Name: "refresh_token", Type: "string", Default: "", Description: "手工指定 refresh token"},
		{Name: "office_identity", Type: "string", Default: "", Description: "团队身份（X-Org-Code），个人账号留空"},
		{Name: "credential_alias", Type: "string", Default: "default", Description: "主账号在池子里的别名"},
		{Name: "credential_priority", Type: "integer", Default: 10, Description: "主账号优先级（数值越大越先用，与 CPA 一致）"},
		{Name: "accounts_dir", Type: "string", Default: "", Description: "账号目录：每个 *.json 一个账号（文件名即别名）"},
		{Name: "import_interval_seconds", Type: "integer", Default: 30, Description: "维护循环间隔：扫描账号目录、对齐 CPA 凭据（秒）"},
		{Name: "refresh_margin_seconds", Type: "integer", Default: 600, Description: "token 到期前多久主动刷新（秒）"},
		{Name: "quota_probe_seconds", Type: "integer", Default: 600, Description: "额度探测间隔（秒），0 关闭"},
		{Name: "points_logging", Type: "boolean", Default: true, Description: "请求结束后查账单，把本次消耗积分写进请求记录"},
		{Name: "client_name", Type: "string", Default: defaultClientName, Description: "x-client-name"},
		{Name: "client_platform", Type: "string", Default: "desktop-windows-x64", Description: "x-client-platform"},
		{Name: "client_version", Type: "string", Default: "v1.1.3", Description: "x-client-version（客户端升级后同步修改）"},
		{Name: "client_os_version", Type: "string", Default: "10.0.26100", Description: "x-client-os-version"},
		{Name: "client_channel", Type: "string", Default: "official", Description: "x-client-channel"},
		{Name: "client_device_id", Type: "string", Default: "", Description: "x-client-device-id，留空则读取桌面端设备指纹"},
		{Name: "user_agent", Type: "string", Default: defaultUserAgent, Description: "上游 User-Agent"},
		{Name: "sdk_version", Type: "string", Default: "2.8.0", Description: "X-Stainless-Package-Version"},
		{Name: "python_version", Type: "string", Default: "3.12.13", Description: "X-Stainless-Runtime-Version"},
		{Name: "refresh_seconds", Type: "integer", Default: 1800, Description: "模型目录刷新间隔（秒）"},
		{Name: "model_denylist", Type: "array", Default: []string{}, Description: "不上架的模型 id"},
		{Name: "model_allowlist", Type: "array", Default: []string{}, Description: "仅上架这些模型 id（留空表示全部）"},
		{Name: "image_modalities", Type: "enum", Default: "auto", EnumValues: []string{"auto", "all", "none"}, Description: "图片输入能力的声明策略"},
		{Name: "vision_denylist", Type: "array", Default: []string{"sn-glm-5-3"}, Description: "目录标了 vision 但实际看不见图片的模型（实测）"},
		{Name: "max_attempts", Type: "integer", Default: 3, Description: "单次请求最多尝试几个账号"},
		{Name: "retry_jitter", Type: "boolean", Default: true, Description: "重试退避加随机抖动"},
		{Name: "cooldown_seconds", Type: "integer", Default: 600, Description: "429/5xx 后该账号冷却时长（秒）"},
		{Name: "quota_cooldown_seconds", Type: "integer", Default: 3600, Description: "额度不足后该账号冷却时长（秒）"},
		{Name: "app_name", Type: "string", Default: "办公小浣熊客户端", Description: "授权页上报的客户端名"},
		{Name: "login_timeout_seconds", Type: "integer", Default: 600, Description: "授权链接有效期（秒）"},
		{Name: "request_timeout_seconds", Type: "integer", Default: 1200, Description: "上游请求超时（秒）"},
		{Name: "max_tokens_default", Type: "integer", Default: 80000, Description: "客户端未给 max_tokens 时补的默认值，0 表示不补"},
		{Name: "reasoning_effort", Type: "enum", Default: "low", EnumValues: []string{"", "low", "medium", "high"}, Description: "客户端未指定思考强度时下发的默认值（官方客户端默认 low），留空则不下发"},
		{Name: "debug_dump", Type: "boolean", Default: false, Description: "日志里记录出站头部与正文（排障用）"},
	}
}
