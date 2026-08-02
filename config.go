package main

import "time"

// ─── Tunable configuration ──────────────────────────────────────────────────

var cfg = struct {
	ParallelWorkers    int
	StaggerMinMS       int
	StaggerMaxMS       int
	HTTPTimeoutShort   time.Duration
	HTTPTimeoutMedium  time.Duration
	PollReceiptMax     int
	ShortSleep         time.Duration
	MaxWaitSeconds     float64
	MaxPrice           float64
	MaxPriceFallback   float64
	FastMode           bool
	SummaryOnly        bool
	HardcodedPhone     string
	SiteRemoval        bool
	SingleProxyAttempt bool
	// Single-site / API path (target ~10-25s total)
	SingleSiteProbeSec    int
	SingleSiteCheckoutSec int
}{
	ParallelWorkers:    32,
	StaggerMinMS:       30,
	StaggerMaxMS:       80,
	HTTPTimeoutShort:   8 * time.Second,
	HTTPTimeoutMedium:  10 * time.Second,
	PollReceiptMax:     4,
	ShortSleep:         80 * time.Millisecond,
	MaxWaitSeconds:     2.0,
	MaxPrice:           25.0,
	MaxPriceFallback:   50.0,
	FastMode:           true,
	SummaryOnly:        true,
	HardcodedPhone:     "2494851515",
	SiteRemoval:        true,
	SingleProxyAttempt: true,
	SingleSiteProbeSec:    12,
	SingleSiteCheckoutSec: 18,
}
