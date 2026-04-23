//go:build unit

package service

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestStripThirdPartyBodyFields_RemovesServiceTier(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","service_tier":"auto","messages":[]}`)
	out, changed := stripThirdPartyBodyFields(body)
	if !changed {
		t.Fatalf("expected changed=true when service_tier present")
	}
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("service_tier should have been removed: %s", out)
	}
	if gjson.GetBytes(out, "model").String() != "claude-opus-4-7" {
		t.Fatalf("other fields should survive: %s", out)
	}
}

func TestStripThirdPartyBodyFields_NoChangeWhenAbsent(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[],"max_tokens":1000}`)
	out, changed := stripThirdPartyBodyFields(body)
	if changed {
		t.Fatalf("expected changed=false when no third-party fields present")
	}
	if string(out) != string(body) {
		t.Fatalf("body should be returned unchanged: got %s", out)
	}
}

func TestStripThirdPartyBodyFields_EmptyBody(t *testing.T) {
	out, changed := stripThirdPartyBodyFields(nil)
	if changed {
		t.Fatalf("empty body must not report changed")
	}
	if out != nil {
		t.Fatalf("nil body must round-trip as nil")
	}

	out2, changed2 := stripThirdPartyBodyFields([]byte{})
	if changed2 {
		t.Fatalf("empty byte slice must not report changed")
	}
	if len(out2) != 0 {
		t.Fatalf("empty byte slice must round-trip empty")
	}
}

func TestStripThirdPartyBodyFields_ServiceTierStandardOnly(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-6","service_tier":"standard_only"}`)
	out, changed := stripThirdPartyBodyFields(body)
	if !changed {
		t.Fatalf("service_tier:\"standard_only\" should also be stripped")
	}
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Fatalf("service_tier should have been removed: %s", out)
	}
}

func TestStripThirdPartyBodyFields_PreservesBodyStructure(t *testing.T) {
	// Realistic body: messages, tools, system, etc. — only service_tier should change.
	body := []byte(`{
		"model": "claude-opus-4-6",
		"service_tier": "auto",
		"max_tokens": 8000,
		"stream": true,
		"system": [{"type": "text", "text": "You are Claude."}],
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"name": "read", "description": "read file"}]
	}`)
	out, changed := stripThirdPartyBodyFields(body)
	if !changed {
		t.Fatalf("expected changed")
	}
	// Positive checks: these fields must still be intact
	for _, key := range []string{"model", "max_tokens", "stream", "system", "messages", "tools"} {
		if !gjson.GetBytes(out, key).Exists() {
			t.Errorf("field %q should still be present after strip: %s", key, out)
		}
	}
	// Negative check
	if gjson.GetBytes(out, "service_tier").Exists() {
		t.Errorf("service_tier should have been removed: %s", out)
	}
}

func TestStripThirdPartyBodyFields_InvalidJSON(t *testing.T) {
	// sjson.DeleteBytes on malformed input should return error → we keep body as-is.
	body := []byte(`{not json`)
	out, _ := stripThirdPartyBodyFields(body)
	// The changed flag may be true or false depending on gjson's leniency;
	// the important invariant is we don't panic and return something sensible.
	_ = out
}
