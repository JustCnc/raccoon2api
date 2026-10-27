package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func postJSON(t *testing.T, a *app, path, body string) map[string]any {
	t.Helper()
	response := a.handleManagement(managementRequest{Method: "POST", Path: path, Body: []byte(body)})
	var out map[string]any
	if err := json.Unmarshal(response.Body, &out); err != nil {
		t.Fatalf("decode response %q: %v", string(response.Body), err)
	}
	return out
}

func TestAddAccountImportsJSON(t *testing.T) {
	var saved []hostAuthSaveRequest
	previous := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if request, ok := payload.(hostAuthSaveRequest); ok {
			saved = append(saved, request)
		}
		return json.RawMessage(`{}`), nil
	}
	defer func() { hostCall = previous }()

	a := newTestApp(t)
	token := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-import", "ses-import")
	raw, _ := json.Marshal(map[string]any{"access_token": token, "office_identity": "personal"})

	out := postJSON(t, a, "/accounts/add", `{"json":`+string(mustJSON(t, string(raw)))+`,"alias":"work","priority":7,"verify":false}`)
	if out["ok"] != true {
		t.Fatalf("import failed: %v", out)
	}
	account := a.poolAccount("raccoon2api-work")
	if account == nil {
		t.Fatalf("account not in pool: %v", out)
	}
	if account.Priority != 7 || account.OfficeIdentity != "personal" || account.Subject != "pu-import" {
		t.Fatalf("unexpected account: %+v", account)
	}
	if account.Source != "import" {
		t.Fatalf("source = %q", account.Source)
	}
	if len(saved) != 1 || saved[0].Name != "raccoon2api-work.json" {
		t.Fatalf("account was not published to the host: %+v", saved)
	}
	var published credentialFile
	if err := json.Unmarshal(saved[0].JSON, &published); err != nil {
		t.Fatalf("published record: %v", err)
	}
	if published.Type != providerKey || published.AccessToken != token || published.Priority != 7 {
		t.Fatalf("unexpected published record: %+v", published)
	}
}

func TestAddAccountAcceptsRawBody(t *testing.T) {
	a := newTestApp(t)
	token := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-raw", "ses-raw")
	out := postJSON(t, a, "/accounts/add", `{"access_token":"`+token+`","verify":false}`)
	if out["ok"] != true {
		t.Fatalf("import failed: %v", out)
	}
	if a.poolSize() != 1 {
		t.Fatalf("pool size = %d", a.poolSize())
	}
}

func TestAddAccountIsIdempotentBySubject(t *testing.T) {
	a := newTestApp(t)
	token := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-same", "ses-same")
	first := postJSON(t, a, "/accounts/add", `{"access_token":"`+token+`","verify":false}`)
	second := postJSON(t, a, "/accounts/add", `{"access_token":"`+token+`","verify":false}`)
	if first["id"] != second["id"] {
		t.Fatalf("same account imported twice: %v vs %v", first["id"], second["id"])
	}
	if a.poolSize() != 1 {
		t.Fatalf("pool size = %d, want 1", a.poolSize())
	}
}

func TestAddAccountRejectsExpiredToken(t *testing.T) {
	a := newTestApp(t)
	token := fakeJWT(time.Now().Add(-time.Hour).Unix(), "pu-old", "ses-old")
	out := postJSON(t, a, "/accounts/add", `{"access_token":"`+token+`","verify":false}`)
	if out["ok"] != false {
		t.Fatalf("expired token accepted: %v", out)
	}
	if a.poolSize() != 0 {
		t.Fatalf("pool size = %d", a.poolSize())
	}
}

func TestAddAccountRejectsTokenUpstreamRefuses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401,"message":"authorization_verify_error"}`))
	}))
	defer server.Close()

	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = server.URL
	a.cfg = cfg

	token := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-bad", "ses-bad")
	out := postJSON(t, a, "/accounts/add", `{"access_token":"`+token+`"}`)
	if out["ok"] != false {
		t.Fatalf("upstream-rejected token accepted: %v", out)
	}
	if a.poolSize() != 0 {
		t.Fatalf("pool size = %d", a.poolSize())
	}
}

func TestAddAccountKeepsTokenWhenVerificationIsUnreachable(t *testing.T) {
	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.BaseURL = "http://127.0.0.1:1"
	a.cfg = cfg

	token := fakeJWT(time.Now().Add(24*time.Hour).Unix(), "pu-offline", "ses-offline")
	out := postJSON(t, a, "/accounts/add", `{"access_token":"`+token+`"}`)
	if out["ok"] != true {
		t.Fatalf("offline verification blocked the import: %v", out)
	}
	if message, _ := out["message"].(string); message == "" {
		t.Fatal("expected a warning in the message")
	}
}

func TestAddAccountRejectsGarbage(t *testing.T) {
	a := newTestApp(t)
	for _, body := range []string{`{"json":"not json"}`, `{"json":{}}`, `[]`} {
		out := postJSON(t, a, "/accounts/add", body)
		if out["ok"] != false {
			t.Fatalf("body %s accepted: %v", body, out)
		}
	}
	if a.poolSize() != 0 {
		t.Fatalf("pool size = %d", a.poolSize())
	}
}

func TestAddAccountKeepsExistingPriorityAndSource(t *testing.T) {
	a := newTestApp(t)
	existing := testAccount("raccoon2api-default", 10, time.Now().Add(24*time.Hour))
	existing.Source = "desktop"
	a.upsertAccount(existing)

	out := postJSON(t, a, "/accounts/add", `{"json":{"access_token":"`+existing.AccessToken+`"},"verify":false}`)
	if out["ok"] != true {
		t.Fatalf("import failed: %v", out)
	}
	account := a.poolAccount("raccoon2api-default")
	if account.Priority != 10 {
		t.Fatalf("priority demoted to %d", account.Priority)
	}
	if account.Source != "desktop" {
		t.Fatalf("source changed to %q", account.Source)
	}
}

func TestAddAccountHonoursExplicitPriority(t *testing.T) {
	a := newTestApp(t)
	existing := testAccount("raccoon2api-default", 10, time.Now().Add(24*time.Hour))
	existing.Source = "desktop"
	a.upsertAccount(existing)

	out := postJSON(t, a, "/accounts/add", `{"json":{"access_token":"`+existing.AccessToken+`"},"priority":1,"verify":false}`)
	if out["ok"] != true {
		t.Fatalf("import failed: %v", out)
	}
	if got := a.poolAccount("raccoon2api-default").Priority; got != 1 {
		t.Fatalf("priority = %d, want the explicit 1", got)
	}
}

func TestExportSingleAccountIsImportable(t *testing.T) {
	a := newTestApp(t)
	account := testAccount("raccoon2api-work", 7, time.Now().Add(24*time.Hour))
	account.Source = "upload"
	a.upsertAccount(account)

	resp := a.handleManagement(managementRequest{Method: "GET", Path: "/accounts/export", Query: url.Values{"id": {"raccoon2api-work"}}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, resp.Body)
	}
	if got := resp.Headers.Get("Content-Disposition"); got != `attachment; filename="raccoon2api-work.json"` {
		t.Fatalf("content-disposition = %q", got)
	}
	var exported credentialFile
	if err := json.Unmarshal(resp.Body, &exported); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if exported.AccessToken != account.AccessToken || exported.Priority != 7 || exported.Alias != "work" {
		t.Fatalf("unexpected export: %+v", exported)
	}

	// The exported record must import straight back.
	other := newTestApp(t)
	out := postJSON(t, other, "/accounts/add", `{"json":`+string(resp.Body)+`,"verify":false}`)
	if out["ok"] != true {
		t.Fatalf("re-import failed: %v", out)
	}
	if other.poolAccount("raccoon2api-work") == nil {
		t.Fatal("account not restored")
	}
}

func TestExportAllAndBatchImport(t *testing.T) {
	a := newTestApp(t)
	a.upsertAccount(testAccount("raccoon2api-a", 10, time.Now().Add(time.Hour)))
	a.upsertAccount(testAccount("raccoon2api-b", 5, time.Now().Add(time.Hour)))

	resp := a.handleManagement(managementRequest{Method: "GET", Path: "/accounts/export"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var exported struct {
		Provider string           `json:"provider"`
		Accounts []credentialFile `json:"accounts"`
	}
	if err := json.Unmarshal(resp.Body, &exported); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if exported.Provider != providerKey || len(exported.Accounts) != 2 {
		t.Fatalf("unexpected export: %+v", exported)
	}

	other := newTestApp(t)
	out := postJSON(t, other, "/accounts/add", `{"json":`+string(resp.Body)+`,"verify":false}`)
	if out["ok"] != true {
		t.Fatalf("batch import failed: %v", out)
	}
	if other.poolSize() != 2 {
		t.Fatalf("pool size = %d, want 2", other.poolSize())
	}
}

func TestBatchImportAcceptsBareArray(t *testing.T) {
	a := newTestApp(t)
	first := testAccount("raccoon2api-a", 10, time.Now().Add(time.Hour))
	second := testAccount("raccoon2api-b", 5, time.Now().Add(time.Hour))
	raw, _ := json.Marshal([]map[string]string{
		{"access_token": first.AccessToken, "alias": "a"},
		{"access_token": second.AccessToken, "alias": "b"},
	})
	out := postJSON(t, a, "/accounts/add", `{"json":`+string(raw)+`,"verify":false}`)
	if out["ok"] != true {
		t.Fatalf("import failed: %v", out)
	}
	if a.poolSize() != 2 {
		t.Fatalf("pool size = %d", a.poolSize())
	}
}

func TestExportUnknownAccount(t *testing.T) {
	a := newTestApp(t)
	resp := a.handleManagement(managementRequest{Method: "GET", Path: "/accounts/export", Query: url.Values{"id": {"nope"}}})
	var out map[string]any
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["ok"] != false {
		t.Fatalf("unexpected: %v", out)
	}
}
