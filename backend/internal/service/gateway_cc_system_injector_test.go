package service

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
)

// TestInjectClaudeCodeSystemBlocks_StructureAndOrder verifies that the
// injector produces exactly three text blocks in the canonical Claude Code
// order: billing header, agent identifier, static core prompt.
func TestInjectClaudeCodeSystemBlocks_StructureAndOrder(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-6",
		"system": [{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}}],
		"messages": [{"role":"user","content":"hey"}]
	}`)

	out := injectClaudeCodeSystemBlocks(body, "2.1.116", "cli")

	sys := gjson.GetBytes(out, "system")
	if !sys.IsArray() {
		t.Fatalf("system is not an array: %s", sys.Raw)
	}
	arr := sys.Array()
	if len(arr) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(arr))
	}

	first := arr[0].Get("text").String()
	if !strings.HasPrefix(first, "x-anthropic-billing-header:") {
		t.Errorf("block[0] is not a billing header: %q", first)
	}
	if !strings.Contains(first, "cc_version=2.1.116.") {
		t.Errorf("billing header missing cc_version.suffix: %q", first)
	}
	if !strings.Contains(first, "cc_entrypoint=cli;") {
		t.Errorf("billing header missing entrypoint: %q", first)
	}
	if !strings.Contains(first, "cch=") {
		t.Errorf("billing header missing cch: %q", first)
	}

	second := arr[1].Get("text").String()
	if second != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Errorf("block[1] is not the agent identifier: %q", second)
	}

	third := arr[2].Get("text").String()
	for _, marker := range []string{"# System", "# Doing tasks", "# Tone and style", "# Output efficiency"} {
		if !strings.Contains(third, marker) {
			t.Errorf("block[2] missing section header %q", marker)
		}
	}

	// None of the injected blocks should carry cache_control — that matches
	// real Claude Code's "no cache on prefix" behavior.
	for i, blk := range arr {
		if blk.Get("cache_control").Exists() {
			t.Errorf("block[%d] unexpectedly has cache_control; real Claude Code doesn't cache the prefix", i)
		}
	}
}

// TestInjectClaudeCodeSystemBlocks_FingerprintDeterministic verifies that
// the same input produces the same billing header output (deterministic
// fingerprint and cch).
func TestInjectClaudeCodeSystemBlocks_FingerprintDeterministic(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-6",
		"system": [{"type":"text","text":"stub"}],
		"messages": [{"role":"user","content":"hey"}]
	}`)

	a := injectClaudeCodeSystemBlocks(body, "2.1.116", "cli")
	b := injectClaudeCodeSystemBlocks(body, "2.1.116", "cli")

	aFirst := gjson.GetBytes(a, "system.0.text").String()
	bFirst := gjson.GetBytes(b, "system.0.text").String()
	if aFirst != bFirst {
		t.Errorf("fingerprint not deterministic:\n a=%q\n b=%q", aFirst, bFirst)
	}
}

// TestInjectClaudeCodeSystemBlocks_EmptyVersionBailsOut verifies we don't
// produce a malformed header when the version cannot be resolved.
func TestInjectClaudeCodeSystemBlocks_EmptyVersionBailsOut(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"original"}],"messages":[]}`)
	out := injectClaudeCodeSystemBlocks(body, "", "cli")

	// Body unchanged — original single-block system preserved.
	if string(out) != string(body) {
		t.Errorf("expected body unchanged when version is empty, got diff")
	}
}

// TestHasInjectedBillingHeader checks the guard used to avoid double-injection.
func TestHasInjectedBillingHeader(t *testing.T) {
	with := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.0;"}]}`)
	without := []byte(`{"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}]}`)

	if !hasInjectedBillingHeader(with) {
		t.Errorf("expected true for body with billing header")
	}
	if hasInjectedBillingHeader(without) {
		t.Errorf("expected false for body without billing header")
	}
}

// TestMaybeInjectClaudeCodeSystemBlocks_FlagOffIsNoOp verifies the safety
// guarantee: with the feature flag disabled, body passes through unchanged.
// This is the property that lets us ship the code without risk.
func TestMaybeInjectClaudeCodeSystemBlocks_FlagOffIsNoOp(t *testing.T) {
	s := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{InjectCCSystemBlocks: false},
		},
	}
	body := []byte(`{"system":[{"type":"text","text":"seed"}],"messages":[{"role":"user","content":"hey"}]}`)

	out := s.maybeInjectClaudeCodeSystemBlocks(body)
	if string(out) != string(body) {
		t.Errorf("expected body unchanged when flag is off, got diff:\n  in:  %s\n  out: %s", body, out)
	}
}

// TestMaybeInjectClaudeCodeSystemBlocks_FlagOnInjects verifies the flag-on
// path produces a 3-block system.
func TestMaybeInjectClaudeCodeSystemBlocks_FlagOnInjects(t *testing.T) {
	s := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{InjectCCSystemBlocks: true},
		},
	}
	body := []byte(`{"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[{"role":"user","content":"hey"}]}`)

	out := s.maybeInjectClaudeCodeSystemBlocks(body)
	sys := gjson.GetBytes(out, "system").Array()
	if len(sys) != 3 {
		t.Fatalf("expected 3 blocks when flag on, got %d", len(sys))
	}
}

// TestMaybeInjectClaudeCodeSystemBlocks_SkipIfAlreadyHasBillingHeader
// prevents double-injection when the caller is the rare client that already
// sent one (real Claude Code).
func TestMaybeInjectClaudeCodeSystemBlocks_SkipIfAlreadyHasBillingHeader(t *testing.T) {
	s := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{InjectCCSystemBlocks: true},
		},
	}
	body := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.0; cc_entrypoint=cli; cch=abcde;"}],"messages":[{"role":"user","content":"hey"}]}`)

	out := s.maybeInjectClaudeCodeSystemBlocks(body)
	if string(out) != string(body) {
		t.Errorf("expected no-op when billing header already present")
	}
}
