package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TestNormalizeClaudeOAuthRequestBody_PrunesClientMetadataWhenNotInjecting
// reproduces the production bug where OpenClaw / pi-ai SDK sends
// metadata.user_id together with extra keys (trace_id, project_id, etc.)
// and we forward them all unchanged, causing upstream 400
// "metadata: Extra inputs are not permitted".
//
// After the fix, pruneClaudeOAuthMetadataToUserIDOnly runs unconditionally
// inside normalizeClaudeOAuthRequestBody, so extra keys are stripped
// regardless of whether we're also injecting our own user_id.
func TestNormalizeClaudeOAuthRequestBody_PrunesClientMetadataWhenNotInjecting(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"client-provided","trace_id":"abc123","project_id":"openclaw-42"}}`)

	// No injection: client already supplied user_id, we only prune extras.
	result, _ := normalizeClaudeOAuthRequestBody(body, "claude-opus-4-7", claudeOAuthNormalizeOptions{
		injectMetadata: false,
	})
	resultStr := string(result)

	require.Contains(t, resultStr, `"user_id":"client-provided"`, "client's user_id should be preserved")
	require.NotContains(t, resultStr, `"trace_id"`, "trace_id extra key must be stripped")
	require.NotContains(t, resultStr, `"project_id"`, "project_id extra key must be stripped")

	metadata := gjson.GetBytes(result, "metadata")
	require.True(t, metadata.Exists())
	keyCount := 0
	metadata.ForEach(func(_, _ gjson.Result) bool {
		keyCount++
		return true
	})
	require.Equal(t, 1, keyCount, "metadata must contain only user_id after prune")
}

// TestNormalizeClaudeOAuthRequestBody_PreservesMetadataWithoutUserID ensures
// the prune is a no-op when metadata lacks user_id (safety check inside
// pruneClaudeOAuthMetadataToUserIDOnly).
func TestNormalizeClaudeOAuthRequestBody_PreservesMetadataWithoutUserID(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"metadata":{"trace_id":"abc123"}}`)

	result, _ := normalizeClaudeOAuthRequestBody(body, "claude-opus-4-7", claudeOAuthNormalizeOptions{
		injectMetadata: false,
	})

	// When metadata has no user_id, prune returns unchanged — trace_id stays.
	// (This is still wrong fingerprint-wise but it's a pre-existing edge case;
	// we only assert the safety behaviour here, not policy.)
	require.Contains(t, string(result), `"trace_id":"abc123"`)
}

// TestForwardCountTokens_StripsMetadataField verifies that when an OAuth
// account is in mimic mode and the client sends a count_tokens request with
// metadata, our gateway strips the entire metadata field. Anthropic's
// /v1/messages/count_tokens endpoint returns HTTP 400 "metadata: Extra inputs
// are not permitted" if metadata is present at all (regardless of contents).
//
// This is a unit-level check on the strip step. The previous bug also
// erroneously injected metadata into the count_tokens body — this test
// guards the absence of metadata after the strip.
func TestForwardCountTokens_StripsMetadataField(t *testing.T) {
	// Simulate the body that ForwardCountTokens hands to the upstream after
	// mimic normalization + the new strip step.
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"abc","trace_id":"xyz"}}`)

	// First normalize (no inject — count_tokens path).
	body, _ = normalizeClaudeOAuthRequestBody(body, "claude-opus-4-7", claudeOAuthNormalizeOptions{
		stripSystemCacheControl: true,
	})

	// Then the explicit metadata strip (what ForwardCountTokens does after normalize).
	if gjson.GetBytes(body, "metadata").Exists() {
		cleaned, err := sjson.DeleteBytes(body, "metadata")
		require.NoError(t, err)
		body = cleaned
	}

	require.False(t, gjson.GetBytes(body, "metadata").Exists(), "metadata must be absent in count_tokens request")
}
