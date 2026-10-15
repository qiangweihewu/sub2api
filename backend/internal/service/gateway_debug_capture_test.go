package service

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGatewayDebugCaptureSecurePermissionsAndDefaultSummary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private", "capture")
	path := filepath.Join(dir, "debug.log")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(debugGatewayRawBodyEnv, "false")
	service := &GatewayService{}
	service.initDebugGatewayBodyFile(path)
	t.Cleanup(func() {
		if capture := service.debugGatewayBodyFile.Load(); capture != nil {
			capture.close()
		}
	})
	if service.debugGatewayBodyFile.Load() == nil {
		t.Fatal("capture was not initialized")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	// Re-open an existing file to verify its permissions are tightened.
	service.debugGatewayBodyFile.Load().close()
	service = &GatewayService{}
	service.initDebugGatewayBodyFile(path)
	if capture := service.debugGatewayBodyFile.Load(); capture != nil {
		t.Cleanup(capture.close)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("file mode = %04o, want 0600", got)
	}
	service.debugLogGatewaySnapshot("CLIENT_ORIGINAL", http.Header{
		"content-type":     {"application/json"},
		"User-Agent":       {"ua-header-sentinel"},
		"Anthropic-Beta":   {"beta-header-sentinel"},
		"X-Stainless-Lang": {"stainless-header-sentinel"},
		"Authorization":    {"Bearer bearer-secret"},
		"X-Private-Header": {"private-header-secret"},
	}, []byte(`{"model":"m","system":[{"type":"text","text":"private-prompt","cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[{"role":"user","content":[{"type":"text","text":"private-tool-input"},{"type":"private-secret-block-type","text":"hidden"}]}],"tools":[{"name":"private-tool"}],"metadata":{"user_id":"private-metadata"},"x-private-secret-key":"hidden"}`), map[string]string{
		"account": "private-context-value",
	})
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-prompt", "private-tool-input", "private-tool", "private-metadata", "private-context-value", "private-header-secret", "bearer-secret", "private-secret-block-type", "x-private-secret-key", "ua-header-sentinel", "beta-header-sentinel", "stainless-header-sentinel"} {
		if strings.Contains(string(content), secret) {
			t.Errorf("default capture leaked %q: %s", secret, content)
		}
	}
	for _, expected := range []string{"\"body_bytes\"", "\"message_count\":1", "\"tool_count\":1", "\"unknown_top_level_field_count\":1", "\"unknown_content_block_type_count\":1", "\"cache_control_count\":1", "\"cache_control_locations\":[\"system\"]", "\"cache_ttl_values\":[\"5m\"]", "\"system_prefix_hmac\"", "\"tools_prefix_hmac\"", "\"content-type\":\"present\"", "\"user-agent\":\"present\"", "\"anthropic-beta\":\"present\"", "\"x-stainless-lang\":\"present\"", "\"authorization\":\"[redacted]\""} {
		if !strings.Contains(string(content), expected) {
			t.Errorf("default structural summary missing %q: %s", expected, content)
		}
	}
}

func TestGatewayDebugCaptureRawBodyRequiresSeparateFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.log")
	t.Setenv(debugGatewayRawBodyEnv, "true")
	service := &GatewayService{}
	service.initDebugGatewayBodyFile(path)
	if capture := service.debugGatewayBodyFile.Load(); capture != nil {
		t.Cleanup(capture.close)
	}
	secretBody := `{"messages":[{"content":"explicitly-captured-prompt"}]}`
	service.debugLogGatewaySnapshot("CLIENT_ORIGINAL", http.Header{
		"User-Agent":     {"raw-mode-header-sentinel"},
		"Anthropic-Beta": {"raw-mode-beta-sentinel"},
	}, []byte(secretBody), nil)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "explicitly-captured-prompt") {
		t.Fatalf("explicit raw capture did not contain request body: %s", content)
	}
	for _, secret := range []string{"raw-mode-header-sentinel", "raw-mode-beta-sentinel"} {
		if strings.Contains(string(content), secret) {
			t.Errorf("raw capture leaked header value %q: %s", secret, content)
		}
	}
}

func TestGatewayDebugCaptureEntryAndTotalCaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bounded.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := newGatewayDebugCaptureWithLimits(file, true, 4096, time.Hour, 1024)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[{"content":"` + strings.Repeat("s", 4000) + `"}]}`)
	capture.write("large", nil, body, nil)
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Size() > 1024 {
		t.Fatalf("entry exceeded cap: %d > 1024", info.Size())
	}
	for i := 0; i < 40; i++ {
		capture.write("small", nil, []byte(`{}`), nil)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Size() > 4096 {
		t.Fatalf("capture exceeded total cap: %d > 4096", info.Size())
	}
	if !capture.isClosed() {
		t.Fatal("capture should stop at its finite write budget")
	}
}

func TestGatewayDebugCaptureConcurrentWritesAreWholeEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := newGatewayDebugCaptureWithLimits(file, false, 1<<20, time.Hour, 1024)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			capture.write("parallel", nil, []byte(`{"messages":[]}`), nil)
		}()
	}
	wg.Wait()
	capture.close()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 40 {
		t.Fatalf("got %d complete lines, want 40", len(lines))
	}
	for i, line := range lines {
		if !strings.Contains(line, `"tag":"parallel"`) {
			t.Errorf("line %d is not a whole record: %q", i, line)
		}
	}
}

func TestGatewayDebugCaptureExpiresAndCanBeClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expired.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := newGatewayDebugCaptureWithLimits(file, false, 4096, 20*time.Millisecond, 1024)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	capture.write("late", nil, []byte(`{}`), nil)
	if !capture.isClosed() {
		t.Fatal("capture did not close at age limit")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	capture.write("after-close", nil, []byte(`{}`), nil)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Fatalf("closed capture changed size: %d -> %d", before.Size(), after.Size())
	}
}

func TestInitDebugGatewayBodyFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.log")
	if err := os.WriteFile(target, []byte("target"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	service := &GatewayService{}
	service.initDebugGatewayBodyFile(link)
	if service.debugGatewayBodyFile.Load() != nil {
		t.Fatal("capture unexpectedly accepted symlink target")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "target" {
		t.Fatalf("symlink target modified: %q", content)
	}
}

func TestInitDebugGatewayBodyFileCreatesPrivateParents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new-private-dir")
	path := filepath.Join(dir, "debug.log")
	service := &GatewayService{}
	service.initDebugGatewayBodyFile(path)
	if capture := service.debugGatewayBodyFile.Load(); capture != nil {
		t.Cleanup(capture.close)
	} else {
		t.Fatal("capture was not initialized")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Fatalf("new parent mode = %04o, want 0700", got)
	}
}
