package service

import (
	"bytes"
	"net/http"

	"github.com/gin-gonic/gin"
)

// CaptureWriter wraps gin.ResponseWriter to capture the response body while
// transparently forwarding all writes to the underlying writer.
// After the handler completes, the captured data can be stored in a cache.
type CaptureWriter struct {
	gin.ResponseWriter
	buf        bytes.Buffer
	statusCode int
}

// NewCaptureWriter creates a CaptureWriter wrapping the given gin.ResponseWriter.
func NewCaptureWriter(w gin.ResponseWriter) *CaptureWriter {
	return &CaptureWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
	}
}

// WriteHeader captures the status code and forwards it.
func (w *CaptureWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// Write captures data into the internal buffer while forwarding to the client.
func (w *CaptureWriter) Write(data []byte) (int, error) {
	w.buf.Write(data) // capture (ignore error — bytes.Buffer.Write never fails)
	return w.ResponseWriter.Write(data)
}

// WriteString captures and forwards string data.
func (w *CaptureWriter) WriteString(s string) (int, error) {
	w.buf.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// StatusCode returns the captured HTTP status code.
func (w *CaptureWriter) StatusCode() int {
	return w.statusCode
}

// CapturedBytes returns the captured response body.
func (w *CaptureWriter) CapturedBytes() []byte {
	return w.buf.Bytes()
}

// CapturedLen returns the number of bytes captured so far.
func (w *CaptureWriter) CapturedLen() int {
	return w.buf.Len()
}

// ReplayCachedSSEResponse writes a cached SSE response blob back to the client.
// It sets the correct headers for SSE streaming and flushes after writing.
func ReplayCachedSSEResponse(c *gin.Context, data []byte) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Write(data)
	c.Writer.Flush()
}

// ReplayCachedJSONResponse writes a cached non-streaming JSON response.
func ReplayCachedJSONResponse(c *gin.Context, data []byte) {
	c.Header("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Write(data)
}
