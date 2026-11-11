package controller

import (
	"time"

	"github.com/robfig/cron/v3"
)

// maxMissedRuns bounds how far back we look for missed schedule times.
const maxMissedRuns = 1000

// nextRuns returns the most recent schedule time in (since, now] that has not
// been run yet (zero if none) and the next schedule time after now.
func nextRuns(sched cron.Schedule, since, now time.Time) (missed, next time.Time) {
	for t, i := sched.Next(since), 0; !t.After(now) && i < maxMissedRuns; t, i = sched.Next(t), i+1 {
		missed = t
	}
	return missed, sched.Next(now)
}

// parseSchedule parses a standard 5-field cron expression (descriptors such as @daily are allowed).
func parseSchedule(spec string) (cron.Schedule, error) {
	return cron.ParseStandard(spec)
}
