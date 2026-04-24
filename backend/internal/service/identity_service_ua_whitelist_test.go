package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// trackingIdentityCache counts Get/Set calls and stores the last-written fingerprint.
// Used to assert that non-claude-cli UAs never touch the cache.
type trackingIdentityCache struct {
	stored     *Fingerprint
	getCalls   int
	setCalls   int
	initialFP  *Fingerprint
	initialErr error
}

func (c *trackingIdentityCache) GetFingerprint(_ context.Context, _ int64) (*Fingerprint, error) {
	c.getCalls++
	if c.stored != nil {
		return c.stored, nil
	}
	return c.initialFP, c.initialErr
}

func (c *trackingIdentityCache) SetFingerprint(_ context.Context, _ int64, fp *Fingerprint) error {
	c.setCalls++
	cp := *fp
	c.stored = &cp
	return nil
}

func (c *trackingIdentityCache) GetMaskedSessionID(_ context.Context, _ int64) (string, error) {
	return "", nil
}
func (c *trackingIdentityCache) SetMaskedSessionID(_ context.Context, _ int64, _ string, _ time.Duration) error {
	return nil
}

func TestGetOrCreateFingerprint_NonClaudeCLIUA_NeverTouchesCache(t *testing.T) {
	tests := []struct {
		name string
		ua   string
	}{
		{"OpenAI JS SDK", "OpenAI/JS 6.26.0"},
		{"ai-sdk", "ai/6.0.105 ai-sdk/provider-utils/4.0.16 runtime/node.js/22"},
		{"empty UA", ""},
		{"curl", "curl/7.68.0"},
		{"claude-api (decoy, not claude-cli)", "claude-api/1.0.0"},
		{"claude-cli without version", "claude-cli/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := &trackingIdentityCache{}
			svc := NewIdentityService(cache)

			headers := http.Header{}
			if tt.ua != "" {
				headers.Set("User-Agent", tt.ua)
			}
			// simulate the client also passing stainless headers for an OpenAI-SDK-ish req
			headers.Set("X-Stainless-Package-Version", "6.26.0")
			headers.Set("X-Stainless-OS", "Linux")

			fp, err := svc.GetOrCreateFingerprint(context.Background(), 42, headers)
			require.NoError(t, err)
			require.NotNil(t, fp)

			require.Equal(t, 0, cache.getCalls, "must not read cache for non-CC UA")
			require.Equal(t, 0, cache.setCalls, "must not write cache for non-CC UA")

			// Returned fingerprint must be a real CC default, not the client's polluted values
			require.Equal(t, defaultFingerprint.UserAgent, fp.UserAgent)
			require.Equal(t, defaultFingerprint.StainlessPackageVersion, fp.StainlessPackageVersion)
			require.Equal(t, defaultFingerprint.StainlessOS, fp.StainlessOS)
			require.Equal(t, defaultFingerprint.StainlessRuntimeVersion, fp.StainlessRuntimeVersion)
			require.NotEmpty(t, fp.ClientID, "ClientID should be generated per request")
		})
	}
}

func TestGetOrCreateFingerprint_NonClaudeCLIUA_IgnoresPollutedCache(t *testing.T) {
	// Simulate the production incident: cache contains an OpenAI/JS-polluted fingerprint
	// from before the whitelist was added. Non-CC UA requests must not read or surface it.
	cache := &trackingIdentityCache{
		initialFP: &Fingerprint{
			ClientID:                "poisoned-client-id",
			UserAgent:               "OpenAI/JS 6.26.0",
			StainlessLang:           "js",
			StainlessPackageVersion: "6.26.0",
			StainlessOS:             "Linux",
			StainlessArch:           "x64",
			StainlessRuntime:        "node",
			StainlessRuntimeVersion: "v22.22.0",
			UpdatedAt:               time.Now().Unix(),
		},
	}
	svc := NewIdentityService(cache)

	headers := http.Header{}
	headers.Set("User-Agent", "OpenAI/JS 6.26.0")

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 1, headers)
	require.NoError(t, err)

	require.Equal(t, 0, cache.getCalls, "must not read cache for non-CC UA even if polluted entry exists")
	require.Equal(t, 0, cache.setCalls)
	require.Equal(t, defaultFingerprint.UserAgent, fp.UserAgent, "returned fingerprint must be defaultFingerprint, not polluted cache")
	require.NotEqual(t, "poisoned-client-id", fp.ClientID)
}

func TestGetOrCreateFingerprint_RealClaudeCLI_DiscardsPollutedCacheAndRebuilds(t *testing.T) {
	// Cache has OpenAI/JS pollution from a prior OpenClaw request.
	// A real claude-cli request must detect it, discard, and create fresh from its own headers.
	cache := &trackingIdentityCache{
		initialFP: &Fingerprint{
			ClientID:                "poisoned-client-id",
			UserAgent:               "OpenAI/JS 6.26.0",
			StainlessLang:           "js",
			StainlessPackageVersion: "6.26.0",
			StainlessOS:             "Linux",
			StainlessArch:           "x64",
			StainlessRuntime:        "node",
			StainlessRuntimeVersion: "v22.22.0",
			UpdatedAt:               time.Now().Unix(),
		},
	}
	svc := NewIdentityService(cache)

	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.118 (external, cli)")
	headers.Set("X-Stainless-Lang", "js")
	headers.Set("X-Stainless-Package-Version", "0.81.0")
	headers.Set("X-Stainless-OS", "MacOS")
	headers.Set("X-Stainless-Arch", "arm64")
	headers.Set("X-Stainless-Runtime", "node")
	headers.Set("X-Stainless-Runtime-Version", "v24.3.0")

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 1, headers)
	require.NoError(t, err)

	require.Equal(t, 1, cache.getCalls, "should read cache once for CC UA")
	require.Equal(t, 1, cache.setCalls, "should rebuild and write cache after discarding polluted entry")
	require.Equal(t, "claude-cli/2.1.118 (external, cli)", fp.UserAgent)
	require.Equal(t, "0.81.0", fp.StainlessPackageVersion, "should have taken value from real CC headers, not polluted cache")
	require.Equal(t, "MacOS", fp.StainlessOS)
	require.Equal(t, "arm64", fp.StainlessArch)
	require.Equal(t, "v24.3.0", fp.StainlessRuntimeVersion)
	require.NotEqual(t, "poisoned-client-id", fp.ClientID, "new ClientID should be generated after discarding polluted cache")
}

func TestGetOrCreateFingerprint_RealClaudeCLI_FirstRequest_WritesCache(t *testing.T) {
	cache := &trackingIdentityCache{}
	svc := NewIdentityService(cache)

	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.118 (external, cli)")
	headers.Set("X-Stainless-Package-Version", "0.81.0")
	headers.Set("X-Stainless-OS", "MacOS")

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 99, headers)
	require.NoError(t, err)

	require.Equal(t, 1, cache.getCalls)
	require.Equal(t, 1, cache.setCalls, "must write cache for first real CC request")
	require.Equal(t, "claude-cli/2.1.118 (external, cli)", fp.UserAgent)
	require.NotNil(t, cache.stored)
	require.Equal(t, fp.UserAgent, cache.stored.UserAgent)
}

func TestGetOrCreateFingerprint_RealClaudeCLI_CleanCache_ReturnsCached(t *testing.T) {
	// Cache has a valid CC fingerprint; same-version request should not re-write.
	now := time.Now().Unix()
	cache := &trackingIdentityCache{
		initialFP: &Fingerprint{
			ClientID:                "existing-client-id",
			UserAgent:               "claude-cli/2.1.118 (external, cli)",
			StainlessLang:           "js",
			StainlessPackageVersion: "0.81.0",
			StainlessOS:             "MacOS",
			StainlessArch:           "arm64",
			StainlessRuntime:        "node",
			StainlessRuntimeVersion: "v24.3.0",
			UpdatedAt:               now,
		},
	}
	svc := NewIdentityService(cache)

	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.118 (external, cli)")

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 1, headers)
	require.NoError(t, err)

	require.Equal(t, 1, cache.getCalls)
	require.Equal(t, 0, cache.setCalls, "same-version clean cache should not be re-written")
	require.Equal(t, "existing-client-id", fp.ClientID)
	require.Equal(t, "claude-cli/2.1.118 (external, cli)", fp.UserAgent)
}
