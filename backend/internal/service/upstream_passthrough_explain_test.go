package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestExplainUpstreamPassthroughPolicySeparatesLegacyRuntimeFromCandidate(t *testing.T) {
	account := &Account{
		ID:       17,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"upstream_passthrough": map[string]any{"profile": "transparent"}},
	}
	settings := newPolicyExplanationSettings(t, true)
	t.Setenv(upstreamPolicyRuntimeEnv, "false")

	got := ExplainUpstreamPassthroughPolicy(context.Background(), account, nil, settings)

	require.False(t, got.Gates.ProcessOptIn)
	require.Equal(t, UpstreamPolicyDatabaseGateEnabled, got.Gates.DatabaseGate)
	require.True(t, got.Gates.AccountEligible)
	require.False(t, got.Gates.RuntimeActive)
	require.Equal(t, UpstreamPolicyRuntimeModeLegacy, got.RuntimeMode)
	require.Nil(t, got.RuntimePolicy)
	require.NotNil(t, got.Candidate)
	require.Equal(t, ProfileTransparent, got.Candidate.Profile)
	require.Equal(t, UpstreamPolicySourceAccountProfile, got.Candidate.Toggles[0].Source)
}

func TestExplainUpstreamPassthroughPolicyReportsUnavailableSettingService(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	got := ExplainUpstreamPassthroughPolicy(context.Background(), account, nil, nil)

	require.Equal(t, UpstreamPolicyDatabaseGateUnavailable, got.Gates.DatabaseGate)
	require.False(t, got.Gates.RuntimeActive)
	require.Equal(t, UpstreamPolicyRuntimeModeLegacy, got.RuntimeMode)
	require.Nil(t, got.RuntimePolicy)
}

func TestExplainUpstreamPassthroughPolicyReportsDatabaseReadFailureAsUnavailable(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	settings := NewSettingService(&fakeSettingRepoForUpstream{failGet: true}, &config.Config{})
	account := &Account{ID: 21, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	got := ExplainUpstreamPassthroughPolicy(context.Background(), account, nil, settings)

	require.True(t, got.Gates.ProcessOptIn)
	require.Equal(t, UpstreamPolicyDatabaseGateUnavailable, got.Gates.DatabaseGate)
	require.False(t, got.Gates.RuntimeActive)
	require.Equal(t, UpstreamPolicyRuntimeModeLegacy, got.RuntimeMode)
	require.Nil(t, got.RuntimePolicy)
	require.Nil(t, got.Candidate)
	ctx := ResolveAndStorePolicy(context.Background(), account, settings)
	_, runtimeActive := GetUpstreamPolicyFromContext(ctx)
	require.False(t, runtimeActive)
}

func TestExplainUpstreamPassthroughPolicyDatabaseGateOffKeepsLegacyRuntime(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	account := &Account{ID: 18, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	got := ExplainUpstreamPassthroughPolicy(context.Background(), account, nil, newPolicyExplanationSettings(t, false))

	require.True(t, got.Gates.ProcessOptIn)
	require.Equal(t, UpstreamPolicyDatabaseGateDisabled, got.Gates.DatabaseGate)
	require.False(t, got.Gates.RuntimeActive)
	require.Equal(t, UpstreamPolicyRuntimeModeLegacy, got.RuntimeMode)
	require.Nil(t, got.RuntimePolicy)
	require.NotNil(t, got.Candidate)
	require.Equal(t, ProfileProtected, got.Candidate.Profile)
}

func TestExplainUpstreamPassthroughPolicyUnknownAccountProfileUsesCategoryDefault(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "false")
	account := &Account{ID: 19, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{
		"upstream_passthrough": map[string]any{
			"profile":   "unrecognized-profile",
			"overrides": map[string]any{"not_a_toggle": true},
		},
	}}

	got := ExplainUpstreamPassthroughPolicy(context.Background(), account, nil, newPolicyExplanationSettings(t, true))

	require.Equal(t, ProfileProtected, got.Candidate.Profile)
	require.Equal(t, CategoryOfficial, got.Candidate.Category)
	for _, toggle := range got.Candidate.Toggles {
		require.Equal(t, UpstreamPolicySourceCategoryDefault, toggle.Source)
	}
}

func TestExplainUpstreamPassthroughPolicyReportsSystemAndAccountToggleSources(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "false")
	defaults := UpstreamPassthroughDefaults{
		Official: UpstreamPassthroughCategoryDefault{
			Profile: ProfileProtected,
			Overrides: map[string]bool{
				ToggleSkipBodyScrub: true,
			},
		},
	}
	rawDefaults, err := json.Marshal(defaults)
	require.NoError(t, err)
	resetUpstreamPassthroughDefaultsCache()
	resetUpstreamPassthroughGlobalOverrideCache()
	t.Cleanup(resetUpstreamPassthroughDefaultsCache)
	t.Cleanup(resetUpstreamPassthroughGlobalOverrideCache)
	settings := NewSettingService(&fakeSettingRepoForUpstream{values: map[string]string{
		SettingKeyUpstreamPolicyV1Enabled:           "true",
		SettingKeyUpstreamPassthroughDefaults:       string(rawDefaults),
		SettingKeyUpstreamPassthroughGlobalOverride: "auto",
	}}, &config.Config{})
	account := &Account{ID: 20, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{
		"upstream_passthrough": map[string]any{"overrides": map[string]any{ToggleForwardClientHeaders: true}},
	}}

	got := ExplainUpstreamPassthroughPolicy(context.Background(), account, nil, settings)

	require.Equal(t, UpstreamPolicySourceAccountOverride, got.Candidate.Toggles[0].Source)
	require.Equal(t, UpstreamPolicySourceCategoryDefault, got.Candidate.Toggles[1].Source)
	require.Equal(t, true, got.Candidate.Toggles[2].Value)
	require.Equal(t, UpstreamPolicySourceSystemOverride, got.Candidate.Toggles[2].Source)
}

func TestExplainUpstreamPassthroughPolicyMatchesRuntimeResolver(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	settings := newPolicyExplanationSettings(t, true)
	cases := []struct {
		name    string
		account *Account
		group   *Group
	}{
		{
			name: "account profile beats group profile and legacy fields",
			account: &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{
				"anthropic_passthrough": true,
				"upstream_passthrough":  map[string]any{"profile": "strict"},
			}},
			group: &Group{ID: 4, DefaultPassthroughProfile: "transparent"},
		},
		{
			name:    "group profile wins over legacy fields",
			account: &Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{"anthropic_passthrough": true}},
			group:   &Group{ID: 5, Hydrated: true, Platform: PlatformAnthropic, Status: "active", DefaultPassthroughProfile: "strict"},
		},
		{
			name: "legacy fields apply when no new profile or overrides exist",
			account: &Account{ID: 3, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{
				"anthropic_passthrough": true,
			}},
		},
		{
			name: "global override takes precedence",
			account: &Account{ID: 6, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{
				"upstream_passthrough": map[string]any{"profile": "strict"},
			}},
		},
		{
			name: "account toggle overrides profile value",
			account: &Account{ID: 7, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{
				"upstream_passthrough": map[string]any{
					"profile": "protected",
					"overrides": map[string]any{
						"forward_client_headers": true,
					},
				},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "global override takes precedence" {
				settings = newPolicyExplanationSettingsWithOverride(t, true, GlobalOverrideForceTransparent)
			} else {
				settings = newPolicyExplanationSettings(t, true)
			}
			got := ExplainUpstreamPassthroughPolicy(context.Background(), tc.account, tc.group, settings)
			ctx := context.Background()
			if tc.group != nil {
				ctx = context.WithValue(ctx, ctxkey.Group, tc.group)
			}
			ctx = ResolveAndStorePolicy(ctx, tc.account, settings)
			want, ok := GetUpstreamPolicyFromContext(ctx)
			require.True(t, ok)
			require.True(t, got.Gates.RuntimeActive)
			require.Equal(t, UpstreamPolicyRuntimeModePolicy, got.RuntimeMode)
			require.NotNil(t, got.RuntimePolicy)
			require.Equal(t, want.ProfileApplied, got.RuntimePolicy.Profile)
			require.Equal(t, want.Category, got.RuntimePolicy.Category)
			require.Equal(t, want.ForwardClientHeaders, got.RuntimePolicy.Toggles[0].Value)
			require.Equal(t, want.SkipBodyScrub, got.RuntimePolicy.Toggles[2].Value)
			if tc.name == "global override takes precedence" {
				for _, toggle := range got.RuntimePolicy.Toggles {
					require.Equal(t, UpstreamPolicySourceGlobalOverride, toggle.Source)
				}
			}
			if tc.name == "account toggle overrides profile value" {
				require.Equal(t, true, got.RuntimePolicy.Toggles[0].Value)
				require.Equal(t, UpstreamPolicySourceAccountOverride, got.RuntimePolicy.Toggles[0].Source)
				require.Equal(t, UpstreamPolicySourceAccountProfile, got.RuntimePolicy.Toggles[1].Source)
			}
			if tc.name == "group profile wins over legacy fields" {
				require.Equal(t, UpstreamPolicySourceGroupProfile, got.RuntimePolicy.Toggles[0].Source)
			}
			if tc.name == "legacy fields apply when no new profile or overrides exist" {
				require.Equal(t, UpstreamPolicySourceLegacy, got.RuntimePolicy.Toggles[0].Source)
				require.Equal(t, UpstreamPolicySourceLegacy, got.RuntimePolicy.Toggles[2].Source)
				require.Equal(t, UpstreamPolicySourceLegacy, got.RuntimePolicy.Toggles[5].Source)
				require.Equal(t, UpstreamPolicySourceCategoryDefault, got.RuntimePolicy.Toggles[1].Source)
			}
		})
	}
}

func TestExplainUpstreamPassthroughPolicyExcludesOAuthAndMantle(t *testing.T) {
	t.Setenv(upstreamPolicyRuntimeEnv, "true")
	settings := newPolicyExplanationSettings(t, true)
	for _, account := range []*Account{
		{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeBedrock, Credentials: map[string]any{"auth_mode": "bedrock_mantle"}},
	} {
		got := ExplainUpstreamPassthroughPolicy(context.Background(), account, nil, settings)
		require.False(t, got.Gates.AccountEligible)
		require.False(t, got.Gates.RuntimeActive)
		require.Equal(t, UpstreamPolicyRuntimeModeIneligible, got.RuntimeMode)
		require.Nil(t, got.RuntimePolicy)
		require.Nil(t, got.Candidate)
	}
}

func newPolicyExplanationSettings(t *testing.T, enabled bool) *SettingService {
	t.Helper()
	return newPolicyExplanationSettingsWithOverride(t, enabled, GlobalOverrideAuto)
}

func newPolicyExplanationSettingsWithOverride(t *testing.T, enabled bool, override GlobalOverrideMode) *SettingService {
	t.Helper()
	resetUpstreamPassthroughDefaultsCache()
	resetUpstreamPassthroughGlobalOverrideCache()
	t.Cleanup(resetUpstreamPassthroughDefaultsCache)
	t.Cleanup(resetUpstreamPassthroughGlobalOverrideCache)
	flag := "false"
	if enabled {
		flag = "true"
	}
	return NewSettingService(&fakeSettingRepoForUpstream{values: map[string]string{
		SettingKeyUpstreamPolicyV1Enabled:           flag,
		SettingKeyUpstreamPassthroughGlobalOverride: string(override),
	}}, &config.Config{})
}
