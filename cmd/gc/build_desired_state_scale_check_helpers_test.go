package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// Wall-clock wrappers over defaultScaleCheckCountsAndDemandAt, for tests whose
// subject is not the clock.
//
// They live in a _test.go file on purpose (OPS-78 review finding 4, agy
// 2026-09-17). They had no production caller, and a production function that
// samples time.Now() for a pass whose exclusions are time-dependent is the shape
// the "one pass reads one clock" rule exists to forbid — kept alive only by
// tests, it is an invitation for a future caller to reintroduce the split clock.
// A test that DOES care about the instant must call the -At form and name it.

func defaultScaleCheckCounts(targets []defaultScaleCheckTarget) (map[string]int, map[string]bool, []error) {
	counts, _, partialTemplates, errs := defaultScaleCheckCountsAndDemandAt(nil, targets, time.Now())
	return counts, partialTemplates, errs
}

func defaultScaleCheckCountsAndDemand(cfg *config.City, targets []defaultScaleCheckTarget, caches ...*readyDemandCache) (map[string]int, map[string]scaleCheckDemand, map[string]bool, []error) {
	return defaultScaleCheckCountsAndDemandAt(cfg, targets, time.Now(), caches...)
}
