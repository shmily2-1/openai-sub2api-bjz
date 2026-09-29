package basispoints

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeSub2APIConfigUsesSafeDefaults(t *testing.T) {
	cfg, normalized, err := NormalizeSub2APIConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("NormalizeSub2APIConfig() error = %v", err)
	}
	if cfg.ResponsesURL != DefaultResponsesURL || cfg.UpstreamModel != DefaultUpstreamModel {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.MaxRequestBytes != defaultSub2APIMaxRequest || len(normalized) == 0 {
		t.Fatalf("request limit/default JSON missing: %#v %q", cfg, normalized)
	}
	if len(cfg.Models) != 5 || cfg.ModelMappings[DefaultSolModelID] != DefaultSolModelID || cfg.ModelMappings[DefaultLunaModelID] != DefaultLunaModelID || cfg.ModelMappings[DefaultTerraModelID] != DefaultTerraModelID || cfg.ModelMappings[DefaultAstraModelID] != DefaultAstraModelID {
		t.Fatalf("default GPT-6 models are not enabled: %#v", cfg)
	}
}

func TestNormalizeSub2APIConfigRejectsUnknownFields(t *testing.T) {
	if _, _, err := NormalizeSub2APIConfig([]byte(`{"unknown":true}`)); err == nil {
		t.Fatal("unknown configuration field was accepted")
	}
}

func TestNormalizeSub2APIConfigMigratesLegacyDefaultModels(t *testing.T) {
	cfg, _, err := NormalizeSub2APIConfig([]byte(`{"models":["gpt-6-astra-basispoints"],"model_mappings":{"gpt-6-astra-basispoints":"gpt-6-astra"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models) != 5 || cfg.ModelMappings[DefaultSolModelID] != DefaultSolModelID || cfg.ModelMappings[DefaultLunaModelID] != DefaultLunaModelID || cfg.ModelMappings[DefaultTerraModelID] != DefaultTerraModelID || cfg.ModelMappings[DefaultAstraModelID] != DefaultAstraModelID {
		t.Fatalf("legacy default was not migrated: %#v", cfg)
	}
}

func TestNormalizeSub2APIConfigMigratesV012DefaultModels(t *testing.T) {
	cfg, _, err := NormalizeSub2APIConfig([]byte(`{"models":["gpt-6-astra-basispoints","gpt-6-sol"],"model_mappings":{"gpt-6-astra-basispoints":"gpt-6-astra","gpt-6-sol":"gpt-6-sol"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models) != 5 {
		t.Fatalf("v0.1.2 default was not migrated: %#v", cfg)
	}
}

func TestPrepareSub2APIPayloadMapsModelAndPreservesStream(t *testing.T) {
	cfg, _, err := NormalizeSub2APIConfig([]byte(`{"models":["basis-alias"],"model_mappings":{"basis-alias":"gpt-6-astra"}}`))
	if err != nil {
		t.Fatal(err)
	}
	payload, streaming, err := PrepareSub2APIPayload([]byte(`{"model":"basis-alias","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !streaming || payload["model"] != "gpt-6-astra" {
		t.Fatalf("unexpected prepared payload: streaming=%v payload=%v", streaming, payload)
	}
	if payload["store"] != false || payload["reasoning_effort"] != "medium" {
		t.Fatalf("missing safe upstream defaults: %v", payload)
	}
}

func TestPrepareSub2APIPayloadSupportsDefaultSolModel(t *testing.T) {
	payload, streaming, err := PrepareSub2APIPayload([]byte(`{"model":"gpt-6-sol","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`), DefaultSub2APIConfig())
	if err != nil {
		t.Fatal(err)
	}
	if streaming || payload["model"] != "gpt-6-sol" {
		t.Fatalf("unexpected default Sol payload: streaming=%v payload=%v", streaming, payload)
	}
}

func TestPrepareSub2APIPayloadSupportsDirectGPT6Models(t *testing.T) {
	for _, model := range []string{DefaultLunaModelID, DefaultTerraModelID, DefaultAstraModelID} {
		raw := []byte(`{"model":"` + model + `","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
		payload, streaming, err := PrepareSub2APIPayload(raw, DefaultSub2APIConfig())
		if err != nil {
			t.Fatalf("model %s: %v", model, err)
		}
		if streaming || payload["model"] != model {
			t.Fatalf("model %s: streaming=%v payload=%v", model, streaming, payload)
		}
	}
}

func TestCredentialFromHeadersUsesAccountHeader(t *testing.T) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer access-token")
	headers.Set("chatgpt-account-id", "account-123")
	credential, err := CredentialFromHeaders(headers, "chatgpt")
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "access-token" || credential.AccountID != "account-123" || credential.AuthMode != "chatgpt" {
		t.Fatalf("unexpected credential: %#v", credential)
	}
	upstream, err := BasisPointsHeaders(headers, DefaultSub2APIConfig(), true)
	if err != nil {
		t.Fatal(err)
	}
	if upstream.Get("Authorization") != "Bearer access-token" || upstream.Get("ChatGPT-Account-ID") != "account-123" || upstream.Get("Accept") != "text/event-stream" {
		t.Fatalf("unexpected upstream headers: %#v", upstream)
	}
}

func TestPrepareSub2APIPayloadRejectsUnsupportedContinuation(t *testing.T) {
	cfg := DefaultSub2APIConfig()
	_, _, err := PrepareSub2APIPayload([]byte(`{"model":"gpt-6-astra-basispoints","previous_response_id":"resp_1","input":[]}`), cfg)
	if err == nil || !strings.Contains(err.Error(), "previous_response_id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMarshalSub2APIPayloadProducesJSON(t *testing.T) {
	encoded, err := MarshalSub2APIPayload(map[string]any{"model": "gpt-6-astra"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded["model"] != "gpt-6-astra" {
		t.Fatalf("invalid encoded payload: %s", encoded)
	}
}
