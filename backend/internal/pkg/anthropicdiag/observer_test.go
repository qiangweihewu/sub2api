package anthropicdiag

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

type chunkReader struct {
	data   []byte
	step   int
	closed bool
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.step
	if n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}
func (r *chunkReader) Close() error { r.closed = true; return nil }

func consume(t *testing.T, stream string, step int) (Summary, []byte) {
	t.Helper()
	raw := []byte(stream)
	body := &chunkReader{data: append([]byte(nil), raw...), step: step}
	o := New(time.Now())
	tee := o.Wrap(body)
	got, err := io.ReadAll(tee)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("observer changed downstream bytes")
	}
	if err := tee.Close(); err != nil {
		t.Fatal(err)
	}
	if !body.closed {
		t.Fatal("original body close was not preserved")
	}
	return o.Finalize(), got
}

func TestTeePreservesBytesAndParsesOneByteUTF8CRLFMulitline(t *testing.T) {
	stream := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4,\"cache_creation_input_tokens\":2,\"cache_read_input_tokens\":1}}}\r\n\r\nevent: content_block_start\r\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\r\n\r\nevent: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\r\ndata: \"index\":0,\"delta\":{\"text\":\"秘密🙂\"}}\r\n\r\nevent: content_block_stop\r\ndata: {\"type\":\"content_block_stop\",\"index\":0}\r\n\r\nevent: message_delta\r\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":6,\"output_tokens\":2}}\r\n\r\nevent: message_delta\r\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":5,\"output_tokens\":3}}\r\n\r\nevent: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"
	s, got := consume(t, stream, 1)
	if string(got) != stream {
		t.Fatal("byte stream mismatch")
	}
	if s.Status != "clean" || s.MessageStart != 1 || s.ContentBlockDelta != 1 || s.MessageStop != 1 {
		t.Fatalf("unexpected summary: %+v", s)
	}
	if s.InputTokens != 5 || s.CacheCreateTokens != 2 || s.CacheReadTokens != 1 || s.OutputTokens != 3 {
		t.Fatalf("usage semantics not retained: %+v", s)
	}
	if !s.EOF {
		t.Fatal("EOF was not observed")
	}
	if strings.Contains(s.LogValue(), "秘密") || strings.Contains(s.LogValue(), "index") {
		t.Fatal("summary leaked event content")
	}
}

func TestMalformedJSONAndUnknownEventAreInconclusive(t *testing.T) {
	for name, stream := range map[string]string{
		"malformed": "event: message_start\ndata: {secret\n\n",
		"unknown":   "event: future_event\ndata: {\"type\":\"future_event\",\"secret\":\"keep-private\"}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := consume(t, stream, 3)
			if s.Status != "inconclusive" {
				t.Fatalf("got %s: %+v", s.Status, s)
			}
			if strings.Contains(s.LogValue(), "keep-private") {
				t.Fatal("payload leaked")
			}
		})
	}
}

func TestOversizedFrameAndTruncatedStreamAreInconclusive(t *testing.T) {
	oversized := "event: message_delta\n" + strings.Repeat("data: "+strings.Repeat("x", 4000)+"\n", 10) + "\n"
	s, _ := consume(t, oversized, 257)
	if s.Status != "inconclusive" || s.Reason != "oversized_event" {
		t.Fatalf("unexpected oversized result: %+v", s)
	}
	s, _ = consume(t, "event: message_start\ndata: {\"type\":\"message_start\"}", 2)
	if s.Status != "inconclusive" {
		t.Fatalf("truncated stream must be inconclusive: %+v", s)
	}
}

func TestDuplicateStopAndDataAfterStopAreAnomalies(t *testing.T) {
	stream := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\nevent: ping\ndata: {\"type\":\"ping\"}\n\n"
	s, _ := consume(t, stream, 5)
	if s.Status != "anomaly" || s.DuplicateStop != 1 || s.AfterTerminal != 2 {
		t.Fatalf("unexpected terminal handling: %+v", s)
	}
}

func TestDataOnlyDuplicateStopIsCounted(t *testing.T) {
	stream := "data: {\"type\":\"message_stop\"}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	s, _ := consume(t, stream, 2)
	if s.Status != "anomaly" || s.DuplicateStop != 1 || s.AfterTerminal != 1 {
		t.Fatalf("payload-only duplicate stop was not recognized: %+v", s)
	}
}

func TestUsageValidationAndOutputMonotonicity(t *testing.T) {
	stream := "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":-1,\"output_tokens\":8}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":2,\"output_tokens\":7}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	s, _ := consume(t, stream, 11)
	if s.Status != "anomaly" || s.Reason != "invalid_usage_number" {
		t.Fatalf("negative and decreasing usage should be anomalous: %+v", s)
	}
}

func TestUsageNumbersRequireNonnegativeJSONIntegers(t *testing.T) {
	for _, raw := range []string{`"7"`, `null`, `true`, `-1`, `1.5`, `1e2`, `9223372036854775808`} {
		if _, ok := int64Field([]byte(raw)); ok {
			t.Errorf("invalid token count accepted: %s", raw)
		}
	}
	if n, ok := int64Field([]byte(" 7 ")); !ok || n != 7 {
		t.Fatalf("valid integer rejected: %d %t", n, ok)
	}
}

func TestEOFWithoutTerminalIsAnomalyButNoEOFIsInconclusive(t *testing.T) {
	s, _ := consume(t, "event: ping\ndata: {\"type\":\"ping\"}\n\n", 4)
	if s.Status != "anomaly" || s.Reason != "missing_message_stop" {
		t.Fatalf("unexpected missing terminal result: %+v", s)
	}
	o := New(time.Now())
	tee := o.Wrap(&nonEOFReader{data: []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")})
	_, _ = io.ReadAll(tee)
	s = o.Finalize()
	if s.Status != "inconclusive" || s.Reason != "upstream_not_eof" {
		t.Fatalf("client-side close without upstream EOF should be inconclusive: %+v", s)
	}
}

type nonEOFReader struct{ data []byte }

func (r *nonEOFReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
func (*nonEOFReader) Close() error { return nil }

type blockedReader struct {
	entered chan struct{}
	release chan struct{}
	data    []byte
}

func (r *blockedReader) Read(p []byte) (int, error) {
	close(r.entered)
	<-r.release
	return copy(p, r.data), nil
}
func (*blockedReader) Close() error { return nil }

func TestFinalizeCanRaceBlockedRead(t *testing.T) {
	body := &blockedReader{entered: make(chan struct{}), release: make(chan struct{}), data: []byte("data: {\"type\":\"message_stop\"}\n\n")}
	o := New(time.Now())
	tee := o.Wrap(body)
	readDone := make(chan struct{})
	go func() { _, _ = tee.Read(make([]byte, 256)); close(readDone) }()
	<-body.entered
	first := o.Finalize()
	close(body.release)
	<-readDone
	second := o.Finalize()
	if first.Status != "inconclusive" || first.Reason != "upstream_not_eof" || second != first {
		t.Fatalf("finalize should be stable during a blocked read: first=%+v second=%+v", first, second)
	}
}
