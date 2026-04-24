package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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
