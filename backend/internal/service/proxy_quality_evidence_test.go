package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

type proxyQualityEvidenceRepo struct {
	ProxyRepository
	proxy *Proxy
}

func (r *proxyQualityEvidenceRepo) GetByID(context.Context, int64) (*Proxy, error) {
	return r.proxy, nil
}

type proxyQualityEvidenceProber struct {
	info *ProxyExitInfo
	err  error
}

type proxyQualityEvidenceLatencyCache struct {
	info *ProxyLatencyInfo
}

func (c *proxyQualityEvidenceLatencyCache) GetProxyLatencies(context.Context, []int64) (map[int64]*ProxyLatencyInfo, error) {
	if c.info == nil {
		return map[int64]*ProxyLatencyInfo{}, nil
	}
	return map[int64]*ProxyLatencyInfo{9: c.info}, nil
}

func (c *proxyQualityEvidenceLatencyCache) SetProxyLatency(_ context.Context, _ int64, info *ProxyLatencyInfo) error {
	copyOfInfo := *info
	c.info = &copyOfInfo
	return nil
}

func (p proxyQualityEvidenceProber) ProbeProxy(context.Context, string) (*ProxyExitInfo, int64, error) {
	return p.info, 3, p.err
}

func TestCheckProxyQualityWithoutProberPreservesUnknownEvidence(t *testing.T) {
	svc := &adminServiceImpl{proxyRepo: &proxyQualityEvidenceRepo{proxy: &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080}}}

	result, err := svc.CheckProxyQuality(context.Background(), 9)

	require.NoError(t, err)
	require.Equal(t, 1+len(proxyQualityTargets), result.UnknownCount)
	require.Equal(t, "unknown", result.OverallStatus)
	require.Equal(t, "unknown", result.Grade)
	require.Zero(t, result.Score)
	require.Equal(t, "unknown", result.Items[0].Status)
	require.Equal(t, "unavailable", result.Items[0].EvidenceSource)
	require.Equal(t, "probe_unavailable", result.Items[0].ReasonCode)
	require.Equal(t, "unknown", result.Items[1].Status)
}

func TestCheckProxyQualityInvalidExitIPDoesNotClaimHealthyConnectivity(t *testing.T) {
	svc := &adminServiceImpl{
		proxyRepo:   &proxyQualityEvidenceRepo{proxy: &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080}},
		proxyProber: proxyQualityEvidenceProber{info: &ProxyExitInfo{IP: "not-an-ip"}},
	}

	result, err := svc.CheckProxyQuality(context.Background(), 9)

	require.NoError(t, err)
	require.Equal(t, "unknown", result.Items[0].Status)
	require.Equal(t, "invalid_probe_response", result.Items[0].ReasonCode)
	require.Empty(t, result.ExitIP)
	require.NotEqual(t, "healthy", result.OverallStatus)
	require.Equal(t, "unknown", result.Items[1].Status)
}

func TestRunProxyQualityTargetCanceledRequestIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	target := proxyQualityTarget{Target: "synthetic", URL: server.URL, Method: http.MethodGet}

	item := runProxyQualityTarget(ctx, server.Client(), target)

	require.Equal(t, "unknown", item.Status)
	require.Equal(t, "request_canceled", item.ReasonCode)
	require.Equal(t, "unavailable", item.EvidenceSource)
}

func TestRunProxyQualityTarget429RemainsKnownWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	target := proxyQualityTarget{Target: "synthetic", URL: server.URL, Method: http.MethodGet}

	item := runProxyQualityTarget(context.Background(), server.Client(), target)

	require.Equal(t, "warn", item.Status)
	require.Equal(t, "observed", item.EvidenceSource)
	require.Equal(t, "target_rate_limited", item.ReasonCode)
}

func TestFinalizeProxyQualityResultAllUnknownHasNoPassingScore(t *testing.T) {
	result := &ProxyQualityCheckResult{UnknownCount: 4}

	finalizeProxyQualityResult(result)

	require.Zero(t, result.Score)
	require.Equal(t, "unknown", result.Grade)
	require.Equal(t, "unknown", result.OverallStatus)
}

func TestFinalizeProxyQualityResultOnlyNotApplicableItemsIsUnrated(t *testing.T) {
	result := &ProxyQualityCheckResult{NotApplicableCount: 4}

	finalizeProxyQualityResult(result)

	require.Zero(t, result.Score)
	require.Equal(t, "unknown", result.Grade)
	require.Equal(t, "unknown", result.OverallStatus)
	require.Contains(t, result.Summary, "暂未评级")
}

func TestFinalizeProxyQualityResultMixedEvidenceIsProvisional(t *testing.T) {
	result := &ProxyQualityCheckResult{
		PassedCount:  1,
		UnknownCount: 1,
		Items: []ProxyQualityCheckItem{
			{Target: "base_connectivity", Status: "pass", EvidenceSource: "observed"},
			{Target: "gemini", Status: "unknown", EvidenceSource: "unavailable"},
		},
	}

	finalizeProxyQualityResult(result)

	require.Equal(t, "unknown", result.OverallStatus)
	require.Equal(t, "unknown", result.Grade)
	require.Contains(t, result.Summary, "部分证据")
}

func TestFinalizeProxyQualityResultKnownWarningDominatesUnknown(t *testing.T) {
	result := &ProxyQualityCheckResult{WarnCount: 1, UnknownCount: 1}

	finalizeProxyQualityResult(result)

	require.Equal(t, "warn", result.OverallStatus)
}

func TestProxyQualityDiagnosticsNeverExposeProxyCredentialsOrQuery(t *testing.T) {
	secretUser, secretPassword, secretQuery := "user-sentinel", "pass-sentinel", "query-sentinel"
	proxy := &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080, Username: secretUser, Password: secretPassword}
	proberErr := errors.New("dial failed through http://user-sentinel:pass-sentinel@proxy.test:8080/?token=query-sentinel")
	svc := &adminServiceImpl{
		proxyRepo:   &proxyQualityEvidenceRepo{proxy: proxy},
		proxyProber: proxyQualityEvidenceProber{err: proberErr},
	}

	result, err := svc.CheckProxyQuality(context.Background(), 9)

	require.NoError(t, err)
	encoded, marshalErr := json.Marshal(result)
	require.NoError(t, marshalErr)
	for _, secret := range []string{secretUser, secretPassword, secretQuery} {
		require.NotContains(t, string(encoded), secret)
	}
	require.Contains(t, string(encoded), `"reason_code"`)
}

func TestProxyQualityOverallStatusUnknownForOnlyUnknownItems(t *testing.T) {
	result := &ProxyQualityCheckResult{UnknownCount: 2}
	require.Equal(t, "unknown", proxyQualityOverallStatus(result))
}

func TestUnknownQualitySnapshotPreservesPriorLatencyEvidence(t *testing.T) {
	latency := int64(27)
	cache := &proxyQualityEvidenceLatencyCache{info: &ProxyLatencyInfo{
		Success:   true,
		LatencyMs: &latency,
		Message:   "prior observed latency",
		IPAddress: "203.0.113.9",
	}}
	svc := &adminServiceImpl{proxyLatencyCache: cache}
	result := &ProxyQualityCheckResult{
		UnknownCount: 1,
		Grade:        "unknown",
		Summary:      "部分证据",
		Items:        []ProxyQualityCheckItem{{Target: "base_connectivity", Status: "unknown"}},
	}

	svc.saveProxyQualitySnapshot(context.Background(), 9, result, nil)
	proxies := []ProxyWithAccountCount{{Proxy: Proxy{ID: 9}}}
	svc.attachProxyLatency(context.Background(), proxies)

	require.Equal(t, "unknown", cache.info.QualityStatus)
	require.True(t, cache.info.Success)
	require.Equal(t, int64(27), *cache.info.LatencyMs)
	require.Equal(t, "203.0.113.9", cache.info.IPAddress)
	require.Equal(t, "success", proxies[0].LatencyStatus)
	require.Equal(t, "unknown", proxies[0].QualityStatus)
}

func TestUnknownQualitySnapshotWithoutPriorLatencyDoesNotInventFailure(t *testing.T) {
	cache := &proxyQualityEvidenceLatencyCache{}
	svc := &adminServiceImpl{proxyLatencyCache: cache}
	result := &ProxyQualityCheckResult{
		UnknownCount: 1,
		Grade:        "unknown",
		Summary:      "部分证据",
		Items:        []ProxyQualityCheckItem{{Target: "base_connectivity", Status: "unknown"}},
	}

	svc.saveProxyQualitySnapshot(context.Background(), 9, result, nil)
	proxies := []ProxyWithAccountCount{{Proxy: Proxy{ID: 9}}}
	svc.attachProxyLatency(context.Background(), proxies)

	require.Equal(t, "unknown", proxies[0].QualityStatus)
	require.Empty(t, proxies[0].LatencyStatus)
}

func TestTestProxyWithoutProberReturnsUnknownWithoutCacheWrite(t *testing.T) {
	cache := &proxyQualityEvidenceLatencyCache{}
	svc := &adminServiceImpl{
		proxyRepo:         &proxyQualityEvidenceRepo{proxy: &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080}},
		proxyLatencyCache: cache,
	}

	result, err := svc.TestProxy(context.Background(), 9)

	require.NoError(t, err)
	require.False(t, result.Success)
	require.Equal(t, "unknown", result.Status)
	require.Equal(t, "probe_unavailable", result.ReasonCode)
	require.Nil(t, cache.info)
}

func TestTestProxyCanceledProbeReturnsUnknownWithoutCacheWrite(t *testing.T) {
	cache := &proxyQualityEvidenceLatencyCache{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := &adminServiceImpl{
		proxyRepo:         &proxyQualityEvidenceRepo{proxy: &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080}},
		proxyProber:       proxyQualityEvidenceProber{err: errors.New("request canceled")},
		proxyLatencyCache: cache,
	}

	result, err := svc.TestProxy(ctx, 9)

	require.NoError(t, err)
	require.Equal(t, "unknown", result.Status)
	require.Equal(t, ProxyQualityReasonProbeCanceled, result.ReasonCode)
	require.Nil(t, cache.info)
}

func TestTestProxyInvalidExitIPReturnsUnknownWithoutCacheWrite(t *testing.T) {
	cache := &proxyQualityEvidenceLatencyCache{}
	svc := &adminServiceImpl{
		proxyRepo:         &proxyQualityEvidenceRepo{proxy: &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080}},
		proxyProber:       proxyQualityEvidenceProber{info: &ProxyExitInfo{IP: "invalid-ip"}},
		proxyLatencyCache: cache,
	}

	result, err := svc.TestProxy(context.Background(), 9)

	require.NoError(t, err)
	require.Equal(t, "unknown", result.Status)
	require.Equal(t, "invalid_probe_response", result.ReasonCode)
	require.Nil(t, cache.info)
}

func TestTestProxyTypedProbeUncertaintyReturnsUnknownWithoutCacheWrite(t *testing.T) {
	cache := &proxyQualityEvidenceLatencyCache{}
	svc := &adminServiceImpl{
		proxyRepo:         &proxyQualityEvidenceRepo{proxy: &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080}},
		proxyProber:       proxyQualityEvidenceProber{err: NewProxyProbeDiagnosticError(ProxyQualityReasonInvalidProbeResponse)},
		proxyLatencyCache: cache,
	}

	result, err := svc.TestProxy(context.Background(), 9)

	require.NoError(t, err)
	require.Equal(t, "unknown", result.Status)
	require.Equal(t, ProxyQualityReasonInvalidProbeResponse, result.ReasonCode)
	require.Nil(t, cache.info)
}

func TestTestProxyKnownFailureIsSafeAndRemainsFailed(t *testing.T) {
	cache := &proxyQualityEvidenceLatencyCache{}
	secret := "user-sentinel:pass-sentinel"
	svc := &adminServiceImpl{
		proxyRepo:         &proxyQualityEvidenceRepo{proxy: &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080}},
		proxyProber:       proxyQualityEvidenceProber{err: errors.New("proxy connect failed " + secret)},
		proxyLatencyCache: cache,
	}

	result, err := svc.TestProxy(context.Background(), 9)

	require.NoError(t, err)
	require.Equal(t, "failed", result.Status)
	require.Equal(t, ProxyQualityReasonProbeFailed, result.ReasonCode)
	require.NotContains(t, result.Message, secret)
	require.NotNil(t, cache.info)
	require.NotContains(t, cache.info.Message, secret)
}

func TestAsyncProxyProbeUncertaintyDoesNotOverwriteLatency(t *testing.T) {
	latency := int64(31)
	cache := &proxyQualityEvidenceLatencyCache{info: &ProxyLatencyInfo{Success: true, LatencyMs: &latency, Message: "prior success"}}
	svc := &adminServiceImpl{
		proxyProber:       proxyQualityEvidenceProber{err: NewProxyProbeDiagnosticError(ProxyQualityReasonInvalidProbeResponse)},
		proxyLatencyCache: cache,
	}

	svc.probeProxyLatency(context.Background(), &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080})

	require.True(t, cache.info.Success)
	require.Equal(t, int64(31), *cache.info.LatencyMs)
	require.Equal(t, "prior success", cache.info.Message)
}

func TestAsyncProxyProbeInvalidExitIPDoesNotOverwriteLatency(t *testing.T) {
	latency := int64(31)
	cache := &proxyQualityEvidenceLatencyCache{info: &ProxyLatencyInfo{Success: true, LatencyMs: &latency, Message: "prior success"}}
	svc := &adminServiceImpl{
		proxyProber:       proxyQualityEvidenceProber{info: &ProxyExitInfo{IP: "invalid-ip"}},
		proxyLatencyCache: cache,
	}

	svc.probeProxyLatency(context.Background(), &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080})

	require.True(t, cache.info.Success)
	require.Equal(t, int64(31), *cache.info.LatencyMs)
	require.Equal(t, "prior success", cache.info.Message)
}

func TestAsyncProxyProbeKnownFailureIsSanitized(t *testing.T) {
	cache := &proxyQualityEvidenceLatencyCache{}
	svc := &adminServiceImpl{
		proxyProber:       proxyQualityEvidenceProber{err: errors.New("secret proxy credentials")},
		proxyLatencyCache: cache,
	}

	svc.probeProxyLatency(context.Background(), &Proxy{ID: 9, Protocol: "http", Host: "proxy.test", Port: 8080})

	require.NotNil(t, cache.info)
	require.False(t, cache.info.Success)
	require.Equal(t, "Proxy probe failed", cache.info.Message)
}
