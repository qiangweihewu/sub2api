//go:build unit

package service

import (
	"testing"
	"time"
)

func TestComputeNextTempUnschedDuration_FreshStart(t *testing.T) {
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	d, step := computeNextTempUnschedDuration(nil, nil, now)
	if d != 1*time.Minute {
		t.Fatalf("fresh start duration = %v, want 1m", d)
	}
	if step != 0 {
		t.Fatalf("fresh start step = %d, want 0", step)
	}
}

func TestComputeNextTempUnschedDuration_StepUpWithinWindow(t *testing.T) {
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		curStep  int
		lastRec  time.Time
		wantDur  time.Duration
		wantStep int
	}{
		{0, now.Add(-2 * time.Minute), 5 * time.Minute, 1},
		{1, now.Add(-2 * time.Minute), 15 * time.Minute, 2},
		{2, now.Add(-2 * time.Minute), 30 * time.Minute, 3},
		{3, now.Add(-2 * time.Minute), 60 * time.Minute, 4},
		{4, now.Add(-2 * time.Minute), 60 * time.Minute, 4}, // saturate
	}
	for _, c := range cases {
		cur := c.curStep
		rec := c.lastRec
		d, s := computeNextTempUnschedDuration(&cur, &rec, now)
		if d != c.wantDur || s != c.wantStep {
			t.Errorf("cur=%d → got (%v,%d), want (%v,%d)", c.curStep, d, s, c.wantDur, c.wantStep)
		}
	}
}

func TestComputeNextTempUnschedDuration_FreshAfterQuietWindow(t *testing.T) {
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	cur := 4
	rec := now.Add(-6 * time.Minute)
	d, s := computeNextTempUnschedDuration(&cur, &rec, now)
	if d != 1*time.Minute || s != 0 {
		t.Errorf("quiet window reset → got (%v,%d), want (1m,0)", d, s)
	}
}

func TestComputeNextTempUnschedDuration_NilLastRecoveredTreatedAsInStreak(t *testing.T) {
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	cur := 2
	// step_index set but never recovered — only happens when a retrigger fires
	// before the window expires. Continue the streak (safe default).
	d, s := computeNextTempUnschedDuration(&cur, nil, now)
	if d != 30*time.Minute || s != 3 {
		t.Errorf("step=2 nil lastRec → got (%v,%d), want (30m,3)", d, s)
	}
}
