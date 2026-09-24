package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
)

// TestGetOrCreateFingerprintForAccount_NonCC_UsesRegistrationFingerprint covers the P1-1
// core invariant: a macOS-registered account, when accessed by a non-Claude-CLI client,
// reports MacOS in x-stainless-os instead of the global Linux default.
func TestGetOrCreateFingerprintForAccount_NonCC_UsesRegistrationFingerprint(t *testing.T) {
	cache := &trackingIdentityCache{}
	svc := NewIdentityService(cache)

	account := &Account{
		ID: 100,
		Extra: map[string]any{
			ExtraKeyRegistrationFingerprint: &RegistrationFingerprint{
				OS:             "MacOS",
				Arch:           "arm64",
				Runtime:        "node",
				RuntimeVersion: "v22.11.0",
				UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
			},
		},
	}

	headers := http.Header{}
	headers.Set("User-Agent", "OpenAI/JS 6.26.0") // non-CC client

	fp, err := svc.GetOrCreateFingerprintForAccount(context.Background(), account, headers)
	require.NoError(t, err)
	require.NotNil(t, fp)

	// Cache must not be touched (non-CC client policy)
	require.Equal(t, 0, cache.getCalls)
	require.Equal(t, 0, cache.setCalls)

	// OS/Arch overridden by registration fingerprint
	require.Equal(t, "MacOS", fp.StainlessOS, "should use MacOS from registration fp")
	require.Equal(t, "arm64", fp.StainlessArch, "should use arm64 from registration fp")

	// runtime/runtime_version preserved as default (CLI mimicry)
	require.Equal(t, defaultFingerprint().StainlessRuntime, fp.StainlessRuntime)
	require.Equal(t, defaultFingerprint().StainlessRuntimeVersion, fp.StainlessRuntimeVersion)
	require.Equal(t, defaultFingerprint().UserAgent, fp.UserAgent)
	require.NotEmpty(t, fp.ClientID)
}

func TestGetOrCreateFingerprintForAccount_NonCC_NoRegFp_FallsBackToDefault(t *testing.T) {
	cache := &trackingIdentityCache{}
	svc := NewIdentityService(cache)

	// Account without registration_fingerprint
	account := &Account{ID: 100}

	headers := http.Header{}
	headers.Set("User-Agent", "OpenAI/JS 6.26.0")

	fp, err := svc.GetOrCreateFingerprintForAccount(context.Background(), account, headers)
	require.NoError(t, err)
	require.NotNil(t, fp)

	// Falls back to defaultFingerprint
	require.Equal(t, defaultFingerprint().StainlessOS, fp.StainlessOS)
	require.Equal(t, defaultFingerprint().StainlessArch, fp.StainlessArch)
	require.Equal(t, defaultFingerprint().UserAgent, fp.UserAgent)
}

func TestGetOrCreateFingerprintForAccount_NilAccount_FallsBackToDefault(t *testing.T) {
	cache := &trackingIdentityCache{}
	svc := NewIdentityService(cache)

	headers := http.Header{}
	headers.Set("User-Agent", "OpenAI/JS 6.26.0")

	fp, err := svc.GetOrCreateFingerprintForAccount(context.Background(), nil, headers)
	require.NoError(t, err)
	require.NotNil(t, fp)
	require.Equal(t, defaultFingerprint().StainlessOS, fp.StainlessOS)
}

// TestGetOrCreateFingerprintForAccount_RealCC_IgnoresRegFp ensures that real Claude CLI
// clients use their own headers and the per-account fingerprint cache, not the
// registration fingerprint. The reg fp is purely a fallback for non-CC clients.
func TestGetOrCreateFingerprintForAccount_RealCC_IgnoresRegFp(t *testing.T) {
	cache := &trackingIdentityCache{}
	svc := NewIdentityService(cache)

	account := &Account{
		ID: 100,
		Extra: map[string]any{
			ExtraKeyRegistrationFingerprint: &RegistrationFingerprint{
				OS:   "MacOS",
				Arch: "arm64",
			},
		},
	}

	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.116 (external, cli)")
	headers.Set("X-Stainless-OS", "Linux") // real CC reports its actual OS
	headers.Set("X-Stainless-Arch", "x64")

	fp, err := svc.GetOrCreateFingerprintForAccount(context.Background(), account, headers)
	require.NoError(t, err)
	require.NotNil(t, fp)

	// CC path uses request headers, not reg fp
	require.Equal(t, "Linux", fp.StainlessOS, "real CC should use request headers, not reg fp")
	require.Equal(t, "x64", fp.StainlessArch)

	// Cache should be touched for CC path
	require.Greater(t, cache.getCalls+cache.setCalls, 0)
}

// TestGetOrCreateFingerprintForAccount_NonCC_PartialRegFp ensures that an incomplete
// registration fingerprint (e.g., only OS captured, arch unknown) only overrides
// the populated fields and falls back to defaults for the rest.
func TestGetOrCreateFingerprintForAccount_NonCC_PartialRegFp(t *testing.T) {
	cache := &trackingIdentityCache{}
	svc := NewIdentityService(cache)

	account := &Account{
		ID: 100,
		Extra: map[string]any{
			ExtraKeyRegistrationFingerprint: &RegistrationFingerprint{
				OS: "Windows",
				// Arch intentionally empty
			},
		},
	}

	headers := http.Header{}
	headers.Set("User-Agent", "OpenAI/JS 6.26.0")

	fp, err := svc.GetOrCreateFingerprintForAccount(context.Background(), account, headers)
	require.NoError(t, err)

	require.Equal(t, "Windows", fp.StainlessOS, "OS should be overridden")
	require.Equal(t, defaultFingerprint().StainlessArch, fp.StainlessArch, "empty Arch should preserve default")
}

// TestGetOrCreateFingerprint_LegacyAPI_StillWorks ensures the deprecated method continues
// to work for non-migrated callers (returns default for non-CC, no reg fp lookup).
func TestGetOrCreateFingerprint_LegacyAPI_StillWorks(t *testing.T) {
	cache := &trackingIdentityCache{}
	svc := NewIdentityService(cache)

	headers := http.Header{}
	headers.Set("User-Agent", "OpenAI/JS 6.26.0")

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 42, headers)
	require.NoError(t, err)
	require.NotNil(t, fp)

	// Legacy API has no account → cannot apply reg fp → uses default
	require.Equal(t, defaultFingerprint().StainlessOS, fp.StainlessOS)
}

// TestGetOrCreateFingerprintForAccount_NonCC_AppliesPerAccountVersionDithering verifies
// P1-2: when cli_recent_versions is populated, different accountIDs get different UAs.
func TestGetOrCreateFingerprintForAccount_NonCC_AppliesPerAccountVersionDithering(t *testing.T) {
	origRecent := GetCachedRecentVersions()
	t.Cleanup(func() { SetCachedRecentVersions(origRecent) })
	SetCachedRecentVersions([]string{"2.1.302", "2.1.301", "2.1.300"})

	svc := NewIdentityService(&trackingIdentityCache{})

	headers := http.Header{}
	headers.Set("User-Agent", "OpenAI/JS 6.26.0") // non-CC client

	uaSet := map[string]int{}
	for id := int64(1); id <= 200; id++ {
		acct := &Account{ID: id}
		fp, err := svc.GetOrCreateFingerprintForAccount(context.Background(), acct, headers)
		require.NoError(t, err)
		uaSet[fp.UserAgent]++
	}

	// 期望覆盖到 2 个或 3 个不同版本 UA（5% N-2 桶在小样本上偶尔为 0 也可接受）。
	require.GreaterOrEqual(t, len(uaSet), 2, "should produce at least 2 distinct UAs across 200 accounts: %v", uaSet)
	for ua := range uaSet {
		require.Contains(t, ua, "claude-cli/", "UA must be claude-cli format: %q", ua)
	}

	// 同一 accountID 多次调用必须稳定
	a1, _ := svc.GetOrCreateFingerprintForAccount(context.Background(), &Account{ID: 99}, headers)
	a2, _ := svc.GetOrCreateFingerprintForAccount(context.Background(), &Account{ID: 99}, headers)
	require.Equal(t, a1.UserAgent, a2.UserAgent, "same accountID should produce same UA")
	expectedUA := claude.BuildUserAgentForVersion(claude.PickVersionForAccount(99, []string{"2.1.302", "2.1.301", "2.1.300"}))
	require.Equal(t, expectedUA, a1.UserAgent)
}
