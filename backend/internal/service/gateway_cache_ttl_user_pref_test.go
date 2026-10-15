package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestResolveCacheTTLUsageOverride_UserPrefOutranksAccountOverride ensures the new
// user-level preference (C2) sits at the top of the priority chain. Without this
// behaviour, customers selecting "cost_priority" would be silently overridden by an
// operator-set account TTL — defeating the self-service promise of Workstream C.
func TestResolveCacheTTLUsageOverride_UserPrefOutranksAccountOverride(t *testing.T) {
	svc := &GatewayService{settingService: NewSettingService(&gatewayTTLSettingRepo{}, &config.Config{})}
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"cache_ttl_override_enabled": true,
			"cache_ttl_override_target":  "1h", // account says 1h
		},
	}
	apiKey := &APIKey{CacheStrategy: CacheStrategyCostPriority} // user says 5m

	target, ok, trace := svc.resolveCacheTTLUsageOverride(context.Background(), account, apiKey)
	require.True(t, ok)
	require.Equal(t, "5m", target, "user pref must win over account override")
	require.Equal(t, "user_pref:5m", trace)
}

func TestResolveCacheTTLUsageOverride_UserPrefLatencyForces1h(t *testing.T) {
	svc := &GatewayService{settingService: NewSettingService(&gatewayTTLSettingRepo{}, &config.Config{})}
	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	apiKey := &APIKey{CacheStrategy: CacheStrategyLatencyPriority}

	target, ok, trace := svc.resolveCacheTTLUsageOverride(context.Background(), account, apiKey)
	require.True(t, ok)
	require.Equal(t, "1h", target)
	require.Equal(t, "user_pref:1h", trace)
}

// TestResolveCacheTTLUsageOverride_UserPrefIgnoredOnUnsupportedAccount documents the
// capability gate: a user selecting a strategy bound to an OpenAI account must NOT
// silently take effect (would mis-classify usage for a platform that has no
// Anthropic cache semantics). The trace value is the smoking gun for ops debugging
// "why didn't my setting work?" tickets.
func TestResolveCacheTTLUsageOverride_UserPrefIgnoredOnUnsupportedAccount(t *testing.T) {
	svc := &GatewayService{settingService: NewSettingService(&gatewayTTLSettingRepo{}, &config.Config{})}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKey := &APIKey{CacheStrategy: CacheStrategyCostPriority}

	target, ok, trace := svc.resolveCacheTTLUsageOverride(context.Background(), account, apiKey)
	require.False(t, ok)
	require.Empty(t, target)
	require.Equal(t, "user_pref_ignored:account_unsupported", trace)
}

func TestResolveCacheTTLUsageOverride_AutoFallsThroughToAccount(t *testing.T) {
	svc := &GatewayService{settingService: NewSettingService(&gatewayTTLSettingRepo{}, &config.Config{})}
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"cache_ttl_override_enabled": true,
			"cache_ttl_override_target":  "1h",
		},
	}
	apiKey := &APIKey{CacheStrategy: CacheStrategyAuto}

	target, ok, trace := svc.resolveCacheTTLUsageOverride(context.Background(), account, apiKey)
	require.True(t, ok)
	require.Equal(t, "1h", target)
	require.Equal(t, "acct_override:1h", trace,
		"with apiKey.cache_strategy=auto, account override must surface unchanged")
}

func TestResolveCacheTTLUsageOverride_NilAPIKeyMatchesPreC2Behaviour(t *testing.T) {
	svc := &GatewayService{settingService: NewSettingService(&gatewayTTLSettingRepo{}, &config.Config{})}
	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{
		"cache_ttl_override_enabled": true,
		"cache_ttl_override_target":  "5m",
	}}

	target, ok, trace := svc.resolveCacheTTLUsageOverride(context.Background(), account, nil)
	require.True(t, ok)
	require.Equal(t, "5m", target)
	require.Equal(t, "acct_override:5m", trace)
}

// TestResolveCacheTTLRequestRewriteTarget_MirrorsUsageTarget locks in the
// "consistent by default" rule: until we deliberately introduce divergence, the
// request-rewrite target must equal the usage-classification target. This guards
// against accidental drift that would create a compliance gap ("upstream produced
// 1h cache but we billed 5m").
func TestResolveCacheTTLRequestRewriteTarget_MirrorsUsageTarget(t *testing.T) {
	svc := &GatewayService{settingService: NewSettingService(&gatewayTTLSettingRepo{}, &config.Config{})}
	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	apiKey := &APIKey{CacheStrategy: CacheStrategyLatencyPriority}

	usageTarget, usageOK := svc.resolveCacheTTLUsageOverrideTarget(context.Background(), account, apiKey)
	rewriteTarget, rewriteOK := svc.resolveCacheTTLRequestRewriteTarget(context.Background(), account, apiKey)

	require.Equal(t, usageOK, rewriteOK)
	require.Equal(t, usageTarget, rewriteTarget)
}

// User preference promises an upstream TTL. Accounts whose request body is not
// rewritten (passthrough / Bedrock / Vertex / API key) must not be billed by it,
// otherwise a 5m upstream cache would be charged at the 1h price.
func TestResolveCacheTTLUsageOverride_UserPrefIgnoredWhenRequestNotEnforced(t *testing.T) {
	svc := &GatewayService{settingService: NewSettingService(&gatewayTTLSettingRepo{}, &config.Config{})}
	apiKey := &APIKey{CacheStrategy: CacheStrategyLatencyPriority}

	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	target, ok, trace := svc.resolveCacheTTLUsageOverride(context.Background(), account, apiKey)
	require.False(t, ok)
	require.Empty(t, target)
	require.Equal(t, "user_pref_ignored:not_enforced", trace)

	account.Extra = map[string]any{"cache_ttl_override_enabled": true, "cache_ttl_override_target": "5m"}
	target, ok, trace = svc.resolveCacheTTLUsageOverride(context.Background(), account, apiKey)
	require.True(t, ok)
	require.Equal(t, "5m", target, "account billing override still applies")
	require.Equal(t, "acct_override:5m;pref_ignored", trace)
	require.LessOrEqual(t, len(trace+"|shadow:would_inject_5m_safe"), cachePolicyTraceMaxLen)
}

func TestResolveCacheTTLRequestRewriteTarget_PolicyChain(t *testing.T) {
	for _, tc := range []struct {
		name        string
		accountType string
		global      string
		strategy    string
		acctTarget  string
		want        string
		wantOK      bool
	}{
		{"user_latency_without_global", AccountTypeOAuth, "false", CacheStrategyLatencyPriority, "", "1h", true},
		{"user_cost_over_global", AccountTypeOAuth, "true", CacheStrategyCostPriority, "", "5m", true},
		{"global_inject_auto", AccountTypeSetupToken, "true", CacheStrategyAuto, "", "1h", true},
		{"account_override_is_billing_only", AccountTypeOAuth, "false", CacheStrategyAuto, "1h", "", false},
		{"apikey_not_enforced", AccountTypeAPIKey, "true", CacheStrategyLatencyPriority, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGatewayForwardingSettingsCacheForTest(t)
			svc := &GatewayService{settingService: NewSettingService(&gatewayTTLSettingRepo{data: map[string]string{
				SettingKeyEnableAnthropicCacheTTL1hInjection: tc.global,
			}}, &config.Config{})}
			account := &Account{Platform: PlatformAnthropic, Type: tc.accountType}
			if tc.acctTarget != "" {
				account.Extra = map[string]any{"cache_ttl_override_enabled": true, "cache_ttl_override_target": tc.acctTarget}
			}
			got, ok := svc.resolveCacheTTLRequestRewriteTarget(context.Background(), account, &APIKey{CacheStrategy: tc.strategy})
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

// End-to-end through Forward: the TTL sent upstream must match the TTL the
// response/billing side classifies by, whenever the user preference applies.
func TestForward_CacheTTLRequestMatchesUserPreference(t *testing.T) {
	for _, tc := range []struct {
		name, global, strategy, initial, acctTarget, wantUpstream string
	}{
		{"user_latency", "false", CacheStrategyLatencyPriority, "5m", "", "1h"},
		{"user_cost_over_global", "true", CacheStrategyCostPriority, "1h", "", "5m"},
		{"global_inject", "true", CacheStrategyAuto, "5m", "", "1h"},
		{"account_override_leaves_request", "false", CacheStrategyAuto, "5m", "1h", "5m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGatewayForwardingSettingsCacheForTest(t)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("User-Agent", "claude-cli/2.1.0")
			apiKey := &APIKey{CacheStrategy: tc.strategy}
			c.Set("api_key", apiKey)
			metadataID := FormatMetadataUserID("a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", "550e8400-e29b-41d4-a716-446655440000", "123e4567-e89b-42d3-a456-426614174000", "2.1.0")
			body := []byte(`{"model":"claude-haiku-4-5-20251001","metadata":{"user_id":` + strconvQuote(metadataID) + `},"system":[{"type":"text","text":"client system","cache_control":{"type":"ephemeral","ttl":"` + tc.initial + `"}}],"messages":[{"role":"user","content":"hello"}]}`)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
			require.NoError(t, err)
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
			}}
			svc := newForwardPartialUsageServiceForTest(upstream)
			svc.settingService = NewSettingService(&gatewayTTLSettingRepo{data: map[string]string{
				SettingKeyEnableAnthropicCacheTTL1hInjection: tc.global,
			}}, &config.Config{})
			account := newAnthropicOAuthAccountForPartialUsageTest()
			if tc.acctTarget != "" {
				account.Extra = map[string]any{"cache_ttl_override_enabled": true, "cache_ttl_override_target": tc.acctTarget}
			}

			_, err = svc.Forward(context.Background(), c, account, parsed)
			require.NoError(t, err)
			require.Equal(t, tc.wantUpstream, gjson.GetBytes(upstream.lastBody, "system.0.cache_control.ttl").String())

			if _, prefApplied := apiKey.CacheStrategyTTLTarget(); prefApplied {
				usageTarget, ok := svc.resolveCacheTTLUsageOverrideTarget(context.Background(), account, apiKey)
				require.True(t, ok)
				require.Equal(t, tc.wantUpstream, usageTarget, "billing TTL must match upstream TTL")
			}
		})
	}
}
