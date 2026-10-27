package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const catalogSample = `{
  "code": 0,
  "message": "success",
  "data": {
    "categories": [
      {
        "type": "chat",
        "default_model": "raccoon-8c4485",
        "models": [
          {
            "name": "raccoon-8c4485",
            "description": "Raccoon-Work",
            "visible": false,
            "billing_category": "normal",
            "model_name": "raccoon-8c4485",
            "display_description": "适合复杂办公任务",
            "tags": ["general", "office", "code", "vision", "reasoning", "auto"],
            "ability_level": 2,
            "params": {"context_window": 1000000, "max_tokens": 100000},
            "billing_multiplier": 1,
            "billing_effective_multiplier": 1,
            "billing_status": "normal"
          },
          {
            "name": "sn-glm-5-3",
            "description": "GLM-5-3",
            "visible": true,
            "model_name": "sn-glm-5-3",
            "tags": ["general", "vision", "reasoning"],
            "params": {"context_window": 1000000, "max_tokens": 100000},
            "billing_multiplier": 0.75,
            "billing_effective_multiplier": 0.75
          },
          {
            "name": "sn-glm-5-3-flash",
            "description": "GLM-5-3-Flash",
            "visible": true,
            "model_name": "sn-glm-5-3-flash",
            "display_description": "适合通用对话",
            "tags": ["general", "chat", "fast"],
            "params": {"context_window": 1000000, "max_tokens": 100000},
            "billing_multiplier": 0.2,
            "billing_effective_multiplier": 0.2
          }
        ]
      },
      {
        "type": "image",
        "models": [
          {"name": "image-model", "model_name": "image-model"}
        ]
      }
    ]
  }
}`

func TestParseCatalog(t *testing.T) {
	snapshot, err := parseCatalog([]byte(catalogSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snapshot.Default != "raccoon-8c4485" {
		t.Fatalf("default = %s", snapshot.Default)
	}
	if len(snapshot.Models) != 3 {
		t.Fatalf("models = %d, want 3 (image category skipped)", len(snapshot.Models))
	}
	var flash catalogModel
	for _, model := range snapshot.Models {
		if model.ID == "sn-glm-5-3-flash" {
			flash = model
		}
	}
	if flash.ID == "" {
		t.Fatal("sn-glm-5-3-flash missing")
	}
	if flash.ContextWindow != 1000000 || flash.MaxTokens != 100000 {
		t.Fatalf("limits = %+v", flash)
	}
	if flash.BillingMultiplier != 0.2 {
		t.Fatalf("multiplier = %v", flash.BillingMultiplier)
	}
}

func TestParseCatalogRejectsErrorEnvelope(t *testing.T) {
	if _, err := parseCatalog([]byte(`{"code":200002,"message":"authorization_invalid_error"}`)); err == nil {
		t.Fatal("expected an error for a rejected catalogue call")
	}
}

func TestModelInfoModalities(t *testing.T) {
	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.ImageModalities = "auto"
	a.cfg = cfg
	snapshot, err := parseCatalog([]byte(catalogSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.setCatalog(snapshot)
	models := a.modelInfoList()
	if len(models) != 3 {
		t.Fatalf("models = %d", len(models))
	}
	hasImage := map[string]bool{}
	for _, model := range models {
		for _, modality := range model.SupportedInputModalities {
			if modality == "image" {
				hasImage[model.ID] = true
			}
		}
	}
	// Model ids are namespaced for CPA; the checks below use that form.
	if !hasImage["raccoon/raccoon-8c4485"] {
		t.Fatal("vision-tagged model should advertise image input")
	}
	if hasImage["raccoon/sn-glm-5-3"] {
		t.Fatal("sn-glm-5-3 is tagged vision but cannot see images; it must be denied")
	}
	if hasImage["raccoon/sn-glm-5-3-flash"] {
		t.Fatal("model without a vision tag should not advertise image input")
	}
}

func TestResolveModelRejectsUnknown(t *testing.T) {
	a := newTestApp(t)
	snapshot, err := parseCatalog([]byte(catalogSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.setCatalog(snapshot)
	if _, err := a.resolveModel("does-not-exist"); err == nil {
		t.Fatal("unknown model should be rejected")
	}
	if model, err := a.resolveModel(""); err != nil || model != "raccoon-8c4485" {
		t.Fatalf("default resolution = %s, %v", model, err)
	}
	if model, err := a.resolveModel("sn-glm-5-3-flash"); err != nil || model != "sn-glm-5-3-flash" {
		t.Fatalf("explicit resolution = %s, %v", model, err)
	}
}

func TestResolveModelHonoursDenylist(t *testing.T) {
	a := newTestApp(t)
	cfg := a.currentConfig()
	cfg.ModelDenylist = []string{"sn-glm-5-3-flash"}
	a.cfg = cfg
	snapshot, err := parseCatalog([]byte(catalogSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.setCatalog(snapshot)
	if _, err := a.resolveModel("sn-glm-5-3-flash"); err == nil {
		t.Fatal("denied model should be rejected")
	}
	if len(a.modelInfoList()) != 2 {
		t.Fatalf("exposed models = %d", len(a.modelInfoList()))
	}
}

func TestCatalogCacheRoundTrip(t *testing.T) {
	a := newTestApp(t)
	snapshot, err := parseCatalog([]byte(catalogSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	snapshot.Source = "remote"
	snapshot.FetchedAt = 1700000000
	a.saveCachedCatalog(snapshot)
	cached, ok := a.loadCachedCatalog()
	if !ok {
		t.Fatal("cache should load")
	}
	if cached.Source != "cache" || len(cached.Models) != len(snapshot.Models) {
		t.Fatalf("cached = %+v", cached)
	}
}

func TestIsErrorEnvelope(t *testing.T) {
	cases := map[string]bool{
		`{"id":"x","choices":[{"index":0}]}`:        false,
		`{"error":{"code":"500","message":"boom"}}`: true,
		`{"code":200002,"message":"bad"}`:           true,
		`{"code":0,"choices":[]}`:                   false,
		`not json`:                                  false,
	}
	for body, want := range cases {
		if got := isErrorEnvelope([]byte(body)); got != want {
			t.Fatalf("isErrorEnvelope(%s) = %v, want %v", body, got, want)
		}
	}
	_ = json.Valid
}

func TestRegisteredModelsAreNamespaced(t *testing.T) {
	a := newTestApp(t)
	snapshot, err := parseCatalog([]byte(catalogSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.setCatalog(snapshot)

	for _, model := range a.registeredModels() {
		if !strings.HasPrefix(model.ID, modelPrefix) {
			t.Fatalf("model %q is not namespaced", model.ID)
		}
	}

	// Both forms resolve to the bare upstream id.
	bare, errBare := a.resolveModel("sn-glm-5-3-flash")
	if errBare != nil {
		t.Fatalf("bare id rejected: %v", errBare)
	}
	prefixed, errPrefixed := a.resolveModel("raccoon/sn-glm-5-3-flash")
	if errPrefixed != nil {
		t.Fatalf("prefixed id rejected: %v", errPrefixed)
	}
	if bare != prefixed || bare != "sn-glm-5-3-flash" {
		t.Fatalf("resolve mismatch: bare=%q prefixed=%q", bare, prefixed)
	}
	if _, err := a.resolveModel("raccoon/does-not-exist"); err == nil {
		t.Fatal("unknown model accepted")
	}
}
