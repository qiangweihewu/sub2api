package service

import "time"

// tempUnschedBackoffSequence is the duration sequence applied to repeated
// temp-unschedulable triggers. Position 0 is the first strike; the last
// entry saturates on further strikes until a fresh-start resets.
var tempUnschedBackoffSequence = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
	60 * time.Minute,
}

// tempUnschedFreshStartWindow — if more than this elapses between the last
// natural recovery and the next strike, the streak resets to step 0.
const tempUnschedFreshStartWindow = 5 * time.Minute

// computeNextTempUnschedDuration decides the next unsched duration and the
// step index to persist.
//
//   - curStep  — current account.temp_unsched_step_index (nil = no streak)
//   - lastRec  — current account.temp_unsched_last_recovered_at
//   - now      — the trigger time
//
// Returns (duration, nextStepIndex).
func computeNextTempUnschedDuration(curStep *int, lastRec *time.Time, now time.Time) (time.Duration, int) {
	isFreshStart := curStep == nil ||
		(lastRec != nil && now.Sub(*lastRec) > tempUnschedFreshStartWindow)

	var next int
	if isFreshStart {
		next = 0
	} else {
		next = *curStep + 1
		if next >= len(tempUnschedBackoffSequence) {
			next = len(tempUnschedBackoffSequence) - 1
		}
	}
	return tempUnschedBackoffSequence[next], next
}
