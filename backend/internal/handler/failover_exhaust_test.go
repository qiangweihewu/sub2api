package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnthropicFailoverExhaustedCopiesSafeRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	date := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	tests := []struct {
		name   string
		status int
		value  string
		want   string
	}{
		{name: "429 delta seconds", status: http.StatusTooManyRequests, value: "120", want: "120"},
		{name: "503 http date", status: http.StatusServiceUnavailable, value: date, want: date},
		{name: "invalid value", status: http.StatusTooManyRequests, value: "120\r\nX-Evil: yes"},
		{name: "401 does not copy retry hint", status: http.StatusUnauthorized, value: "120"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			failoverErr := &service.UpstreamFailoverError{StatusCode: tt.status, ResponseHeaders: http.Header{"Retry-After": []string{tt.value}}}
			(&GatewayHandler{}).handleFailoverExhausted(c, failoverErr, service.PlatformAnthropic, false)
			assert.Equal(t, tt.want, w.Header().Get("Retry-After"))
		})
	}
}

func TestAnthropicFailoverExhaustedDoesNotSetRetryAfterAfterStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, err := c.Writer.Write([]byte("data: partial\n\n"))
	require.NoError(t, err)
	(&GatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusTooManyRequests, ResponseHeaders: http.Header{"Retry-After": []string{"30"}},
	}, service.PlatformAnthropic, true)
	assert.Empty(t, w.Header().Get("Retry-After"))
	assert.True(t, strings.Contains(w.Body.String(), `"type":"error"`))
}

func TestCopyAnthropicFailoverRetryAfterSkipsNonRetryableMappedStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	copyAnthropicFailoverRetryAfter(c, http.Header{"Retry-After": []string{"30"}}, http.StatusServiceUnavailable, http.StatusBadRequest, false)
	assert.Empty(t, w.Header().Get("Retry-After"))
}

func TestSoftenBedrockExhaustStatus(t *testing.T) {
	// Bedrock soft remap: 429 -> 529 (so mapUpstreamError yields 503 overloaded_error)
	assert.Equal(t, 529, softenBedrockExhaustStatus(http.StatusTooManyRequests, true))
	// not soft: unchanged
	assert.Equal(t, http.StatusTooManyRequests, softenBedrockExhaustStatus(http.StatusTooManyRequests, false))
	// soft but not 429: unchanged
	assert.Equal(t, http.StatusBadGateway, softenBedrockExhaustStatus(http.StatusBadGateway, true))
}
