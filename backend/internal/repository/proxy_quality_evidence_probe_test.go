package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProxyProbeSource429And5xxAreUncertain(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()

			_, _, err := (&proxyProbeService{}).probeWithURL(context.Background(), server.Client(), server.URL, "ipify")

			require.Error(t, err)
			require.Equal(t, service.ProxyQualityReasonUnexpectedHTTPStatus, service.ProxyProbeDiagnosticErrorReason(err))
		})
	}
}

func TestProxyProbeMalformedJSONOrIPIsUncertain(t *testing.T) {
	for _, body := range []string{"not-json", `{"ip":"not-an-ip"}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			_, _, err := (&proxyProbeService{}).probeWithURL(context.Background(), server.Client(), server.URL, "ipify")

			require.Error(t, err)
			require.Equal(t, service.ProxyQualityReasonInvalidProbeResponse, service.ProxyProbeDiagnosticErrorReason(err))
		})
	}
}
