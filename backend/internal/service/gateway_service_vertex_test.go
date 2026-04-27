package service

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ----- isModelSupportedByAccount: Vertex branch -----

func TestGatewayService_IsModelSupportedByAccount_VertexDefaultMappingHit(t *testing.T) {
	svc := &GatewayService{}
	acct := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeVertex,
		Credentials: map[string]any{
			"gcp_region":     "us-east5",
			"gcp_project_id": "my-project",
		},
	}
	assert.True(t, svc.isModelSupportedByAccount(acct, "claude-sonnet-4-5"))
	assert.True(t, svc.isModelSupportedByAccount(acct, "claude-haiku-4-5"))
}

func TestGatewayService_IsModelSupportedByAccount_VertexUnknownModelRejected(t *testing.T) {
	svc := &GatewayService{}
	acct := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeVertex,
		Credentials: map[string]any{
			"gcp_region":     "us-east5",
			"gcp_project_id": "my-project",
		},
	}
	assert.False(t, svc.isModelSupportedByAccount(acct, "totally-unknown-model"))
	assert.False(t, svc.isModelSupportedByAccount(acct, "gpt-4"))
}

func TestGatewayService_IsModelSupportedByAccount_VertexAccountMappingExtendsAllowlist(t *testing.T) {
	svc := &GatewayService{}
	acct := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeVertex,
		Credentials: map[string]any{
			"gcp_region":     "us-east5",
			"gcp_project_id": "my-project",
			"model_mapping": map[string]any{
				"claude-experimental": "claude-experimental@20260101",
			},
		},
	}
	// Custom-mapped name resolves via the user-provided model_mapping
	assert.True(t, svc.isModelSupportedByAccount(acct, "claude-experimental"))
	// Default mapping still works alongside the override
	assert.True(t, svc.isModelSupportedByAccount(acct, "claude-sonnet-4-5"))
}

// ----- betaPolicyScopeMatches: vertex scope + signature change -----

func TestBetaPolicyScopeMatches_VertexScope(t *testing.T) {
	// only matches vertex accounts
	assert.True(t, betaPolicyScopeMatches(BetaPolicyScopeVertex, false, false, true))
	assert.False(t, betaPolicyScopeMatches(BetaPolicyScopeVertex, true, false, false))
	assert.False(t, betaPolicyScopeMatches(BetaPolicyScopeVertex, false, true, false))
	assert.False(t, betaPolicyScopeMatches(BetaPolicyScopeVertex, false, false, false))
}

func TestBetaPolicyScopeMatches_APIKeyScopeExcludesBedrockAndVertex(t *testing.T) {
	// apikey scope must not match bedrock or vertex (those have their own scopes)
	assert.True(t, betaPolicyScopeMatches(BetaPolicyScopeAPIKey, false, false, false))
	assert.False(t, betaPolicyScopeMatches(BetaPolicyScopeAPIKey, false, true, false))  // bedrock
	assert.False(t, betaPolicyScopeMatches(BetaPolicyScopeAPIKey, false, false, true))  // vertex
	assert.False(t, betaPolicyScopeMatches(BetaPolicyScopeAPIKey, true, false, false))  // oauth
}

func TestBetaPolicyScopeMatches_BedrockScope_UnchangedByVertexAddition(t *testing.T) {
	assert.True(t, betaPolicyScopeMatches(BetaPolicyScopeBedrock, false, true, false))
	assert.False(t, betaPolicyScopeMatches(BetaPolicyScopeBedrock, false, false, true))
	assert.False(t, betaPolicyScopeMatches(BetaPolicyScopeBedrock, true, false, false))
}

func TestBetaPolicyScopeMatches_AllScope_AlwaysMatches(t *testing.T) {
	assert.True(t, betaPolicyScopeMatches(BetaPolicyScopeAll, false, false, false))
	assert.True(t, betaPolicyScopeMatches(BetaPolicyScopeAll, true, false, false))
	assert.True(t, betaPolicyScopeMatches(BetaPolicyScopeAll, false, true, false))
	assert.True(t, betaPolicyScopeMatches(BetaPolicyScopeAll, false, false, true))
}

// ----- buildUpstreamRequestVertex: URL + headers + auth -----

func TestBuildUpstreamRequestVertex_AssemblesURLAndAuth(t *testing.T) {
	svc := &GatewayService{}
	body := []byte(`{"anthropic_version":"vertex-2023-10-16","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := svc.buildUpstreamRequestVertex(
		context.Background(),
		body,
		"my-project",
		"us-east5",
		"claude-sonnet-4-5@20250929",
		false,
		"ya29.fakeAccessToken",
	)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, req.Method)
	expectedURL := "https://us-east5-aiplatform.googleapis.com/v1/projects/my-project/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5@20250929:rawPredict"
	assert.Equal(t, expectedURL, req.URL.String())

	assert.Equal(t, "Bearer ya29.fakeAccessToken", req.Header.Get("Authorization"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.True(t, strings.HasPrefix(req.Header.Get("Accept"), "application/"), "Accept header should be JSON-ish")
}

func TestBuildUpstreamRequestVertex_GlobalRegion_NoRegionSubdomain(t *testing.T) {
	svc := &GatewayService{}
	req, err := svc.buildUpstreamRequestVertex(
		context.Background(),
		[]byte(`{"max_tokens":1}`),
		"acme",
		"global",
		"claude-opus-4-1@20250805",
		true,
		"ya29.tok",
	)
	require.NoError(t, err)
	expectedURL := "https://aiplatform.googleapis.com/v1/projects/acme/locations/global/publishers/anthropic/models/claude-opus-4-1@20250805:streamRawPredict"
	assert.Equal(t, expectedURL, req.URL.String())
}

func TestBuildUpstreamRequestVertex_PreservesBodyExactly(t *testing.T) {
	// PrepareVertexRequestBody is the only body rewriter — buildUpstreamRequestVertex
	// must not mutate the body (would otherwise corrupt prepared JSON).
	svc := &GatewayService{}
	body := []byte(`{"anthropic_version":"vertex-2023-10-16","messages":[{"role":"user","content":"x"}]}`)
	req, err := svc.buildUpstreamRequestVertex(
		context.Background(),
		body,
		"p",
		"us-east5",
		"m@1",
		false,
		"t",
	)
	require.NoError(t, err)
	require.NotNil(t, req.Body)

	// Read body back out and assert equality
	var sentBody []byte
	if req.Body != nil {
		buf := make([]byte, 1024)
		n, _ := req.Body.Read(buf)
		sentBody = buf[:n]
	}
	assert.Equal(t, "vertex-2023-10-16", gjson.GetBytes(sentBody, "anthropic_version").String())
	assert.Equal(t, "x", gjson.GetBytes(sentBody, "messages.0.content").String())
}
