package service

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

const (
	defaultGatewayDebugCaptureMaxBytes = 8 << 20
	defaultGatewayDebugCaptureMaxAge   = time.Hour
	defaultGatewayDebugCaptureMaxEntry = 64 << 10
)

// gatewayDebugCapture is a bounded, process-local debug sink. The HMAC key is
// random per process so structural comparisons cannot be correlated across runs.
type gatewayDebugCapture struct {
	mu        sync.Mutex
	file      *os.File
	rawBody   bool
	key       [32]byte
	startedAt time.Time
	maxBytes  int64
	maxAge    time.Duration
	maxEntry  int
	written   int64
	closed    bool
	timer     *time.Timer
}

type gatewayDebugRecord struct {
	Time              string            `json:"time"`
	Tag               string            `json:"tag"`
	BodyBytes         int               `json:"body_bytes"`
	JSONValid         bool              `json:"json_valid"`
	RootType          string            `json:"root_type,omitempty"`
	TopLevelFields    []string          `json:"top_level_fields,omitempty"`
	UnknownTopFields  int               `json:"unknown_top_level_field_count,omitempty"`
	MessageCount      int               `json:"message_count,omitempty"`
	ContentBlockType  map[string]int    `json:"content_block_types,omitempty"`
	UnknownBlockTypes int               `json:"unknown_content_block_type_count,omitempty"`
	ToolCount         int               `json:"tool_count,omitempty"`
	CacheControlCount int               `json:"cache_control_count,omitempty"`
	CacheLocations    []string          `json:"cache_control_locations,omitempty"`
	CacheTTLValues    []string          `json:"cache_ttl_values,omitempty"`
	SystemPrefixHMAC  string            `json:"system_prefix_hmac,omitempty"`
	ToolsPrefixHMAC   string            `json:"tools_prefix_hmac,omitempty"`
	ScanTruncated     bool              `json:"scan_truncated,omitempty"`
	ContextFieldCount int               `json:"context_field_count,omitempty"`
	Headers           map[string]string `json:"safe_headers,omitempty"`
	RawBody           string            `json:"raw_body,omitempty"`
	RawBodyTruncated  bool              `json:"raw_body_truncated,omitempty"`
}

func newGatewayDebugCapture(file *os.File, rawBody bool) (*gatewayDebugCapture, error) {
	return newGatewayDebugCaptureWithLimits(file, rawBody, defaultGatewayDebugCaptureMaxBytes, defaultGatewayDebugCaptureMaxAge, defaultGatewayDebugCaptureMaxEntry)
}

func newGatewayDebugCaptureWithLimits(file *os.File, rawBody bool, maxBytes int64, maxAge time.Duration, maxEntry int) (*gatewayDebugCapture, error) {
	if file == nil || maxBytes <= 0 || maxAge <= 0 || maxEntry <= 0 {
		return nil, os.ErrInvalid
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err != nil {
			return nil, err
		}
		return nil, os.ErrInvalid
	}
	capture := &gatewayDebugCapture{
		file:      file,
		rawBody:   rawBody,
		startedAt: time.Now(),
		maxBytes:  maxBytes,
		maxAge:    maxAge,
		maxEntry:  maxEntry,
		written:   info.Size(),
	}
	if _, err := rand.Read(capture.key[:]); err != nil {
		return nil, err
	}
	capture.mu.Lock()
	capture.timer = time.AfterFunc(maxAge, capture.close)
	capture.mu.Unlock()
	if capture.written >= capture.maxBytes {
		capture.close()
	}
	return capture, nil
}

func (c *gatewayDebugCapture) write(tag string, headers http.Header, body []byte, extra map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if time.Since(c.startedAt) >= c.maxAge || c.written >= c.maxBytes {
		c.closeLocked()
		return
	}

	record := c.makeRecord(tag, headers, body, extra)
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	if len(line)+1 > c.maxEntry && record.RawBody != "" {
		record.RawBody = ""
		record.RawBodyTruncated = true
		line, err = json.Marshal(record)
		if err != nil {
			return
		}
	}
	if len(line)+1 > c.maxEntry || int64(len(line)+1) > c.maxBytes-c.written {
		c.closeLocked()
		return
	}
	line = append(line, '\n')
	n, writeErr := c.file.Write(line)
	c.written += int64(n)
	if writeErr != nil || n != len(line) || c.written >= c.maxBytes {
		c.closeLocked()
	}
}

func (c *gatewayDebugCapture) makeRecord(tag string, headers http.Header, body []byte, extra map[string]string) gatewayDebugRecord {
	record := gatewayDebugRecord{
		Time:      time.Now().UTC().Format(time.RFC3339Nano),
		Tag:       boundedLogText(tag, 64),
		BodyBytes: len(body),
		Headers:   safeGatewayDebugHeaders(headers),
	}
	record.ContextFieldCount = len(extra)
	if len(body) > 0 {
		parsed := gjson.ParseBytes(body)
		record.JSONValid = gjson.ValidBytes(body) && parsed.Exists() && (parsed.IsObject() || parsed.IsArray())
		if record.JSONValid {
			record.RootType = "object"
			if parsed.IsArray() {
				record.RootType = "array"
			}
			if parsed.IsObject() {
				knownFields := map[string]bool{
					"model": true, "system": true, "messages": true, "tools": true,
					"tool_choice": true, "max_tokens": true, "stream": true,
					"temperature": true, "top_p": true, "top_k": true,
					"stop_sequences": true, "metadata": true, "thinking": true,
					"output_config": true, "betas": true, "anthropic_version": true,
					"contents": true, "generationConfig": true, "safetySettings": true,
					"systemInstruction": true, "cachedContent": true,
				}
				parsed.ForEach(func(key, _ gjson.Result) bool {
					field := key.String()
					if knownFields[field] {
						record.TopLevelFields = append(record.TopLevelFields, field)
					} else {
						record.UnknownTopFields++
					}
					return len(record.TopLevelFields)+record.UnknownTopFields < 128
				})
				if len(record.TopLevelFields)+record.UnknownTopFields >= 128 {
					record.ScanTruncated = true
				}
				sort.Strings(record.TopLevelFields)
				countJSONItems(parsed.Get("messages"), 128, &record.MessageCount, &record.ScanTruncated, func(message gjson.Result) {
					countJSONItems(message.Get("content"), 256, nil, &record.ScanTruncated, func(block gjson.Result) {
						kind := block.Get("type").String()
						switch kind {
						case "text", "tool_use", "tool_result", "image", "document", "thinking", "redacted_thinking", "server_tool_use", "web_search_tool_result", "image_url", "input_text", "output_text", "function", "refusal", "file", "video", "audio", "json", "reasoning":
							if record.ContentBlockType == nil {
								record.ContentBlockType = make(map[string]int)
							}
							record.ContentBlockType[kind]++
						default:
							record.UnknownBlockTypes++
						}
						c.addCacheControl(&record, block, "message_content")
					})
				})
				countJSONItems(parsed.Get("tools"), 128, &record.ToolCount, &record.ScanTruncated, func(tool gjson.Result) {
					c.addCacheControl(&record, tool, "tools")
				})
				if parsed.Get("system").IsArray() {
					countJSONItems(parsed.Get("system"), 128, nil, &record.ScanTruncated, func(block gjson.Result) {
						c.addCacheControl(&record, block, "system")
					})
				} else {
					c.addCacheControl(&record, parsed.Get("system"), "system")
				}
				record.SystemPrefixHMAC = c.contentHMACPrefix(parsed.Get("system").Raw)
				record.ToolsPrefixHMAC = c.contentHMACPrefix(parsed.Get("tools").Raw)
			}
		}
	}
	if c.rawBody {
		maxRaw := c.maxEntry / 8
		if maxRaw < 1 {
			maxRaw = 1
		}
		if len(body) > maxRaw {
			body = body[:maxRaw]
			record.RawBodyTruncated = true
		}
		record.RawBody = string(body)
	}
	return record
}

func (c *gatewayDebugCapture) contentHMACPrefix(value string) string {
	if value == "" {
		return ""
	}
	if len(value) > 4096 {
		value = value[:4096]
	}
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil)[:12])
}

func countJSONItems(value gjson.Result, max int, count *int, truncated *bool, visit func(gjson.Result)) {
	seen := 0
	stopped := false
	value.ForEach(func(_, item gjson.Result) bool {
		if seen >= max {
			stopped = true
			return false
		}
		seen++
		if count != nil {
			*count = seen
		}
		visit(item)
		return true
	})
	if stopped && truncated != nil {
		*truncated = true
	}
}

func (c *gatewayDebugCapture) addCacheControl(record *gatewayDebugRecord, value gjson.Result, location string) {
	if !value.Exists() {
		return
	}
	cacheControl := value.Get("cache_control")
	if !cacheControl.Exists() {
		return
	}
	record.CacheControlCount++
	if len(record.CacheLocations) < 8 && !containsString(record.CacheLocations, location) {
		record.CacheLocations = append(record.CacheLocations, location)
		sort.Strings(record.CacheLocations)
	}
	ttl := cacheControl.Get("ttl").String()
	switch ttl {
	case "5m", "1h":
		if len(record.CacheTTLValues) < 4 && !containsString(record.CacheTTLValues, ttl) {
			record.CacheTTLValues = append(record.CacheTTLValues, ttl)
			sort.Strings(record.CacheTTLValues)
		}
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func safeGatewayDebugHeaders(headers http.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string)
	for key := range headers {
		canonical := http.CanonicalHeaderKey(key)
		lower := strings.ToLower(canonical)
		if !safeProtocolHeader(lower) {
			continue
		}
		marker := "present"
		if credentialHeader(lower) {
			marker = "[redacted]"
		}
		out[lower] = marker
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func credentialHeader(key string) bool {
	switch key {
	case "authorization", "proxy-authorization", "cookie", "set-cookie",
		"x-api-key", "api-key", "apikey", "x-auth-token", "x-access-token",
		"x-token", "token", "secret", "x-secret", "client-secret",
		"x-client-secret", "proxy-authenticate":
		return true
	default:
		return false
	}
}

func safeProtocolHeader(key string) bool {
	switch key {
	case "authorization", "proxy-authorization", "cookie", "set-cookie",
		"x-api-key", "api-key", "apikey", "x-auth-token", "x-access-token",
		"x-token", "token", "secret", "x-secret", "client-secret",
		"x-client-secret", "proxy-authenticate", "user-agent", "content-type",
		"accept", "anthropic-version", "anthropic-beta",
		"anthropic-dangerous-direct-browser-access", "x-app", "x-stainless-lang",
		"x-stainless-package-version", "x-stainless-os", "x-stainless-arch",
		"x-stainless-runtime", "x-stainless-runtime-version", "x-stainless-retry-count",
		"x-stainless-timeout", "x-stainless-helper-method":
		return true
	default:
		return false
	}
}

func boundedLogText(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	if len(value) > max {
		return value[:max]
	}
	return value
}

func (c *gatewayDebugCapture) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *gatewayDebugCapture) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *gatewayDebugCapture) closeLocked() {
	if c.closed {
		return
	}
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
	}
	_ = c.file.Close()
}
