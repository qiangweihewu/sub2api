// Package anthropicdiag passively inspects Anthropic SSE streams for diagnostics.
// It deliberately stores no event payloads or upstream supplied labels.
package anthropicdiag

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxLineBytes  = 8 << 10
	maxEventBytes = 32 << 10
	maxBlocks     = 256
)

// Enabled reports whether the opt-in stream observer is enabled.
func Enabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SUB2API_CLAUDE_DIAGNOSTICS")), "true")
}

// Summary contains only bounded counters, durations, and fixed reason codes.
type Summary struct {
	Status            string
	Reason            string
	MessageStart      int
	ContentBlockStart int
	ContentBlockDelta int
	ContentBlockStop  int
	MessageDelta      int
	MessageStop       int
	Ping              int
	Error             int
	Unknown           int
	DuplicateStop     int
	AfterTerminal     int
	FirstEventMS      int64
	FirstContentMS    int64
	InputTokens       int64
	CacheCreateTokens int64
	CacheReadTokens   int64
	OutputTokens      int64
	UsageFields       int
	EOF               bool
}

// LogValue is safe to log: it includes no upstream-controlled strings.
func (s Summary) LogValue() string {
	return fmt.Sprintf("status=%s reason=%s events=%d/%d/%d/%d/%d/%d/%d/%d unknown=%d dup_stop=%d after_terminal=%d first_ms=%d first_content_ms=%d usage_fields=%d input=%d cache_create=%d cache_read=%d output=%d eof=%t",
		s.Status, s.Reason, s.MessageStart, s.ContentBlockStart, s.ContentBlockDelta, s.ContentBlockStop,
		s.MessageDelta, s.MessageStop, s.Ping, s.Error, s.Unknown, s.DuplicateStop, s.AfterTerminal,
		s.FirstEventMS, s.FirstContentMS, s.UsageFields, s.InputTokens, s.CacheCreateTokens, s.CacheReadTokens, s.OutputTokens, s.EOF)
}

// Observer is a bounded, passive parser. Feed never holds its lock while reading
// the upstream body, so finalization can safely race a blocked network read.
type Observer struct {
	mu             sync.Mutex
	started        time.Time
	summary        Summary
	line           []byte
	eventBytes     int
	data           []byte
	eventName      string
	droppingLine   bool
	droppingEvent  bool
	skipLF         bool
	firstEvent     bool
	firstContent   bool
	terminal       bool
	messageStarted bool
	blocks         map[int]bool
	lastOutput     int64
	hasOutput      bool
	badReason      string
	finished       bool
}

func New(start time.Time) *Observer {
	if start.IsZero() {
		start = time.Now()
	}
	return &Observer{started: start, blocks: make(map[int]bool), line: make([]byte, 0, 256), data: make([]byte, 0, 1024)}
}

// Wrap returns a ReadCloser that tees bytes into the observer and preserves the
// original reader and close behavior exactly.
func (o *Observer) Wrap(r io.ReadCloser) io.ReadCloser {
	return &teeReadCloser{ReadCloser: r, observer: o}
}

func (o *Observer) accept(p []byte, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return
	}
	if len(p) > 0 {
		o.feedLocked(p)
	}
	if err == io.EOF {
		o.summary.EOF = true
	}
}

func (o *Observer) feedLocked(p []byte) {
	for _, b := range p {
		if o.skipLF {
			o.skipLF = false
			if b == '\n' {
				continue
			}
		}
		if b == '\r' || b == '\n' {
			o.finishLine()
			if b == '\r' {
				o.skipLF = true
			}
			continue
		}
		if o.droppingLine {
			continue
		}
		if len(o.line) >= maxLineBytes {
			o.droppingLine = true
			o.inconclusive("oversized_line")
			continue
		}
		o.line = append(o.line, b)
	}
}

func (o *Observer) finishLine() {
	if o.droppingLine {
		o.droppingLine = false
		o.line = o.line[:0]
		o.droppingEvent = true
		return
	}
	line := o.line
	o.line = o.line[:0]
	if len(line) == 0 {
		o.dispatch()
		return
	}
	if line[0] == ':' {
		return
	}
	o.eventBytes += len(line) + 1
	if o.eventBytes > maxEventBytes {
		o.droppingEvent = true
		o.data = o.data[:0]
		o.inconclusive("oversized_event")
		return
	}
	field, value, hasColon := bytes.Cut(line, []byte{':'})
	if hasColon && len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(field) {
	case "event":
		if len(value) <= 64 {
			o.eventName = string(value)
		} else {
			o.droppingEvent = true
			o.inconclusive("oversized_event_name")
		}
	case "data":
		if o.droppingEvent {
			return
		}
		need := len(value)
		if len(o.data) > 0 {
			need++
		}
		if len(o.data)+need > maxEventBytes {
			o.droppingEvent = true
			o.data = o.data[:0]
			o.inconclusive("oversized_event")
			return
		}
		if len(o.data) > 0 {
			o.data = append(o.data, '\n')
		}
		o.data = append(o.data, value...)
	default:
		// id/retry and unknown SSE fields do not affect Anthropic event interpretation.
	}
}

func (o *Observer) dispatch() {
	if o.droppingEvent {
		o.droppingEvent = false
		o.eventName = ""
		o.data = o.data[:0]
		return
	}
	if len(o.data) == 0 && o.eventName == "" {
		return
	}
	name := o.eventName
	if name == "" {
		name = "message"
	}
	o.eventName = ""
	if !o.firstEvent {
		o.firstEvent = true
		o.summary.FirstEventMS = time.Since(o.started).Milliseconds()
	}
	if !o.known(name) && name != "message" {
		o.summary.Unknown++
		o.inconclusive("unknown_event")
		o.clearEvent()
		return
	}
	if len(o.data) == 0 {
		o.inconclusive("missing_data")
		o.clearEvent()
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(o.data, &payload); err != nil || payload == nil {
		o.inconclusive("malformed_json")
		o.clearEvent()
		return
	}
	if name == "message" {
		var dataName string
		if json.Unmarshal(payload["type"], &dataName) != nil || !o.known(dataName) || dataName == "message" {
			o.summary.Unknown++
			o.inconclusive("unknown_event")
			o.clearEvent()
			return
		}
		name = dataName
	}
	if o.terminal {
		if name == "message_stop" {
			o.summary.MessageStop++
			o.summary.DuplicateStop++
		}
		o.summary.AfterTerminal++
		o.anomaly("event_after_terminal")
		o.clearEvent()
		return
	}
	if typ, ok := payload["type"]; ok {
		var dataType string
		if json.Unmarshal(typ, &dataType) != nil || (name != "message" && dataType != name) {
			o.inconclusive("event_type_mismatch")
			o.clearEvent()
			return
		}
	}
	o.observe(name, payload)
	o.clearEvent()
}

func (o *Observer) clearEvent() { o.data = o.data[:0]; o.eventBytes = 0 }
func (o *Observer) known(n string) bool {
	switch n {
	case "message", "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "ping", "error":
		return true
	}
	return false
}

func (o *Observer) observe(name string, p map[string]json.RawMessage) {
	switch name {
	case "message": // Generic SSE event name; use only a known Anthropic payload type.
		var typ string
		if json.Unmarshal(p["type"], &typ) != nil || !o.known(typ) || typ == "message" {
			o.summary.Unknown++
			o.inconclusive("unknown_event")
			return
		}
		o.observe(typ, p)
	case "message_start":
		o.summary.MessageStart++
		if o.messageStarted {
			o.anomaly("duplicate_message_start")
		}
		o.messageStarted = true
		var msg map[string]json.RawMessage
		if json.Unmarshal(p["message"], &msg) == nil {
			o.usage(msg["usage"])
		}
	case "content_block_start":
		o.summary.ContentBlockStart++
		o.contentSeen()
		if !o.messageStarted {
			o.inconclusive("event_before_message_start")
		}
		idx, ok := intField(p["index"])
		if !ok {
			o.inconclusive("invalid_block_index")
			return
		}
		if len(o.blocks) >= maxBlocks {
			o.inconclusive("block_limit")
			return
		}
		if _, exists := o.blocks[idx]; exists {
			o.anomaly("duplicate_block_start")
		} else {
			o.blocks[idx] = true
		}
	case "content_block_delta":
		o.summary.ContentBlockDelta++
		o.contentSeen()
		if !o.messageStarted {
			o.inconclusive("event_before_message_start")
		}
		idx, ok := intField(p["index"])
		if !ok {
			o.inconclusive("invalid_block_index")
			return
		}
		if !o.blocks[idx] {
			o.anomaly("delta_without_open_block")
		}
	case "content_block_stop":
		o.summary.ContentBlockStop++
		if !o.messageStarted {
			o.inconclusive("event_before_message_start")
		}
		idx, ok := intField(p["index"])
		if !ok {
			o.inconclusive("invalid_block_index")
			return
		}
		if !o.blocks[idx] {
			o.anomaly("duplicate_or_unmatched_block_stop")
		} else {
			delete(o.blocks, idx)
		}
	case "message_delta":
		o.summary.MessageDelta++
		if !o.messageStarted {
			o.inconclusive("event_before_message_start")
		}
		o.usage(p["usage"])
	case "message_stop":
		o.summary.MessageStop++
		if o.terminal {
			o.summary.DuplicateStop++
			o.anomaly("duplicate_message_stop")
		}
		o.terminal = true
	case "ping":
		o.summary.Ping++
	case "error":
		o.summary.Error++
	}
}

func (o *Observer) contentSeen() {
	if !o.firstContent {
		o.firstContent = true
		o.summary.FirstContentMS = time.Since(o.started).Milliseconds()
	}
}

func (o *Observer) usage(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		o.inconclusive("malformed_usage")
		return
	}
	for key, target := range map[string]*int64{"input_tokens": &o.summary.InputTokens, "cache_creation_input_tokens": &o.summary.CacheCreateTokens, "cache_read_input_tokens": &o.summary.CacheReadTokens, "output_tokens": &o.summary.OutputTokens} {
		v, ok := values[key]
		if !ok {
			continue
		}
		n, valid := int64Field(v)
		if !valid {
			o.anomaly("invalid_usage_number")
			continue
		}
		o.summary.UsageFields++
		if key == "output_tokens" {
			if o.hasOutput && n < o.lastOutput {
				o.anomaly("decreasing_output_tokens")
			}
			o.hasOutput = true
			o.lastOutput = n
		}
		*target = n // Input/cache fields may legitimately be revised by the upstream.
	}
}

func int64Field(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] < '0' || raw[0] > '9' {
		return 0, false
	}
	var n json.Number
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&n) != nil {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	return i, err == nil && i >= 0
}
func intField(raw json.RawMessage) (int, bool) {
	n, ok := int64Field(raw)
	if !ok || int64(int(n)) != n {
		return 0, false
	}
	return int(n), true
}

func (o *Observer) anomaly(reason string) {
	if o.badReason == "" || o.summary.Status != "anomaly" {
		o.badReason = reason
		o.summary.Status = "anomaly"
	}
}
func (o *Observer) inconclusive(reason string) {
	if o.summary.Status == "anomaly" {
		return
	}
	if o.badReason == "" {
		o.badReason = reason
	}
	o.summary.Status = "inconclusive"
}

// Finalize returns the one bounded result. EOF is observed from the wrapped
// reader; a gateway return caused by client disconnect remains inconclusive.
func (o *Observer) Finalize() Summary {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return o.summary
	}
	o.finished = true
	if len(o.line) > 0 || o.droppingLine || len(o.data) > 0 || o.droppingEvent {
		o.inconclusive("truncated_event")
	}
	if o.summary.Status == "" {
		if !o.summary.EOF {
			o.inconclusive("upstream_not_eof")
		} else if !o.terminal {
			if o.summary.Error > 0 {
				o.inconclusive("upstream_error_event")
			} else {
				o.anomaly("missing_message_stop")
			}
		} else if !o.messageStarted {
			o.inconclusive("missing_message_start")
		} else if len(o.blocks) > 0 {
			o.anomaly("open_content_block")
		} else {
			o.summary.Status = "clean"
			o.badReason = "none"
		}
	}
	if o.summary.Status == "anomaly" || o.summary.Status == "inconclusive" {
		o.summary.Reason = o.badReason
	}
	if o.summary.Status == "clean" {
		o.summary.Reason = "none"
	}
	return o.summary
}

type teeReadCloser struct {
	io.ReadCloser
	observer *Observer
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	t.observer.accept(p[:n], err)
	return n, err
}
