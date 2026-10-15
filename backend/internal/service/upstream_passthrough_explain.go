package service

import (
	"context"
	"os"
)

type UpstreamPolicyDatabaseGate string

const (
	UpstreamPolicyDatabaseGateEnabled     UpstreamPolicyDatabaseGate = "enabled"
	UpstreamPolicyDatabaseGateDisabled    UpstreamPolicyDatabaseGate = "disabled"
	UpstreamPolicyDatabaseGateUnavailable UpstreamPolicyDatabaseGate = "unavailable"
)

type UpstreamPolicyRuntimeMode string

const (
	UpstreamPolicyRuntimeModePolicy     UpstreamPolicyRuntimeMode = "policy"
	UpstreamPolicyRuntimeModeLegacy     UpstreamPolicyRuntimeMode = "legacy"
	UpstreamPolicyRuntimeModeIneligible UpstreamPolicyRuntimeMode = "ineligible"
)

type UpstreamPolicyValueSource string

const (
	UpstreamPolicySourceCategoryDefault UpstreamPolicyValueSource = "category_default"
	UpstreamPolicySourceAccountProfile  UpstreamPolicyValueSource = "account_profile"
	UpstreamPolicySourceGroupProfile    UpstreamPolicyValueSource = "group_profile"
	UpstreamPolicySourceSystemOverride  UpstreamPolicyValueSource = "system_override"
	UpstreamPolicySourceAccountOverride UpstreamPolicyValueSource = "account_override"
	UpstreamPolicySourceGlobalOverride  UpstreamPolicyValueSource = "global_override"
	UpstreamPolicySourceLegacy          UpstreamPolicyValueSource = "legacy"
)

type UpstreamPolicyGates struct {
	ProcessOptIn    bool                       `json:"process_opt_in"`
	DatabaseGate    UpstreamPolicyDatabaseGate `json:"database_gate"`
	AccountEligible bool                       `json:"account_eligible"`
	RuntimeActive   bool                       `json:"runtime_active"`
}

type UpstreamPolicyToggleExplanation struct {
	Key    string                    `json:"key"`
	Value  bool                      `json:"value"`
	Source UpstreamPolicyValueSource `json:"source"`
}

type ResolvedUpstreamPolicyExplanation struct {
	Category UpstreamCategory                  `json:"category"`
	Profile  PassthroughProfile                `json:"profile"`
	Toggles  []UpstreamPolicyToggleExplanation `json:"toggles"`
}

// UpstreamPolicyExplanation contains only fixed policy enums, booleans, and
// numeric identifiers. It intentionally excludes account text and credentials.
type UpstreamPolicyExplanation struct {
	AccountID     int64                              `json:"account_id"`
	GroupID       *int64                             `json:"group_id,omitempty"`
	Gates         UpstreamPolicyGates                `json:"gates"`
	RuntimeMode   UpstreamPolicyRuntimeMode          `json:"runtime_mode"`
	RuntimePolicy *ResolvedUpstreamPolicyExplanation `json:"runtime_policy,omitempty"`
	Candidate     *ResolvedUpstreamPolicyExplanation `json:"candidate,omitempty"`
}

// ExplainUpstreamPassthroughPolicy reports the runtime gates and the policy
// resolved by the same resolver used by ResolveAndStorePolicy. Candidate is a
// hypothetical resolution when an eligible account's runtime gates are off.
func ExplainUpstreamPassthroughPolicy(
	ctx context.Context,
	account *Account,
	group *Group,
	settingService *SettingService,
) UpstreamPolicyExplanation {
	if ctx == nil {
		ctx = context.Background()
	}
	report := UpstreamPolicyExplanation{RuntimeMode: UpstreamPolicyRuntimeModeLegacy}
	if account != nil {
		report.AccountID = account.ID
	}
	if group != nil {
		id := group.ID
		report.GroupID = &id
	}
	report.Gates.ProcessOptIn = parseDebugEnvBool(os.Getenv(upstreamPolicyRuntimeEnv))
	report.Gates.AccountEligible = upstreamPolicyAccountEligible(account)
	if settingService == nil {
		report.Gates.DatabaseGate = UpstreamPolicyDatabaseGateUnavailable
		if !report.Gates.AccountEligible {
			report.RuntimeMode = UpstreamPolicyRuntimeModeIneligible
		}
		return report
	}
	enabled, available := settingService.ReadUpstreamPolicyV1Gate(ctx)
	if !available {
		report.Gates.DatabaseGate = UpstreamPolicyDatabaseGateUnavailable
		if !report.Gates.AccountEligible {
			report.RuntimeMode = UpstreamPolicyRuntimeModeIneligible
		}
		return report
	}
	if enabled {
		report.Gates.DatabaseGate = UpstreamPolicyDatabaseGateEnabled
	} else {
		report.Gates.DatabaseGate = UpstreamPolicyDatabaseGateDisabled
	}
	if !report.Gates.AccountEligible {
		report.RuntimeMode = UpstreamPolicyRuntimeModeIneligible
		return report
	}
	defaults := settingService.GetUpstreamPassthroughDefaults(ctx)
	override := settingService.GetUpstreamPassthroughGlobalOverride(ctx)
	policy, sources := resolveUpstreamPassthroughPolicyWithGroupTrace(account, group, &defaults, override)
	candidate := explainResolvedPolicy(policy, sources)
	report.Candidate = &candidate
	report.Gates.RuntimeActive = report.Gates.ProcessOptIn && enabled
	if report.Gates.RuntimeActive {
		report.RuntimeMode = UpstreamPolicyRuntimeModePolicy
		runtime := candidate
		report.RuntimePolicy = &runtime
	}
	return report
}

func explainResolvedPolicy(policy EffectiveUpstreamPolicy, sources map[string]string) ResolvedUpstreamPolicyExplanation {
	profile := policy.ProfileApplied
	if profile != ProfileTransparent && profile != ProfileProtected && profile != ProfileStrict {
		// Unknown persisted profiles use Protected toggle values in the resolver;
		// report only a fixed profile enum, never the raw stored string.
		profile = ProfileProtected
	}
	values := map[string]bool{
		ToggleForwardClientHeaders:   policy.ForwardClientHeaders,
		ToggleForwardUserNetworkInfo: policy.ForwardUserNetworkInfo,
		ToggleSkipBodyScrub:          policy.SkipBodyScrub,
		ToggleSkipSystemPromptInject: policy.SkipSystemPromptInject,
		ToggleForwardClientUA:        policy.ForwardClientUA,
		ToggleForwardBetaFlags:       policy.ForwardBetaFlags,
		ToggleSkipModelRewrite:       policy.SkipModelRewrite,
	}
	toggles := make([]UpstreamPolicyToggleExplanation, 0, len(upstreamPassthroughToggleKeys))
	for _, key := range upstreamPassthroughToggleKeys {
		toggles = append(toggles, UpstreamPolicyToggleExplanation{
			Key: key, Value: values[key], Source: upstreamPolicyValueSource(sources[key]),
		})
	}
	return ResolvedUpstreamPolicyExplanation{
		Category: policy.Category,
		Profile:  profile,
		Toggles:  toggles,
	}
}

func upstreamPolicyValueSource(source string) UpstreamPolicyValueSource {
	switch source {
	case string(UpstreamPolicySourceAccountProfile):
		return UpstreamPolicySourceAccountProfile
	case string(UpstreamPolicySourceGroupProfile):
		return UpstreamPolicySourceGroupProfile
	case string(UpstreamPolicySourceSystemOverride):
		return UpstreamPolicySourceSystemOverride
	case string(UpstreamPolicySourceAccountOverride):
		return UpstreamPolicySourceAccountOverride
	case string(UpstreamPolicySourceGlobalOverride):
		return UpstreamPolicySourceGlobalOverride
	case string(UpstreamPolicySourceLegacy):
		return UpstreamPolicySourceLegacy
	default:
		return UpstreamPolicySourceCategoryDefault
	}
}
