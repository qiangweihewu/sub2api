package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpstreamPassthroughPolicyRuntimeDisabled_NoPolicyAttached(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "")
	resetUpstreamPassthroughDefaultsCache()
	resetUpstreamPassthroughGlobalOverrideCache()

	repo := &fakeSettingRepoForUpstream{values: map[string]string{
		SettingKeyUpstreamPolicyV1Enabled: "true",
	}}
	svc := NewSettingService(repo, &config.Config{})
	account := &Account{Type: AccountTypeAPIKey, Platform: PlatformAnthropic}

	ctx := ResolveAndStorePolicy(context.Background(), account, svc)
	policy, ok := GetUpstreamPolicyFromContext(ctx)

	require.False(t, ok)
	require.Nil(t, policy)
}

func TestUpstreamPassthroughPolicyRuntimeDisabled_HelperLegacyBehavior(t *testing.T) {
	ctx := context.Background()

	require.True(t, ShouldScrubBody(ctx))
	require.True(t, ShouldInjectSystemPrompt(ctx))
	require.False(t, ShouldForwardClientHeaders(ctx))
	require.False(t, ShouldForwardUserNetworkInfo(ctx))
	require.False(t, ShouldSkipModelRewrite(ctx))
	require.False(t, ShouldForwardBetaFlags(ctx))
	require.False(t, ShouldForwardClientUA(ctx))

	whitelist := func(key string) bool { return key == "user-agent" }
	require.True(t, ShouldCopyClientHeader(ctx, "user-agent", whitelist))
	require.False(t, ShouldCopyClientHeader(ctx, "x-custom", whitelist))

	upstreamHeaders := http.Header{}
	clientHeaders := http.Header{"X-Forwarded-For": []string{"1.2.3.4"}}
	ForwardUserNetworkInfoHeaders(ctx, upstreamHeaders, clientHeaders)
	require.Empty(t, upstreamHeaders)
}

func TestUpstreamPassthroughPolicyRuntimeDisabled_PassthroughUsesLegacyFields(t *testing.T) {
	ctx := context.Background()

	anthropic := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"anthropic_passthrough": false},
	}
	require.False(t, anthropic.IsAnthropicAPIKeyPassthroughEnabledWithContext(ctx))
	anthropic.Extra["anthropic_passthrough"] = true
	require.True(t, anthropic.IsAnthropicAPIKeyPassthroughEnabledWithContext(ctx))

	openai := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"openai_passthrough": false},
	}
	require.False(t, openai.IsOpenAIPassthroughEnabledWithContext(ctx))
	openai.Extra["openai_passthrough"] = true
	require.True(t, openai.IsOpenAIPassthroughEnabledWithContext(ctx))
}

func TestUpstreamPassthroughPolicyRuntimeDisabled_ModelMappingStillApplies(t *testing.T) {
	account := &Account{
		Credentials: map[string]any{"model_mapping": map[string]any{"claude-3-7-sonnet-20250219": "claude-3-haiku-20240307"}},
	}
	ctx := context.Background()

	require.Equal(t, "claude-3-haiku-20240307", account.GetMappedModelForUpstream(ctx, "claude-3-7-sonnet-20250219"))
}

func newPolicyRuntimeSettings(t *testing.T, enabled bool) *SettingService {
	t.Helper()
	resetUpstreamPassthroughDefaultsCache()
	resetUpstreamPassthroughGlobalOverrideCache()
	t.Cleanup(resetUpstreamPassthroughDefaultsCache)
	t.Cleanup(resetUpstreamPassthroughGlobalOverrideCache)
	value := "false"
	if enabled {
		value = "true"
	}
	return NewSettingService(&fakeSettingRepoForUpstream{values: map[string]string{
		SettingKeyUpstreamPolicyV1Enabled: value,
	}}, &config.Config{})
}

func TestUpstreamPolicyRuntime_RequiresBothOptInsAndEligibleAccount(t *testing.T) {
	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	for _, env := range []string{"", "false", "invalid", "true"} {
		for _, flag := range []bool{false, true} {
			t.Run(env+"/"+map[bool]string{false: "off", true: "on"}[flag], func(t *testing.T) {
				t.Setenv(upstreamPolicyRuntimeEnv, env)
				ctx := ResolveAndStorePolicy(context.Background(), account, newPolicyRuntimeSettings(t, flag))
				_, ok := GetUpstreamPolicyFromContext(ctx)
				require.Equal(t, env == "true" && flag, ok)
			})
		}
	}
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	svc := newPolicyRuntimeSettings(t, true)
	for _, excluded := range []*Account{
		nil,
		{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		{Platform: PlatformAnthropic, Type: AccountTypeSetupToken},
		{Platform: PlatformAnthropic, Type: AccountTypeServiceAccount},
		{Platform: PlatformAnthropic, Type: AccountTypeBedrock},
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
	} {
		ctx := ResolveAndStorePolicy(context.Background(), excluded, svc)
		_, ok := GetUpstreamPolicyFromContext(ctx)
		require.False(t, ok)
	}
}

func TestUpstreamPolicyRuntime_ResolvesPrecedenceAndClearsRetryContext(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	svc := newPolicyRuntimeSettings(t, true)
	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Extra: map[string]any{"anthropic_passthrough": true}}
	group := &Group{ID: 1, Hydrated: true, Platform: PlatformAnthropic, Status: StatusActive, DefaultPassthroughProfile: string(ProfileProtected)}
	base := context.WithValue(context.Background(), ctxkey.Group, group)
	ctx := ResolveAndStorePolicy(base, account, svc)
	policy, ok := GetUpstreamPolicyFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, ProfileProtected, policy.ProfileApplied)
	require.False(t, policy.ForwardClientHeaders, "group profile must beat legacy passthrough toggles")
	require.False(t, account.IsAnthropicAPIKeyPassthroughEnabledWithContext(ctx))

	account.Extra["upstream_passthrough"] = map[string]any{"profile": "transparent"}
	ctx = ResolveAndStorePolicy(ctx, account, svc)
	policy, ok = GetUpstreamPolicyFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, ProfileTransparent, policy.ProfileApplied)
	require.True(t, account.IsAnthropicAPIKeyPassthroughEnabledWithContext(ctx))
	require.False(t, ShouldScrubBody(ctx))
	require.False(t, ShouldInjectSystemPrompt(ctx))
	require.True(t, ShouldForwardClientUA(ctx))
	require.True(t, ShouldForwardBetaFlags(ctx))
	account.Credentials = map[string]any{"model_mapping": map[string]any{"alias": "mapped"}}
	require.Equal(t, "alias", account.GetMappedModelForUpstream(ctx, "alias"))
	require.Equal(t, "mapped", account.GetMappedModel("alias"), "billing mapping stays independent")

	// The same request can retry through a different account/protocol.
	ctx = ResolveAndStorePolicy(ctx, &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, svc)
	_, ok = GetUpstreamPolicyFromContext(ctx)
	require.False(t, ok)
	require.True(t, ShouldScrubBody(ctx))
}

func TestUpstreamPolicyRuntime_TransparentRequestContract(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	account := newAnthropicAPIKeyAccountForTest()
	account.Extra = map[string]any{"upstream_passthrough": map[string]any{
		"profile": "transparent", "overrides": map[string]any{"forward_user_network_info": false},
	}}
	ctx := ResolveAndStorePolicy(context.Background(), account, newPolicyRuntimeSettings(t, true))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header = http.Header{
		"Authorization": {"Bearer inbound-secret"}, "X-Api-Key": {"inbound-key"},
		"Cookie": {"session=secret"}, "Proxy-Authorization": {"Basic proxy-secret"},
		"X-Custom-Token": {"private-token"}, "X-Custom-Feature": {"keep"},
		"Connection": {"X-Hop-Private, X-Forwarded-For"}, "X-Hop-Private": {"drop"},
		"X-Forwarded-For": {"192.0.2.1"}, "Content-Length": {"999"},
		"User-Agent": {"my-client/1.0"}, "Anthropic-Beta": {"future-compatibility-beta"},
	}
	body := []byte(`{"model":"alias", "system":[{"type":"text","text":"user-owned system","cache_control":{"type":"ephemeral","ttl":"5m"}}],"tools":[{"name":"z_tool","input_schema":{"type":"object"}},{"name":"a_tool","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"client-session"}}`)
	svc := &GatewayService{cfg: &config.Config{}}
	for _, countTokens := range []bool{false, true} {
		var req *http.Request
		var err error
		if countTokens {
			req, err = svc.buildCountTokensRequestAnthropicAPIKeyPassthrough(ctx, c, account, body, "upstream-key")
		} else {
			req, _, err = svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(ctx, c, account, body, "upstream-key")
		}
		require.NoError(t, err)
		out, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Equal(t, string(body), string(out))
		require.Equal(t, "my-client/1.0", getHeaderRaw(req.Header, "user-agent"))
		require.Equal(t, "future-compatibility-beta", getHeaderRaw(req.Header, "anthropic-beta"))
		require.Equal(t, "upstream-key", getHeaderRaw(req.Header, "x-api-key"))
		require.Equal(t, "keep", getHeaderRaw(req.Header, "x-custom-feature"))
		for key, values := range req.Header {
			require.NotContains(t, strings.Join(values, ","), "inbound", key)
		}
		for _, key := range []string{"cookie", "proxy-authorization", "x-custom-token", "connection", "x-hop-private", "x-forwarded-for", "content-length"} {
			require.Empty(t, getHeaderRaw(req.Header, key), key)
		}
	}
}
