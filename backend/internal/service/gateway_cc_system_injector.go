package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// gateway_cc_system_injector.go
//
// "Nuclear option" Claude Code system-block injector. Disabled by default
// (cfg.Gateway.InjectCCSystemBlocks). When enabled, rewrites the `system`
// field of OAuth mimic requests into the 3-block structure real Claude Code
// produces on the wire:
//
//	system[0]: x-anthropic-billing-header text block (no cache_control)
//	system[1]: "You are Claude Code, Anthropic's official CLI for Claude."
//	           (no cache_control)
//	system[2]: ClaudeCodeIntro + System + DoingTasks + ToneAndStyle +
//	           OutputEfficiency concatenation (no cache_control)
//
// Ported logic borrows from CLIProxyAPI (internal/runtime/executor/
// claude_executor.go:checkSystemInstructionsWithSigningMode). Semantically
// equivalent; reuses sub2api's existing computeClaudeFingerprint /
// extractFirstUserMessageTextForFingerprint so fingerprint algorithm stays
// single-sourced.
//
// Why a feature flag and not default-on:
//   - At enable time (2026-04) production is on 4 consecutive days of zero
//     "Third-party apps now draw from your extra usage…" 400s. The current
//     single-block mimic + scrub is sufficient against the deployed
//     detector. Shipping the 3-block rewrite as default risks new 400s
//     (e.g. Anthropic reserving the "x-anthropic-billing-header" token in
//     system content) for zero measurable win.
//   - Flipping the flag is the "break glass" response if Anthropic tightens
//     detection and Third-party 400s reappear. Keep the code warm and
//     tested; don't burn it in when the stable path works.

// injectClaudeCodeSystemBlocks rewrites body.system to the 3-block Claude
// Code structure and returns the mutated body. Callers MUST have already
// decided that injection is desired (feature flag + OAuth mimic path +
// non-haiku); this function does no gating and unconditionally performs
// the transformation when invoked.
//
// entrypoint is the cc_entrypoint value; real Claude Code sends "cli" for
// terminal and "vscode" for the VSCode extension. Default "cli" if empty.
func injectClaudeCodeSystemBlocks(body []byte, version, entrypoint string) []byte {
	if len(body) == 0 {
		return body
	}
	if strings.TrimSpace(version) == "" {
		// Without a real CC version we can't produce a plausible cc_version
		// suffix. Better to bail than ship "cc_version=." to upstream.
		return body
	}
	if entrypoint == "" {
		entrypoint = "cli"
	}

	// Compute billing header fingerprint from the first user message, matching
	// Claude Code's real algorithm. This must happen BEFORE we blow away the
	// original system array — but since computeClaudeFingerprint reads from
	// `messages`, not `system`, ordering with respect to the system rewrite
	// below is safe either way. We compute first for clarity.
	msgText := extractFirstUserMessageTextForFingerprint(body)
	fp := computeClaudeFingerprint(msgText, version)

	// Build the three blocks. No cache_control on any of them — that matches
	// real Claude Code's prefix behavior (cache_control appears on later
	// runtime blocks, not on the static prefix). See CLIProxyAPI note at
	// claude_executor.go:1556.
	billingText := buildBillingHeaderText(body, version, fp, entrypoint)

	blocks := []map[string]any{
		{"type": "text", "text": billingText},
		{"type": "text", "text": claude.ClaudeCodeAgentIdentifier},
		{"type": "text", "text": claude.StaticSystemCorePrompt()},
	}

	raw, err := json.Marshal(blocks)
	if err != nil {
		return body
	}
	out, err := sjson.SetRawBytes(body, "system", raw)
	if err != nil {
		return body
	}
	return out
}

// buildBillingHeaderText produces the literal text for system[0]. Format:
//
//	x-anthropic-billing-header: cc_version=<ver>.<fp3>; cc_entrypoint=<ep>; cch=<hash5>;
//
// cch is the first 5 hex chars of SHA-256 over the serialized payload. We
// compute it over the CURRENT body (before injection) so the hash reflects
// the semantic payload the upstream will see for this request. This matches
// CLIProxyAPI's non-experimental path (claude_executor.go:1511-1513).
//
// When fp is empty we still emit cc_version=<ver>. (no suffix) — this degrades
// gracefully rather than producing a malformed header.
func buildBillingHeaderText(payload []byte, version, fp, entrypoint string) string {
	ver := version
	if fp != "" {
		ver = version + "." + fp
	}
	h := sha256.Sum256(payload)
	cch := hex.EncodeToString(h[:])[:5]
	return fmt.Sprintf(
		"x-anthropic-billing-header: cc_version=%s; cc_entrypoint=%s; cch=%s;",
		ver, entrypoint, cch,
	)
}

// hasInjectedBillingHeader reports whether body.system[0] already looks like
// our injected billing header (or a real Claude Code one). Callers can skip
// re-injection to avoid stacking multiple billing blocks when the request
// already carries one.
func hasInjectedBillingHeader(body []byte) bool {
	first := gjson.GetBytes(body, "system.0.text").String()
	return strings.HasPrefix(first, "x-anthropic-billing-header:")
}

// maybeInjectClaudeCodeSystemBlocks performs the feature-flagged 3-block
// system rewrite for mimic-path requests. Returns body unchanged when the
// flag is off or prerequisites are missing.
//
// Callers should invoke this right after rewriteSystemForNonClaudeCode
// (which moves client system text into messages) and before ScrubThirdPartyBody
// (scrub is idempotent over verbatim Claude Code text since it doesn't
// contain any sensitive words / orchestration markers).
func (s *GatewayService) maybeInjectClaudeCodeSystemBlocks(body []byte) []byte {
	if s == nil || s.cfg == nil {
		return body
	}
	if !s.cfg.Gateway.InjectCCSystemBlocks {
		return body
	}
	if hasInjectedBillingHeader(body) {
		// Upstream client (e.g. real Claude Code) already carries a billing
		// header. Our single-source signBillingHeaderCCH will fix up cc_version
		// and cch downstream in buildUpstreamRequest. Do not double-inject.
		return body
	}
	version := ExtractCLIVersion(claude.DefaultHeaders["User-Agent"])
	if version == "" {
		return body
	}
	return injectClaudeCodeSystemBlocks(body, version, "cli")
}
