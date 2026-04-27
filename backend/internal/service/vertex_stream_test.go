package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// buildVertexStreamGinContext returns a gin.Context backed by a httptest
// ResponseRecorder so the test can inspect what the handler wrote to the
// downstream client.
func buildVertexStreamGinContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

// fakeVertexUpstreamResponse builds an *http.Response whose body holds a
// known SSE stream identical to what Vertex's :streamRawPredict would return.
func fakeVertexUpstreamResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// canonicalVertexSSE is a minimal full Anthropic-style SSE conversation as
// returned by Vertex (identical event shape to Anthropic Messages stream).
const canonicalVertexSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5@20250929","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":15,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":12}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestHandleVertexStreamingResponse_ForwardsSSEAndExtractsUsage(t *testing.T) {
	svc := &GatewayService{}
	c, rec := buildVertexStreamGinContext(t)
	resp := fakeVertexUpstreamResponse(canonicalVertexSSE)
	acct := &Account{ID: 100, Type: AccountTypeVertex, Platform: PlatformAnthropic}

	result, err := svc.handleVertexStreamingResponse(c.Request.Context(), resp, c, acct, time.Now(), "claude-sonnet-4-5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)

	// usage was parsed from message_start (input) and message_delta (output)
	assert.Equal(t, 15, result.usage.InputTokens)
	assert.Equal(t, 12, result.usage.OutputTokens)

	// firstTokenMs was set on first non-empty data event
	assert.NotNil(t, result.firstTokenMs)

	// SSE was forwarded to the client recorder
	body := rec.Body.String()
	assert.Contains(t, body, "event: message_start")
	assert.Contains(t, body, "event: message_stop")
	assert.Contains(t, body, "Hello")
	assert.Contains(t, body, " world")

	// Standard SSE response headers were set
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no", rec.Header().Get("X-Accel-Buffering"))
}

func TestHandleVertexStreamingResponse_MissingTerminalEventReturnsError(t *testing.T) {
	// No message_stop / no [DONE] → handler should report incomplete stream
	const partialSSE = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_x","usage":{"input_tokens":10}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n"

	svc := &GatewayService{}
	c, _ := buildVertexStreamGinContext(t)
	resp := fakeVertexUpstreamResponse(partialSSE)
	acct := &Account{ID: 101, Type: AccountTypeVertex, Platform: PlatformAnthropic}

	result, err := svc.handleVertexStreamingResponse(c.Request.Context(), resp, c, acct, time.Now(), "claude-sonnet-4-5")
	require.Error(t, err)
	require.NotNil(t, result)
	assert.Contains(t, err.Error(), "missing terminal event")
}

// Note: an explicit "no Flusher" path test is omitted because gin always
// wraps the underlying ResponseWriter with its own type that implements
// http.Flusher — the type assertion in the handler is therefore unreachable
// in tests, and asserting it would just be testing gin internals.
