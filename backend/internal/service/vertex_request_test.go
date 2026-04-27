package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ----- BuildVertexURL -----

func TestBuildVertexURL_Regional_NonStream(t *testing.T) {
	got := BuildVertexURL("my-project", "us-east5", "claude-sonnet-4-5@20250929", false)
	want := "https://us-east5-aiplatform.googleapis.com/v1/projects/my-project/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5@20250929:rawPredict"
	assert.Equal(t, want, got)
}

func TestBuildVertexURL_Regional_Stream(t *testing.T) {
	got := BuildVertexURL("acme", "europe-west4", "claude-opus-4-1@20250805", true)
	want := "https://europe-west4-aiplatform.googleapis.com/v1/projects/acme/locations/europe-west4/publishers/anthropic/models/claude-opus-4-1@20250805:streamRawPredict"
	assert.Equal(t, want, got)
}

func TestBuildVertexURL_Global(t *testing.T) {
	// global region uses no region subdomain — host is plain aiplatform.googleapis.com
	got := BuildVertexURL("my-project", "global", "claude-sonnet-4-5@20250929", false)
	want := "https://aiplatform.googleapis.com/v1/projects/my-project/locations/global/publishers/anthropic/models/claude-sonnet-4-5@20250929:rawPredict"
	assert.Equal(t, want, got)
}

func TestBuildVertexURL_Global_Stream(t *testing.T) {
	got := BuildVertexURL("my-project", "global", "claude-opus-4-1@20250805", true)
	want := "https://aiplatform.googleapis.com/v1/projects/my-project/locations/global/publishers/anthropic/models/claude-opus-4-1@20250805:streamRawPredict"
	assert.Equal(t, want, got)
}

// ----- ResolveVertexModelID -----

func TestResolveVertexModelID_DefaultMappingHit(t *testing.T) {
	acct := &Account{Type: AccountTypeVertex, Platform: PlatformAnthropic}
	got, ok := ResolveVertexModelID(acct, "claude-sonnet-4-5")
	require.True(t, ok)
	assert.Equal(t, "claude-sonnet-4-5@20250929", got)
}

func TestResolveVertexModelID_AlreadyResolvedPassesThrough(t *testing.T) {
	// Model IDs already containing '@' (Vertex form) should pass through unchanged
	acct := &Account{Type: AccountTypeVertex, Platform: PlatformAnthropic}
	got, ok := ResolveVertexModelID(acct, "claude-haiku-4-5@20251001")
	require.True(t, ok)
	assert.Equal(t, "claude-haiku-4-5@20251001", got)
}

func TestResolveVertexModelID_AccountMappingWinsOverDefault(t *testing.T) {
	acct := &Account{
		Type:     AccountTypeVertex,
		Platform: PlatformAnthropic,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-sonnet-4-5": "claude-sonnet-4-5@20250929-custom",
			},
		},
	}
	got, ok := ResolveVertexModelID(acct, "claude-sonnet-4-5")
	require.True(t, ok)
	assert.Equal(t, "claude-sonnet-4-5@20250929-custom", got)
}

func TestResolveVertexModelID_UnknownModelReturnsFalse(t *testing.T) {
	acct := &Account{Type: AccountTypeVertex, Platform: PlatformAnthropic}
	_, ok := ResolveVertexModelID(acct, "totally-unknown-model")
	assert.False(t, ok)
}

func TestResolveVertexModelID_NilAccountReturnsFalse(t *testing.T) {
	_, ok := ResolveVertexModelID(nil, "claude-sonnet-4-5")
	assert.False(t, ok)
}

// ----- PrepareVertexRequestBody -----

func TestPrepareVertexRequestBody_InjectsAnthropicVersion(t *testing.T) {
	input := `{"model":"claude-opus-4-1","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
	got, err := PrepareVertexRequestBody([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, "vertex-2023-10-16", gjson.GetBytes(got, "anthropic_version").String())
}

func TestPrepareVertexRequestBody_StripsModelAndStream(t *testing.T) {
	input := `{"model":"claude-opus-4-1","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
	got, err := PrepareVertexRequestBody([]byte(input))
	require.NoError(t, err)
	assert.False(t, gjson.GetBytes(got, "model").Exists(), "model field must be stripped (Vertex puts it in URL)")
	assert.False(t, gjson.GetBytes(got, "stream").Exists(), "stream field must be stripped (Vertex selects via endpoint)")
}

func TestPrepareVertexRequestBody_PreservesOtherFields(t *testing.T) {
	input := `{"model":"x","max_tokens":1024,"temperature":0.7,"system":"you are helpful","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"t","description":"d","input_schema":{"type":"object"}}],"thinking":{"type":"enabled","budget_tokens":1024}}`
	got, err := PrepareVertexRequestBody([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, int64(1024), gjson.GetBytes(got, "max_tokens").Int())
	assert.InDelta(t, 0.7, gjson.GetBytes(got, "temperature").Float(), 1e-9)
	assert.Equal(t, "you are helpful", gjson.GetBytes(got, "system").String())
	assert.Len(t, gjson.GetBytes(got, "messages").Array(), 1)
	assert.Len(t, gjson.GetBytes(got, "tools").Array(), 1)
	assert.Equal(t, "enabled", gjson.GetBytes(got, "thinking.type").String())
}

func TestPrepareVertexRequestBody_IdempotentOnAlreadyVertexBody(t *testing.T) {
	// If a body already has anthropic_version=vertex-2023-10-16 and no model/stream, should not break
	input := `{"anthropic_version":"vertex-2023-10-16","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`
	got, err := PrepareVertexRequestBody([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, "vertex-2023-10-16", gjson.GetBytes(got, "anthropic_version").String())
	assert.False(t, gjson.GetBytes(got, "model").Exists())
	assert.False(t, gjson.GetBytes(got, "stream").Exists())
	assert.Equal(t, int64(256), gjson.GetBytes(got, "max_tokens").Int())
}

// ----- Account credential getters -----

func TestVertexProjectID_FromCredentials(t *testing.T) {
	acct := &Account{
		Type:        AccountTypeVertex,
		Platform:    PlatformAnthropic,
		Credentials: map[string]any{"gcp_project_id": "my-project-123"},
	}
	assert.Equal(t, "my-project-123", vertexProjectID(acct))
}

func TestVertexProjectID_NilAccount(t *testing.T) {
	assert.Equal(t, "", vertexProjectID(nil))
}

func TestVertexRegion_FromCredentials(t *testing.T) {
	acct := &Account{
		Type:        AccountTypeVertex,
		Platform:    PlatformAnthropic,
		Credentials: map[string]any{"gcp_region": "europe-west4"},
	}
	assert.Equal(t, "europe-west4", vertexRegion(acct))
}

func TestVertexRegion_DefaultEmpty(t *testing.T) {
	// No default region: caller must validate. Vertex regions vary by model availability,
	// so silently defaulting risks silent 404s on model-region mismatch.
	acct := &Account{Type: AccountTypeVertex, Platform: PlatformAnthropic}
	assert.Equal(t, "", vertexRegion(acct))
}
