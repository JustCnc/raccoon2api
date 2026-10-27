package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"unicode"
)

// conversation carries the per-turn identity the gateway expects on the
// X-RACCOON-* headers. The official runtime keeps one session id for a whole
// conversation and advances a turn counter per user message; a proxy that
// invents a new session on every request looks like a bot to the gateway.
type conversation struct {
	SessionID string
	TurnID    string
	Title     string
}

// conversationFrom derives the conversation identity from the client request.
// The payload is inspected only for stable, privacy-light seeds: the client's
// own conversation hints win, otherwise the system prompt plus the first user
// message identify the thread across turns.
func conversationFrom(cred *credential, headers http.Header, payload map[string]json.RawMessage) conversation {
	subject := ""
	if cred != nil {
		subject = cred.Subject
	}
	seed := firstNonEmpty(
		headerValue(headers, "X-Session-Id", "Session-Id", "X-Conversation-Id", "Conversation-Id", "X-Thread-Id"),
		conversationSeedFromMessages(payload),
	)
	turns := userTurnCount(payload)
	if seed == "" {
		seed = randomHex(16)
	}
	sessionID := stableSessionID(subject, seed)
	return conversation{
		SessionID: sessionID,
		TurnID:    fmt.Sprintf("%s-turn-%d", sessionID, turns),
		Title:     conversationTitle(payload),
	}
}

func conversationSeedFromMessages(payload map[string]json.RawMessage) string {
	messages, ok := decodeMessages(payload)
	if !ok || len(messages) == 0 {
		return ""
	}
	var b strings.Builder
	for _, message := range messages {
		switch message.Role {
		case "system", "developer":
			b.WriteString("s:")
			b.WriteString(truncateRunes(message.Text, 400))
			b.WriteString("\n")
		}
	}
	for _, message := range messages {
		if message.Role == "user" {
			b.WriteString("u:")
			b.WriteString(truncateRunes(message.Text, 400))
			break
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

func userTurnCount(payload map[string]json.RawMessage) int {
	messages, ok := decodeMessages(payload)
	if !ok {
		return 1
	}
	turns := 0
	for _, message := range messages {
		if message.Role == "user" {
			turns++
		}
	}
	if turns < 1 {
		turns = 1
	}
	return turns
}

func conversationTitle(payload map[string]json.RawMessage) string {
	messages, ok := decodeMessages(payload)
	if !ok {
		return ""
	}
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		title := strings.TrimSpace(collapseWhitespace(message.Text))
		if title == "" {
			return ""
		}
		// The official runtime sends the title as raw UTF-8 bytes; Go's HTTP/1.1
		// and HTTP/2 transports both accept that, so only control characters are
		// stripped here.
		return sanitizeHeaderValue(truncateRunes(title, 40))
	}
	return ""
}

// sanitizeHeaderValue drops control characters that would make a header value
// invalid while leaving multi-byte text untouched.
func sanitizeHeaderValue(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// stableSessionID renders a ses_<uuidv7>-shaped id. Version 7 keeps the value
// time-ordered like the ids the gateway itself issues, and deriving it from a
// hash keeps it stable for a conversation without leaking the seed.
func stableSessionID(subject, seed string) string {
	sum := sha256.Sum256([]byte(subject + "\x00" + seed))
	return "ses-" + uuidV7(sum[:16])
}

func uuidV7(b []byte) string {
	buf := make([]byte, 16)
	copy(buf, b)
	// Set version 7 and the RFC 4122 variant bits.
	buf[6] = (buf[6] & 0x0f) | 0x70
	buf[8] = (buf[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(buf)
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

// upstreamHeaders builds the outbound header set. Everything here mirrors a
// real capture from the desktop runtime: the OpenAI Python SDK defaults, the
// client identity block, and the X-RACCOON conversation headers.
func (a *app) upstreamHeaders(cfg Config, cred *credential, conv conversation, stream bool) http.Header {
	headers := http.Header{}
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", cfg.UserAgent)

	headers.Set("X-Stainless-Lang", "python")
	headers.Set("X-Stainless-Package-Version", cfg.SDKVersion)
	headers.Set("X-Stainless-OS", "Windows")
	headers.Set("X-Stainless-Arch", "x64")
	headers.Set("X-Stainless-Runtime", "CPython")
	headers.Set("X-Stainless-Runtime-Version", cfg.PythonVersion)
	headers.Set("X-Stainless-Retry-Count", "0")
	headers.Set("X-Stainless-Read-Timeout", fmt.Sprintf("%d", cfg.RequestTimeoutSeconds))

	headers.Set("x-client-name", cfg.ClientName)
	headers.Set("x-client-platform", cfg.ClientPlatform)
	headers.Set("x-client-version", cfg.ClientVersion)
	headers.Set("x-client-os-version", cfg.ClientOSVersion)
	headers.Set("x-client-channel", cfg.ClientChannel)
	if deviceID := a.deviceID(cfg); deviceID != "" {
		headers.Set("x-client-device-id", deviceID)
	}

	if conv.SessionID != "" {
		headers.Set("X-RACCOON-Session-ID", conv.SessionID)
	}
	if conv.TurnID != "" {
		headers.Set("X-RACCOON-Turn-ID", conv.TurnID)
	}
	if conv.Title != "" {
		headers.Set("X-RACCOON-Title", conv.Title)
	}
	if cred != nil && cred.AccessToken != "" {
		headers.Set("Authorization", "Bearer "+cred.AccessToken)
	}
	if cred != nil && orgCodeUsable(cred.OfficeIdentity) {
		headers.Set("X-Org-Code", cred.OfficeIdentity)
	}
	return headers
}

// orgCodeUsable reports whether office_identity is a real team code. Personal
// sessions carry the literal "personal", which must never reach the gateway.
func orgCodeUsable(identity string) bool {
	trimmed := strings.TrimSpace(identity)
	if trimmed == "" || trimmed == "personal" {
		return false
	}
	return isPrintableASCII(trimmed)
}

// deviceID resolves the client device fingerprint: config wins, then the
// desktop identity file, then a value persisted for this plugin.
func (a *app) deviceID(cfg Config) string {
	if id := strings.TrimSpace(cfg.ClientDeviceID); id != "" {
		return id
	}
	a.deviceMu.Lock()
	defer a.deviceMu.Unlock()
	if a.device != "" {
		return a.device
	}
	for _, path := range desktopDeviceCandidates(cfg.DesktopDeviceFile) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var file struct {
			ClientDeviceID string `json:"clientDeviceId"`
		}
		if json.Unmarshal(raw, &file) != nil {
			continue
		}
		if id := strings.TrimSpace(file.ClientDeviceID); id != "" {
			a.device = id
			return id
		}
	}
	store := a.stateStore()
	if store != nil {
		if id := store.deviceID(); id != "" {
			a.device = id
			return id
		}
	}
	id := newUUID()
	a.device = id
	if store != nil {
		_ = store.saveDeviceID(id)
	}
	a.log.warn("identity", "未找到桌面端设备指纹，已生成并持久化一个："+id)
	return id
}

func headerValue(headers http.Header, names ...string) string {
	if headers == nil {
		return ""
	}
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func isPrintableASCII(value string) bool {
	for _, r := range value {
		if r > unicode.MaxASCII || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return value != ""
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// sortedKeys is used by the ordered body encoder and log helpers.
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
