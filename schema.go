package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// Plugin identity and the RPC contract mirrored from CLIProxyAPI's
// sdk/pluginabi (ABI v1, schema v6). The plugin exchanges JSON envelopes over
// the exported C ABI, so it needs no SDK dependency to build.
const (
	providerKey   = "raccoon2api"
	pluginVersion = "0.1.1"
	abiVersion    = 1
	schemaVersion = 6
)

const (
	methodPluginRegister    = "plugin.register"
	methodPluginQuiesce     = "plugin.quiesce"
	methodPluginReconfigure = "plugin.reconfigure"
	methodPluginShutdown    = "plugin.shutdown"

	methodModelRegister = "model.register"
	methodModelStatic   = "model.static"
	methodModelForAuth  = "model.for_auth"

	methodAuthIdentifier = "auth.identifier"
	methodAuthParse      = "auth.parse"
	methodAuthRefresh    = "auth.refresh"
	methodAuthLoginStart = "auth.login.start"
	methodAuthLoginPoll  = "auth.login.poll"

	methodExecutorIdentifier    = "executor.identifier"
	methodExecutorExecute       = "executor.execute"
	methodExecutorExecuteStream = "executor.execute_stream"
	methodExecutorCountTokens   = "executor.count_tokens"

	methodManagementRegister = "management.register"
	methodManagementHandle   = "management.handle"

	methodHostStreamEmit  = "host.stream.emit"
	methodHostStreamClose = "host.stream.close"
	methodHostAuthList    = "host.auth.list"
	methodHostAuthGet     = "host.auth.get"
	methodHostAuthSave    = "host.auth.save"
)

const (
	formatChat      = "chat-completions"
	formatResponses = "responses"
	formatMessages  = "anthropic"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
	// HTTPStatus is surfaced to the downstream client (0 => 500).
	HTTPStatus int `json:"http_status,omitempty"`
}

func (e *envelopeError) Error() string { return e.Message }

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func upstreamError(message string, status int, retryable bool) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
		Code: "upstream_error", Message: message, Retryable: retryable, HTTPStatus: status,
	}})
	return raw
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginMetadata         `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type pluginMetadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	Default     any      `json:"Default"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

type registrationCapability struct {
	ModelRegistrar        bool     `json:"model_registrar"`
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	ManagementAPI         bool     `json:"management_api"`
}

// modelInfo mirrors pluginapi.ModelInfo. Only the fields this plugin fills in
// are declared; unknown-to-host zero values are harmless.
type modelInfo struct {
	ID                         string   `json:"ID"`
	Object                     string   `json:"Object"`
	Created                    int64    `json:"Created"`
	OwnedBy                    string   `json:"OwnedBy"`
	Type                       string   `json:"Type"`
	DisplayName                string   `json:"DisplayName"`
	Name                       string   `json:"Name"`
	Version                    string   `json:"Version"`
	Description                string   `json:"Description"`
	InputTokenLimit            int64    `json:"InputTokenLimit"`
	OutputTokenLimit           int64    `json:"OutputTokenLimit"`
	SupportedGenerationMethods []string `json:"SupportedGenerationMethods"`
	ContextLength              int64    `json:"ContextLength"`
	MaxCompletionTokens        int64    `json:"MaxCompletionTokens"`
	SupportedParameters        []string `json:"SupportedParameters"`
	SupportedInputModalities   []string `json:"SupportedInputModalities"`
	SupportedOutputModalities  []string `json:"SupportedOutputModalities"`
	UserDefined                bool     `json:"UserDefined"`
}

type modelRegistrationResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

type authData struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID"`
	FileName         string            `json:"FileName"`
	Label            string            `json:"Label"`
	Prefix           string            `json:"Prefix,omitempty"`
	ProxyURL         string            `json:"ProxyURL,omitempty"`
	Disabled         bool              `json:"Disabled,omitempty"`
	StorageJSON      []byte            `json:"StorageJSON"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter,omitempty"`
}

type authParseRequest struct {
	Provider string `json:"Provider"`
	Path     string `json:"Path"`
	FileName string `json:"FileName"`
	RawJSON  []byte `json:"RawJSON"`
}

type authParseResponse struct {
	Handled bool     `json:"Handled"`
	Auth    authData `json:"Auth"`
}

type authRefreshRequest struct {
	AuthID       string            `json:"AuthID"`
	AuthProvider string            `json:"AuthProvider"`
	StorageJSON  []byte            `json:"StorageJSON"`
	Metadata     map[string]any    `json:"Metadata"`
	Attributes   map[string]string `json:"Attributes"`
}

type authLoginStartRequest struct {
	Provider string         `json:"Provider"`
	BaseURL  string         `json:"BaseURL"`
	Metadata map[string]any `json:"Metadata"`
}

type authLoginStartResponse struct {
	Provider  string    `json:"Provider"`
	URL       string    `json:"URL"`
	State     string    `json:"State"`
	ExpiresAt time.Time `json:"ExpiresAt"`
}

type authLoginPollRequest struct {
	Provider string         `json:"Provider"`
	State    string         `json:"State"`
	Metadata map[string]any `json:"Metadata"`
}

type authLoginPollResponse struct {
	Status  string   `json:"Status"`
	Message string   `json:"Message"`
	Auth    authData `json:"Auth"`
}

type authRefreshResponse struct {
	Auth             authData  `json:"Auth"`
	NextRefreshAfter time.Time `json:"NextRefreshAfter,omitempty"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

// executorRequest mirrors the canonical ExecutorRequest delivered through the
// RPC envelope (default JSON field names, case-insensitive decode).
type executorRequest struct {
	AuthID          string            `json:"AuthID"`
	AuthProvider    string            `json:"AuthProvider"`
	Model           string            `json:"Model"`
	Format          string            `json:"Format"`
	Stream          bool              `json:"Stream"`
	Alt             string            `json:"Alt"`
	Headers         http.Header       `json:"Headers"`
	Query           url.Values        `json:"Query"`
	OriginalRequest []byte            `json:"OriginalRequest"`
	SourceFormat    string            `json:"SourceFormat"`
	Payload         []byte            `json:"Payload"`
	Metadata        map[string]any    `json:"Metadata"`
	StorageJSON     []byte            `json:"StorageJSON"`
	AuthMetadata    map[string]any    `json:"AuthMetadata"`
	AuthAttributes  map[string]string `json:"AuthAttributes"`
}

type rpcExecutorRequest struct {
	executorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type executorResponse struct {
	Payload  []byte         `json:"Payload"`
	Headers  http.Header    `json:"Headers,omitempty"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

type streamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type streamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type managementRegistration struct {
	Routes    []managementRouteSpec `json:"routes,omitempty"`
	Resources []resourceRouteSpec   `json:"resources,omitempty"`
}

type managementRouteSpec struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type resourceRouteSpec struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type managementRequest struct {
	Method         string      `json:"Method"`
	Path           string      `json:"Path"`
	Headers        http.Header `json:"Headers"`
	Query          url.Values  `json:"Query"`
	Body           []byte      `json:"Body"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type hostAuthEntry struct {
	ID            string    `json:"id"`
	AuthIndex     string    `json:"auth_index"`
	Name          string    `json:"name"`
	Type          string    `json:"type"`
	Provider      string    `json:"provider"`
	Label         string    `json:"label"`
	Status        string    `json:"status"`
	StatusMessage string    `json:"status_message"`
	Disabled      bool      `json:"disabled"`
	RuntimeOnly   bool      `json:"runtime_only"`
	Path          string    `json:"path"`
	BaseURL       string    `json:"base_url"`
	Success       int64     `json:"success"`
	Failed        int64     `json:"failed"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type hostAuthListResponse struct {
	Files []hostAuthEntry `json:"files"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

type hostAuthSaveRequest struct {
	Name string          `json:"name"`
	JSON json.RawMessage `json:"json"`
}

type hostAuthSaveResponse struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// normalizeFormat maps host/executor format aliases onto the canonical wire
// family.
func normalizeFormat(format string) string {
	switch format {
	case "chat-completions", "chat_completions", "openai-chat-completions", "openai_chat_completions", "openai":
		return formatChat
	case "responses", "openai-responses", "openai_responses", "openai-response":
		return formatResponses
	case "anthropic", "claude", "messages":
		return formatMessages
	default:
		return ""
	}
}
