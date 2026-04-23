//go:build integration

package service

import (
	"context"
	"testing"
	"time"
)

// fakeAccountRepoForBackoff records the most recent SetTempUnschedulableWithStep call.
type fakeAccountRepoForBackoff struct {
	AccountRepository
	lastUntil     time.Time
	lastStepIndex int
	lastReason    string
	callCount     int
}

func (r *fakeAccountRepoForBackoff) SetTempUnschedulableWithStep(_ context.Context, _ int64, until time.Time, reason string, stepIndex int) error {
	r.callCount++
	r.lastUntil = until
	r.lastStepIndex = stepIndex
	r.lastReason = reason
	return nil
}

// TestBackoffStreakProgression verifies that repeated calls to
// triggerTempUnschedulableWithBackoff climb the backoff ladder and
// that a quiet window resets the streak to step 0.
func TestBackoffStreakProgression(t *testing.T) {
	ctx := context.Background()
	repo := &fakeAccountRepoForBackoff{}
	svc := &RateLimitService{accountRepo: repo}

	acct := &Account{ID: 42}

	// --- Strike 1: fresh start → step 0, 1 minute ---
	beforeT1 := time.Now()
	ok := svc.triggerTempUnschedulableWithBackoff(ctx, acct, 400, "kw", "reason1", nil, -1)
	if !ok {
		t.Fatal("strike 1 returned false")
	}
	if repo.lastStepIndex != 0 {
		t.Errorf("strike 1 step = %d, want 0", repo.lastStepIndex)
	}
	if d := repo.lastUntil.Sub(beforeT1); d < 59*time.Second || d > 61*time.Second {
		t.Errorf("strike 1 duration = %v, want ~1m", d)
	}

	// Simulate: write propagates to account, then window naturally expires
	// and scheduler stamps last_recovered_at.
	stepAfter1 := 0
	recoveredAfter1 := time.Now()
	acct.TempUnschedStepIndex = &stepAfter1
	acct.TempUnschedLastRecoveredAt = &recoveredAfter1

	// --- Strike 2 within fresh-start window → step 1, 5 minutes ---
	beforeT2 := time.Now()
	ok = svc.triggerTempUnschedulableWithBackoff(ctx, acct, 400, "kw", "reason2", nil, -1)
	if !ok {
		t.Fatal("strike 2 returned false")
	}
	if repo.lastStepIndex != 1 {
		t.Errorf("strike 2 step = %d, want 1", repo.lastStepIndex)
	}
	if d := repo.lastUntil.Sub(beforeT2); d < 299*time.Second || d > 301*time.Second {
		t.Errorf("strike 2 duration = %v, want ~5m", d)
	}

	// Strike 3: step 1 → 2, 15 min
	stepAfter2 := 1
	recoveredAfter2 := time.Now()
	acct.TempUnschedStepIndex = &stepAfter2
	acct.TempUnschedLastRecoveredAt = &recoveredAfter2
	beforeT3 := time.Now()
	_ = svc.triggerTempUnschedulableWithBackoff(ctx, acct, 400, "kw", "reason3", nil, -1)
	if repo.lastStepIndex != 2 {
		t.Errorf("strike 3 step = %d, want 2", repo.lastStepIndex)
	}
	if d := repo.lastUntil.Sub(beforeT3); d < 15*time.Minute-time.Second || d > 15*time.Minute+time.Second {
		t.Errorf("strike 3 duration = %v, want ~15m", d)
	}

	// Strike 4: step 2 → 3, 30 min
	stepAfter3 := 2
	recoveredAfter3 := time.Now()
	acct.TempUnschedStepIndex = &stepAfter3
	acct.TempUnschedLastRecoveredAt = &recoveredAfter3
	beforeT4 := time.Now()
	_ = svc.triggerTempUnschedulableWithBackoff(ctx, acct, 400, "kw", "reason4", nil, -1)
	if repo.lastStepIndex != 3 {
		t.Errorf("strike 4 step = %d, want 3", repo.lastStepIndex)
	}
	if d := repo.lastUntil.Sub(beforeT4); d < 30*time.Minute-time.Second || d > 30*time.Minute+time.Second {
		t.Errorf("strike 4 duration = %v, want ~30m", d)
	}

	// Strike 5: step 3 → 4, 60 min (saturation)
	stepAfter4 := 3
	recoveredAfter4 := time.Now()
	acct.TempUnschedStepIndex = &stepAfter4
	acct.TempUnschedLastRecoveredAt = &recoveredAfter4
	beforeT5 := time.Now()
	_ = svc.triggerTempUnschedulableWithBackoff(ctx, acct, 400, "kw", "reason5", nil, -1)
	if repo.lastStepIndex != 4 {
		t.Errorf("strike 5 step = %d, want 4", repo.lastStepIndex)
	}
	if d := repo.lastUntil.Sub(beforeT5); d < 60*time.Minute-time.Second || d > 60*time.Minute+time.Second {
		t.Errorf("strike 5 duration = %v, want ~60m", d)
	}

	// Strike 6: step 4 → still 4 (saturated), still 60 min
	stepAfter5 := 4
	recoveredAfter5 := time.Now()
	acct.TempUnschedStepIndex = &stepAfter5
	acct.TempUnschedLastRecoveredAt = &recoveredAfter5
	_ = svc.triggerTempUnschedulableWithBackoff(ctx, acct, 400, "kw", "reason6", nil, -1)
	if repo.lastStepIndex != 4 {
		t.Errorf("saturated strike step = %d, want 4", repo.lastStepIndex)
	}

	// Fresh-start reset: simulate > 5 minutes quiet since last recovery
	stepAfter6 := 4
	longAgo := time.Now().Add(-10 * time.Minute)
	acct.TempUnschedStepIndex = &stepAfter6
	acct.TempUnschedLastRecoveredAt = &longAgo
	beforeReset := time.Now()
	_ = svc.triggerTempUnschedulableWithBackoff(ctx, acct, 400, "kw", "reason-reset", nil, -1)
	if repo.lastStepIndex != 0 {
		t.Errorf("fresh-start reset step = %d, want 0", repo.lastStepIndex)
	}
	if d := repo.lastUntil.Sub(beforeReset); d < 59*time.Second || d > 61*time.Second {
		t.Errorf("fresh-start reset duration = %v, want ~1m", d)
	}
}
