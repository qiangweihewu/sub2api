//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Sticky affinity must never promote a wildcard account above a usable specific
// route, but should still be retained within the best available routing tier.
func TestGatewayRoutingTiers_StickyRespectsTier(t *testing.T) {
	cases := []struct {
		name  string
		model string
		setup func(*routingTierFixture)
		want  int64
	}{
		{name: "specific_before_wildcard_sticky", want: 115},
		{name: "outside_sticky_cannot_override_single_rule", setup: func(f *routingTierFixture) {
			delete(f.group.ModelRouting, "*")
		}, want: 115},
		{name: "disabled_specific_retains_wildcard_sticky", setup: func(f *routingTierFixture) {
			f.repo.accountsByID[115].Status = StatusDisabled
		}, want: 116},
		{name: "excluded_specific_retains_wildcard_sticky", setup: func(f *routingTierFixture) {
			f.excluded = map[int64]struct{}{115: {}}
		}, want: 116},
		{name: "same_tier_keeps_sticky_over_account_priority", setup: func(f *routingTierFixture) {
			f.group.ModelRouting["claude-opus-*"] = []int64{115, 117}
			f.cache.sessionBindings["tier-session"] = 115
		}, want: 115},
		{name: "exhaust_specific_tier_before_wildcard", setup: func(f *routingTierFixture) {
			f.group.ModelRouting["claude-opus-*"] = []int64{115, 117}
			f.repo.accountsByID[115].Status = StatusDisabled
		}, want: 117},
		{name: "wildcard_only_keeps_sticky", model: "claude-sonnet-4-6", want: 116},
	}
	for _, path := range []string{"legacy-single", "legacy-mixed", "load-aware"} {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				fixture := newRoutingTierFixture(path == "load-aware")
				if tc.setup != nil {
					tc.setup(fixture)
				}
				model := tc.model
				if model == "" {
					model = "claude-opus-4-7"
				}
				selection, err := fixture.selectAccount(path, model)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.NotNil(t, selection.Account)
				if selection.ReleaseFunc != nil {
					t.Cleanup(selection.ReleaseFunc)
				}
				require.Equal(t, tc.want, selection.Account.ID)
				require.Equal(t, tc.want, fixture.cache.sessionBindings["tier-session"])
			})
		}
	}
}

func TestGatewayRoutingTiers_LoadAwareCapacityFallback(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*routingTierFixture)
		want     int64
		wantWait bool
	}{
		{name: "healthy_specific_before_wildcard_wait", setup: func(f *routingTierFixture) {
			f.concurrency.acquireResults[116] = false
		}, want: 115},
		{name: "full_specific_retains_wildcard_sticky", setup: func(f *routingTierFixture) {
			f.concurrency.loadMap[115].LoadRate = 100
		}, want: 116},
		{name: "specific_slot_race_retains_wildcard_sticky", setup: func(f *routingTierFixture) {
			f.concurrency.acquireResults[115] = false
		}, want: 116},
		{name: "sticky_can_acquire_despite_stale_full_load", setup: func(f *routingTierFixture) {
			f.concurrency.loadMap[115].LoadRate = 100
			f.concurrency.loadMap[116].LoadRate = 100
		}, want: 116},
		{name: "unavailable_specific_allows_wildcard_sticky_wait", setup: func(f *routingTierFixture) {
			f.repo.accountsByID[115].Status = StatusDisabled
			f.concurrency.acquireResults[116] = false
		}, want: 116, wantWait: true},
		{name: "full_sticky_queue_allows_another_wildcard", setup: func(f *routingTierFixture) {
			f.concurrency.acquireResults[115] = false
			f.concurrency.acquireResults[116] = false
			f.concurrency.waitCounts[116] = 3
		}, want: 117},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRoutingTierFixture(true)
			tc.setup(fixture)
			selection, err := fixture.selectAccount("load-aware", "claude-opus-4-7")
			require.NoError(t, err)
			require.NotNil(t, selection)
			if selection.ReleaseFunc != nil {
				t.Cleanup(selection.ReleaseFunc)
			}
			require.Equal(t, tc.want, selection.Account.ID)
			if tc.wantWait {
				require.NotNil(t, selection.WaitPlan)
				require.False(t, selection.Acquired)
			} else {
				require.Nil(t, selection.WaitPlan)
				require.True(t, selection.Acquired)
			}
		})
	}
}

type routingTierFixture struct {
	svc         *GatewayService
	groupID     int64
	group       *Group
	repo        *mockAccountRepoForPlatform
	cache       *mockGatewayCacheForPlatform
	concurrency *mockConcurrencyCache
	excluded    map[int64]struct{}
}

func newRoutingTierFixture(loadAware bool) *routingTierFixture {
	groupID := int64(1)
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 115, Platform: PlatformAnthropic, Priority: 10, Status: StatusActive, Schedulable: true, Concurrency: 5, AccountGroups: []AccountGroup{{GroupID: groupID}}},
			{ID: 116, Platform: PlatformAnthropic, Priority: 10, Status: StatusActive, Schedulable: true, Concurrency: 5, AccountGroups: []AccountGroup{{GroupID: groupID}}},
			{ID: 117, Platform: PlatformAnthropic, Priority: 0, Status: StatusActive, Schedulable: true, Concurrency: 5, AccountGroups: []AccountGroup{{GroupID: groupID}}},
		},
		accountsByID: make(map[int64]*Account),
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	group := &Group{
		ID: groupID, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true,
		ModelRoutingEnabled: true,
		ModelRouting:        map[string][]int64{"claude-opus-*": {115}, "*": {116, 117}},
	}
	cache := &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{"tier-session": 116}}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = loadAware
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = time.Second
	svc := &GatewayService{
		accountRepo: repo,
		groupRepo:   &mockGroupRepoForGateway{groups: map[int64]*Group{groupID: group}},
		cache:       cache,
		cfg:         cfg,
	}
	concurrency := &mockConcurrencyCache{
		acquireResults: map[int64]bool{115: true, 116: true, 117: true},
		waitCounts:     make(map[int64]int),
		loadMap: map[int64]*AccountLoadInfo{
			115: {AccountID: 115, LoadRate: 0},
			116: {AccountID: 116, LoadRate: 0},
			117: {AccountID: 117, LoadRate: 0},
		},
	}
	if loadAware {
		svc.concurrencyService = NewConcurrencyService(concurrency)
	}
	return &routingTierFixture{svc: svc, groupID: groupID, group: group, repo: repo, cache: cache, concurrency: concurrency}
}

func (f *routingTierFixture) selectAccount(path, model string) (*AccountSelectionResult, error) {
	ctx := context.Background()
	if path == "legacy-single" {
		account, err := f.svc.selectAccountForModelWithPlatform(ctx, &f.groupID, "tier-session", model, f.excluded, PlatformAnthropic)
		return &AccountSelectionResult{Account: account}, err
	}
	return f.svc.SelectAccountWithLoadAwareness(ctx, &f.groupID, "tier-session", model, f.excluded, "", 0)
}
