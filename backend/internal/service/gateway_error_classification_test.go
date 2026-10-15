package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnthropicFailoverClassificationKeepsRequestErrorsOutOfAccountPool(t *testing.T) {
	svc := &GatewayService{}
	for _, status := range []int{400, 404, 405, 413, 415, 422} {
		require.False(t, svc.shouldFailoverUpstreamError(status), "request error %d must not rotate through accounts", status)
	}
	for _, status := range []int{401, 403, 429, 500, 502, 503, 504, 529} {
		require.True(t, svc.shouldFailoverUpstreamError(status), "upstream error %d retains the existing failover policy", status)
	}
}
