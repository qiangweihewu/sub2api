package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const claudeWireContractFixtureDir = "testdata/claude-wire-contract-v1"

type claudeWireContractManifest struct {
	SchemaVersion int                            `json:"schema_version"`
	SourceKind    string                         `json:"source_kind"`
	Fixtures      []claudeWireContractCase       `json:"fixtures"`
	Streams       []claudeWireContractStreamCase `json:"streams"`
}

type claudeWireContractCase struct {
	FixtureID                 string            `json:"fixture_id"`
	ClientFamily              string            `json:"client_family"`
	ModeledVersion            string            `json:"modeled_version"`
	ModeledVersionIsEmpirical bool              `json:"modeled_version_is_empirical"`
	InputFile                 string            `json:"input_file"`
	ExpectedFiles             map[string]string `json:"expected_files"`
	BetaHeader                string            `json:"beta_header"`
}

type claudeWireContractStreamCase struct {
	FixtureID    string `json:"fixture_id"`
	InputFile    string `json:"input_file"`
	ExpectedFile string `json:"expected_file"`
}

func readClaudeWireContractManifest(dir string) (*claudeWireContractManifest, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var manifest claudeWireContractManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := validateClaudeWireContractManifest(dir, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func validateClaudeWireContractManifest(dir string, manifest *claudeWireContractManifest) error {
	if manifest == nil {
		return fmt.Errorf("manifest is required")
	}
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d", manifest.SchemaVersion)
	}
	if manifest.SourceKind != "synthetic" {
		return fmt.Errorf("source_kind must be synthetic")
	}
	if len(manifest.Fixtures) == 0 {
		return fmt.Errorf("manifest has no request fixtures")
	}
	for _, fixture := range manifest.Fixtures {
		if fixture.FixtureID == "" || fixture.ClientFamily == "" || fixture.ModeledVersion == "" {
			return fmt.Errorf("fixture metadata is incomplete")
		}
		if fixture.ModeledVersionIsEmpirical {
			return fmt.Errorf("fixture %q must not claim an empirically observed version", fixture.FixtureID)
		}
		if fixture.InputFile == "" || len(fixture.ExpectedFiles) == 0 {
			return fmt.Errorf("fixture %q requires input_file and at least one expected output", fixture.FixtureID)
		}
		paths := []string{fixture.InputFile}
		for _, expected := range fixture.ExpectedFiles {
			if expected == "" {
				return fmt.Errorf("fixture %q has an empty expected output path", fixture.FixtureID)
			}
			paths = append(paths, expected)
		}
		for _, name := range paths {
			if filepath.Base(name) != name {
				return fmt.Errorf("fixture %q has non-local path %q", fixture.FixtureID, name)
			}
			payload, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return fmt.Errorf("fixture %q missing expected/input file %q: %w", fixture.FixtureID, name, err)
			}
			if containsPrivateClaudeFixtureMaterial(payload) {
				return fmt.Errorf("fixture %q contains credential-like or private payload material", fixture.FixtureID)
			}
			if strings.HasSuffix(name, ".json") && !json.Valid(payload) {
				return fmt.Errorf("fixture %q file %q is not valid JSON", fixture.FixtureID, name)
			}
		}
	}
	for _, stream := range manifest.Streams {
		if stream.FixtureID == "" || stream.InputFile == "" || stream.ExpectedFile == "" {
			return fmt.Errorf("stream fixture metadata is incomplete")
		}
		for _, name := range []string{stream.InputFile, stream.ExpectedFile} {
			if filepath.Base(name) != name {
				return fmt.Errorf("stream fixture has non-local path %q", name)
			}
			payload, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return fmt.Errorf("stream fixture %q missing file %q: %w", stream.FixtureID, name, err)
			}
			if containsPrivateClaudeFixtureMaterial(payload) {
				return fmt.Errorf("stream fixture %q contains credential-like or private payload material", stream.FixtureID)
			}
		}
	}
	return nil
}

func containsPrivateClaudeFixtureMaterial(payload []byte) bool {
	lower := strings.ToLower(string(payload))
	for _, marker := range []string{"sk-ant-", "authorization", "x-api-key", "inbound-token", "cookie"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func TestClaudeWireContractManifestRejectsUnknownSchemaAndMissingExpectedOutput(t *testing.T) {
	manifest, err := readClaudeWireContractManifest(claudeWireContractFixtureDir)
	require.NoError(t, err)
	require.Equal(t, "synthetic", manifest.SourceKind)
	require.NotEmpty(t, manifest.Fixtures)

	unknownSchema := *manifest
	unknownSchema.SchemaVersion = 999
	require.ErrorContains(t, validateClaudeWireContractManifest(claudeWireContractFixtureDir, &unknownSchema), "unsupported schema_version")

	missingExpected := *manifest
	missingExpected.Fixtures = append([]claudeWireContractCase(nil), manifest.Fixtures...)
	missingExpected.Fixtures[0].ExpectedFiles = nil
	require.ErrorContains(t, validateClaudeWireContractManifest(claudeWireContractFixtureDir, &missingExpected), "requires input_file and at least one expected output")
}

func TestClaudeWireContractFixturesMatchForwardedRequestBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manifest, err := readClaudeWireContractManifest(claudeWireContractFixtureDir)
	require.NoError(t, err)

	for _, fixture := range manifest.Fixtures {
		for profile, expectedFile := range fixture.ExpectedFiles {
			t.Run(fixture.FixtureID+"/"+profile, func(t *testing.T) {
				t.Setenv(upstreamPolicyRuntimeEnv, "true")
				input, err := os.ReadFile(filepath.Join(claudeWireContractFixtureDir, fixture.InputFile))
				require.NoError(t, err)
				expected, err := os.ReadFile(filepath.Join(claudeWireContractFixtureDir, expectedFile))
				require.NoError(t, err)

				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"message","model":"claude-sonnet-4-6","content":[{"type":"text","text":"synthetic ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))}
				upstream := &anthropicHTTPUpstreamRecorder{resp: response}
				cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
				svc := &GatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg), httpUpstream: upstream, rateLimitService: &RateLimitService{}, deferredService: &DeferredService{}, settingService: newPolicyRuntimeSettings(t, true)}
				account := newAnthropicAPIKeyAccountForTest()
				if profile == "transparent" {
					account.Extra = map[string]any{"upstream_passthrough": map[string]any{"profile": "transparent"}}
				} else {
					account.Extra = map[string]any{"upstream_passthrough": map[string]any{"profile": "protected"}}
				}

				requestRecorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(requestRecorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				if fixture.BetaHeader != "" {
					c.Request.Header.Set("Anthropic-Beta", fixture.BetaHeader)
				}
				parsed := &ParsedRequest{Body: NewRequestBodyRef(input), Model: "claude-sonnet-4-6"}
				result, err := svc.Forward(context.Background(), c, account, parsed)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, string(expected), string(upstream.lastBody), "wire request must match the independently authored fixture")
			})
		}
	}
}

func TestClaudeWireContractToolNamesOnlyChangeAtProtocolFields(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		payload, err := os.ReadFile(filepath.Join(claudeWireContractFixtureDir, name))
		require.NoError(t, err)
		require.False(t, containsPrivateClaudeFixtureMaterial(payload))
		return payload
	}
	input := read("unicode-tool-structure.request.json")
	rw := buildToolNameRewriteFromBody(input)
	require.NotNil(t, rw)
	require.Equal(t, "cc_sess_list", rw.Forward["sessions_list"])
	require.Equal(t, string(read("unicode-tool-structure.mapped.json")), string(applyToolNameRewriteToBody(input, rw)))
	wire := read("tool-response.wire.json")
	expected := read("tool-response.restored.json")
	require.Equal(t, string(expected), string(restoreToolNamesInBytes(wire, rw)))
	// The SSE envelope is independently fixed; ordinary text and tool arguments
	// in the payload must not be mistaken for protocol tool-name fields.
	stream := []byte("event: message\ndata: " + strings.TrimSpace(string(wire)) + "\n\n")
	wantStream := "event: message\ndata: " + strings.TrimSpace(string(expected)) + "\n\n"
	require.Equal(t, wantStream, string(restoreToolNamesInBytes(stream, rw)))
}

type claudeWireContractStreamResult struct {
	body  string
	usage *ClaudeUsage
	err   string
}

func replayClaudeWireContractStream(t *testing.T, fixture claudeWireContractStreamCase, diagnostics bool) claudeWireContractStreamResult {
	t.Helper()
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	if diagnostics {
		t.Setenv("SUB2API_CLAUDE_DIAGNOSTICS", "true")
	} else {
		t.Setenv("SUB2API_CLAUDE_DIAGNOSTICS", "false")
	}
	input, err := os.ReadFile(filepath.Join(claudeWireContractFixtureDir, fixture.InputFile))
	require.NoError(t, err)
	expected, err := os.ReadFile(filepath.Join(claudeWireContractFixtureDir, fixture.ExpectedFile))
	require.NoError(t, err)

	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(string(input)))}
	upstream := &anthropicHTTPUpstreamRecorder{resp: response}
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg), httpUpstream: upstream, rateLimitService: &RateLimitService{}, deferredService: &DeferredService{}, settingService: newPolicyRuntimeSettings(t, true)}
	account := newAnthropicAPIKeyAccountForTest()
	account.Extra = map[string]any{"upstream_passthrough": map[string]any{"profile": "transparent"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := []byte(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"synthetic stream request"}]}`)
	parsed := &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-sonnet-4-6", Stream: true}
	result, forwardErr := svc.Forward(context.Background(), c, account, parsed)
	out := claudeWireContractStreamResult{body: recorder.Body.String()}
	if result != nil {
		usage := result.Usage
		out.usage = &usage
	}
	if forwardErr != nil {
		out.err = forwardErr.Error()
	}
	require.Equal(t, string(expected), out.body, "synthetic SSE replay must preserve exact event bytes")
	return out
}

func TestClaudeWireContractSSEReplayDiagnosticsAreByteAndUsageNeutral(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manifest, err := readClaudeWireContractManifest(claudeWireContractFixtureDir)
	require.NoError(t, err)
	for _, fixture := range manifest.Streams {
		t.Run(fixture.FixtureID, func(t *testing.T) {
			baseline := replayClaudeWireContractStream(t, fixture, false)
			observed := replayClaudeWireContractStream(t, fixture, true)
			require.Equal(t, baseline, observed, "diagnostics must not change forwarded SSE bytes, parsed usage, or error outcome")
			require.NotNil(t, observed.usage)
		})
	}
}
