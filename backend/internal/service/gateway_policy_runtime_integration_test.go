//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpstreamPolicyRuntime_ForwardAndRecordUsageKeepAliasPriced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	const alias = "team/best"
	const priced = "claude-sonnet-4"
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	userRepo := &openAIRecordUsageUserRepoStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{})
	svc.settingService = newPolicyRuntimeSettings(t, true)
	svc.rateLimitService = &RateLimitService{}
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"type":"message","model":"team/best","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":6}}`)),
	}}
	svc.httpUpstream = upstream
	account := newAnthropicAPIKeyAccountForTest()
	account.Extra = map[string]any{"upstream_passthrough": map[string]any{"profile": "transparent"}}
	account.Credentials["model_mapping"] = map[string]any{alias: priced}
	body := []byte(`{"model":"team/best", "messages":[{"role":"user","content":"hello"}]}`)
	parsed := &ParsedRequest{Body: NewRequestBodyRef(body), Model: alias}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, string(body), string(upstream.lastBody))
	require.Equal(t, alias, result.Model)
	require.Equal(t, alias, result.UpstreamModel)
	require.Equal(t, priced, result.PricingFallbackModel)
	apiKey := &APIKey{ID: 501, Quota: 100}
	require.Equal(t, priced, svc.recordUsageBillingModel(context.Background(), apiKey, result, ChannelUsageFields{}))
	err = svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: result, APIKey: apiKey, User: &User{ID: 601}, Account: account,
	})
	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, alias, usageRepo.lastLog.Model)
	require.Equal(t, alias, usageRepo.lastLog.RequestedModel)
	require.Greater(t, usageRepo.lastLog.TotalCost, 0.0, "an unpriced wire alias must retain the configured account price fallback")
	require.InDelta(t, usageRepo.lastLog.ActualCost, userRepo.lastAmount, 1e-12)
}

func TestUpstreamPolicyRuntime_ChannelRestrictionChecksActualWireModel(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	settingService := newPolicyRuntimeSettings(t, true)
	account := newAnthropicAPIKeyAccountForTest()
	account.Extra = map[string]any{"upstream_passthrough": map[string]any{"profile": "transparent"}}
	account.Credentials["model_mapping"] = map[string]any{"team/best": "claude-sonnet-4"}
	for _, allowed := range []string{"team/best", "claude-sonnet-4"} {
		channel := Channel{ID: 1, Status: StatusActive, GroupIDs: []int64{10}, RestrictModels: true,
			BillingModelSource: BillingModelSourceUpstream,
			ModelPricing:       []ChannelModelPricing{{Platform: PlatformAnthropic, Models: []string{allowed}}},
		}
		svc := &GatewayService{settingService: settingService,
			channelService: newTestChannelService(makeStandardRepo(channel, map[int64]string{10: PlatformAnthropic}))}
		require.Equal(t, allowed != "team/best", svc.isUpstreamModelRestrictedByChannel(context.Background(), 10, account, "team/best"))
		// With the process opt-in off, existing account mapping remains the
		// selection boundary even though the persisted flag is still true.
		t.Setenv(upstreamPolicyRuntimeEnv, "false")
		require.Equal(t, allowed != "claude-sonnet-4", svc.isUpstreamModelRestrictedByChannel(context.Background(), 10, account, "team/best"))
		t.Setenv(upstreamPolicyRuntimeEnv, "true")
	}
}

func TestUpstreamPolicyRuntime_NetworkInfoToggleIsIndependent(t *testing.T) {
	headers := http.Header{"X-Forwarded-For": {"192.0.2.1"}, "X-Real-Ip": {"192.0.2.2"}}
	for _, clientHeaders := range []bool{false, true} {
		for _, network := range []bool{false, true} {
			ctx := SetUpstreamPolicyInContext(context.Background(), &EffectiveUpstreamPolicy{
				ForwardClientHeaders: clientHeaders, ForwardUserNetworkInfo: network,
			})
			out := http.Header{}
			for key, values := range headers {
				if shouldCopyClientRequestHeader(ctx, key, headers, func(string) bool { return false }) {
					out[key] = append([]string(nil), values...)
				}
			}
			ForwardUserNetworkInfoHeaders(ctx, out, headers)
			if network {
				require.Equal(t, []string{"192.0.2.1"}, out.Values("X-Forwarded-For"))
			} else {
				require.Empty(t, out)
			}
		}
	}
	ctx := SetUpstreamPolicyInContext(context.Background(), &EffectiveUpstreamPolicy{ForwardUserNetworkInfo: true})
	headers.Set("Connection", "X-Forwarded-For")
	out := http.Header{}
	ForwardUserNetworkInfoHeaders(ctx, out, headers)
	require.Empty(t, out.Values("X-Forwarded-For"))
}
