//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type diagnosticsStreamResult struct {
	body  string
	usage *ClaudeUsage
	err   string
}

func runDiagnosticsStream(t *testing.T, path string, enabled bool, stream string) diagnosticsStreamResult {
	t.Helper()
	if enabled {
		t.Setenv("SUB2API_CLAUDE_DIAGNOSTICS", "true")
	} else {
		t.Setenv("SUB2API_CLAUDE_DIAGNOSTICS", "false")
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: sseResponseHeader(), Body: io.NopCloser(strings.NewReader(stream))}
	svc := newMinimalGatewayService()
	account := &Account{ID: 917}
	startTime := time.Now()
	var result *streamingResult
	var err error
	if path == "oauth" {
		result, err = svc.handleStreamingResponse(context.Background(), resp, c, account, startTime, "model", "model", false)
	} else {
		result, err = svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, account, startTime, "model")
	}
	out := diagnosticsStreamResult{body: recorder.Body.String()}
	if result != nil && result.usage != nil {
		usage := *result.usage
		out.usage = &usage
	}
	if err != nil {
		out.err = err.Error()
	}
	return out
}

func TestAnthropicStreamDiagnosticsAreBehaviorNeutral(t *testing.T) {
	gin.SetMode(gin.TestMode)
	streams := map[string]string{
		"complete": "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"text\":\"diagnostic payload🙂\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		"truncated": "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n",
	}
	for _, path := range []string{"oauth", "apikey_passthrough"} {
		t.Run(path, func(t *testing.T) {
			for name, stream := range streams {
				t.Run(name, func(t *testing.T) {
					baseline := runDiagnosticsStream(t, path, false, stream)
					observed := runDiagnosticsStream(t, path, true, stream)
					require.Equal(t, baseline, observed, "diagnostics must preserve downstream bytes, usage, and error result")
					if path == "apikey_passthrough" && name == "complete" {
						require.Equal(t, stream, observed.body, "API-key passthrough must forward canonical SSE bytes exactly")
					}
				})
			}
		})
	}
}
