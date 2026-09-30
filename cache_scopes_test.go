package main

import (
	"testing"
	"time"
)

func TestWindowMerge(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	c := quotaCache{samples: map[string]quotaSample{}}
	a := physicalClaudeAuth{ID: "a", Identity: "identity"}
	c.reconcile([]physicalClaudeAuth{a})
	g, inc, _ := c.bindingSnapshot(a)
	seed := quotaWindowBatch{
		FiveHour: quotaWindow{Percent: 20, ResetAt: now.Add(time.Hour), SampledAt: now.Add(-time.Second), Source: quotaSourceUsage, Valid: true},
		Weekly:   quotaWindow{Percent: 30, ResetAt: now.Add(24 * time.Hour), SampledAt: now.Add(-time.Second), Source: quotaSourceUsage, Valid: true},
	}
	if !c.commitWindowBatch(a, g, inc, seed, now.Add(-time.Second), now.Add(-2*time.Second), true) {
		t.Fatal("seed poll batch rejected")
	}
	if got := c.snapshotWindows("a"); got.FiveHour.Percent != 20 || got.Weekly.Percent != 30 {
		t.Fatalf("seed poll: %+v", got)
	}
	// A five-hour-only header arrives while a poll started at T is in flight.
	headerAt := now.Add(time.Second)
	if !c.commitWindowBatch(a, g, inc, quotaWindowBatch{FiveHour: quotaWindow{Percent: 40, ResetAt: now.Add(2 * time.Hour), SampledAt: headerAt, Source: quotaSourceHeaders, Valid: true}}, headerAt, time.Time{}, false) {
		t.Fatal("five-hour header rejected")
	}
	pollAt := now.Add(2 * time.Second)
	poll := quotaWindowBatch{
		FiveHour: quotaWindow{Percent: 99, ResetAt: now.Add(3 * time.Hour), Source: quotaSourceUsage, Valid: true},
		Weekly:   quotaWindow{Percent: 60, ResetAt: now.Add(48 * time.Hour), Source: quotaSourceUsage, Valid: true},
	}
	if !c.commitWindowBatch(a, g, inc, poll, pollAt, now, true) {
		t.Fatal("poll should commit its independent weekly slot")
	}
	got := c.snapshotWindows("a")
	if got.FiveHour.Percent != 40 || !got.FiveHour.SampledAt.Equal(headerAt) || !got.FiveHour.ResetAt.Equal(now.Add(2*time.Hour)) || got.Weekly.Percent != 60 || !got.Weekly.SampledAt.Equal(pollAt) || got.Weekly.Source != quotaSourceUsage {
		t.Fatalf("per-slot merge: %+v", got)
	}
	sample := c.snapshot("a")
	if sample.FiveHourPercentUsed != 40 || !sample.SampledAt.Equal(headerAt) || !sample.ResetAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("legacy five-hour fields changed on weekly poll: %+v", sample)
	}
	if !c.commitWindowBatch(a, g, inc, quotaWindowBatch{Weekly: quotaWindow{Percent: 90, Valid: true, SampledAt: now.Add(3 * time.Second), Source: quotaSourceHeaders}}, now.Add(3*time.Second), time.Time{}, false) {
		t.Fatal("independent weekly header rejected")
	}
	got = c.snapshotWindows("a")
	if got.Weekly.Percent != 90 || got.FiveHour.Percent != 40 {
		t.Fatalf("partial batch: %+v", got)
	}
	if c.commitWindowBatch(a, g, inc, quotaWindowBatch{FiveHour: quotaWindow{Percent: 1, Valid: true, SampledAt: now, Source: quotaSourceHeaders}}, now, time.Time{}, false) {
		t.Fatal("older five-hour header accepted")
	}
	if got = c.snapshotWindows("a"); got.FiveHour.Percent != 40 || got.Weekly.Percent != 90 {
		t.Fatalf("older header altered cache: %+v", got)
	}

	t.Run("poll tie loses to header", func(t *testing.T) {
		tieAt := now.Add(4 * time.Second)
		if !c.commitWindowBatch(a, g, inc, quotaWindowBatch{FiveHour: quotaWindow{Percent: 50, Valid: true, SampledAt: tieAt, Source: quotaSourceHeaders}}, tieAt, time.Time{}, false) {
			t.Fatal("tie-time header rejected")
		}
		if c.commitWindowBatch(a, g, inc, quotaWindowBatch{FiveHour: quotaWindow{Percent: 99, Valid: true, Source: quotaSourceUsage}}, tieAt.Add(2*time.Second), tieAt, true) {
			t.Fatal("poll tied at start overwrote header")
		}
		if got := c.snapshotWindows("a"); got.FiveHour.Percent != 50 {
			t.Fatalf("tie poll changed five-hour slot: %+v", got.FiveHour)
		}
	})
}

func TestWindowRefresh(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cfg := pluginConfig{CutoffPercentUsed: 95, PollInterval: time.Minute}
	c := quotaCache{samples: map[string]quotaSample{}}
	a := physicalClaudeAuth{ID: "a", Identity: "identity"}
	c.reconcile([]physicalClaudeAuth{a})
	g, inc, _ := c.bindingSnapshot(a)
	batch := quotaWindowBatch{FiveHour: quotaWindow{Percent: 10, Valid: true}, FableWeekly: quotaWindow{Percent: 99, Valid: true}}
	if !c.commitWindowBatch(a, g, inc, batch, now, time.Time{}, false) {
		t.Fatal("seed header batch failed")
	}
	if !c.usageRefreshDue("a", cfg, now.Add(30*time.Second)) {
		t.Fatal("header freshness incorrectly advanced poll-only clock")
	}
	if !c.claimPoll("a", now.Add(time.Minute), 95, time.Minute) {
		t.Fatal("model-only exhaustion vetoed poll")
	}
}

func TestUsageAttemptAndSuccessClocks(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cfg := pluginConfig{CutoffPercentUsed: 95, PollInterval: time.Minute}
	c := quotaCache{samples: map[string]quotaSample{}}
	a := physicalClaudeAuth{ID: "clock", Identity: "id"}
	c.reconcile([]physicalClaudeAuth{a})
	g, inc, _ := c.bindingSnapshot(a)
	if !c.claimPoll("clock", now, 95, time.Minute) {
		t.Fatal("initial reservation rejected")
	}
	if c.claimPoll("clock", now.Add(30*time.Second), 95, time.Minute) {
		t.Fatal("reservation did not throttle retry")
	}
	if !c.recordUsageAttempt(a, g, inc, now) {
		t.Fatal("bound attempt was not recorded")
	}
	// A fresh five-hour header cannot advance the usage clock or postpone weekly refresh.
	headerAt := now.Add(30 * time.Second)
	if !c.commitWindowBatch(a, g, inc, quotaWindowBatch{FiveHour: quotaWindow{Percent: 20, SampledAt: headerAt, Source: quotaSourceHeaders, Valid: true}}, headerAt, time.Time{}, false) {
		t.Fatal("header commit failed")
	}
	weeklyHeaderAt := now.Add(time.Second)
	if !c.commitWindowBatch(a, g, inc, quotaWindowBatch{Weekly: quotaWindow{Percent: 30, SampledAt: weeklyHeaderAt, Source: quotaSourceHeaders, Valid: true}}, weeklyHeaderAt, time.Time{}, false) {
		t.Fatal("weekly header commit failed")
	}
	if c.usageRefreshDue("clock", cfg, now.Add(59*time.Second)) {
		t.Fatal("refresh became due before interval")
	}
	if !c.usageRefreshDue("clock", cfg, now.Add(time.Minute)) {
		t.Fatal("header postponed due usage refresh")
	}
	// A successful fetch counts even when every supplied window loses stale arbitration.
	if c.commitWindowBatch(a, g, inc, quotaWindowBatch{Weekly: quotaWindow{Percent: 60, SampledAt: now.Add(-time.Second), Source: quotaSourceUsage, Valid: true}}, now.Add(2*time.Minute), now, true) {
		t.Fatal("slot result reported changed despite stale arbitration")
	}
	s := c.snapshot("clock")
	if !s.UsageSuccessAt.Equal(now.Add(2*time.Minute)) || s.Windows.Weekly.Percent != 30 {
		t.Fatalf("fetch success/window arbitration mismatch: %+v", s)
	}
}

func TestBindingIncarnationGuardsWindows(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	c := quotaCache{samples: map[string]quotaSample{}}
	a := physicalClaudeAuth{ID: "binding", Identity: "identity"}
	c.reconcile([]physicalClaudeAuth{a})
	g, original, _ := c.bindingSnapshot(a)
	seed := quotaWindowBatch{
		FiveHour: quotaWindow{Percent: 10, Valid: true}, Weekly: quotaWindow{Percent: 20, Valid: true},
		FableWeekly: quotaWindow{Percent: 30, Valid: true}, OpusWeekly: quotaWindow{Percent: 40, Valid: true}, SonnetWeekly: quotaWindow{Percent: 50, Valid: true},
	}
	if !c.commitWindowBatch(a, g, original, seed, now, now.Add(-time.Second), true) {
		t.Fatal("seed five windows")
	}
	revision := "revision-one"
	bound, oldInc, ok := c.bindRevisionForIncarnation(a, revision, g, original)
	if !ok || oldInc != original {
		t.Fatalf("initial bind: incarnation=%d ok=%v", oldInc, ok)
	}
	// A revision change starts a new quota lifetime and clears all cached windows.
	_, ownInc, ok := c.bindRevisionForIncarnation(bound, "revision-two", g, oldInc)
	if !ok || ownInc == oldInc {
		t.Fatalf("own revision bind did not advance incarnation: old=%d new=%d ok=%v", oldInc, ownInc, ok)
	}
	if windows := c.snapshotWindows(a.ID); windows.FiveHour.Valid || windows.Weekly.Valid || windows.FableWeekly.Valid || windows.OpusWeekly.Valid || windows.SonnetWeekly.Valid {
		t.Fatalf("revision replacement retained quota windows: %+v", windows)
	}
	if c.commitWindowBatch(bound, g, original, seed, now.Add(time.Second), now, true) {
		t.Fatal("later replacement accepted old in-flight batch")
	}
	// Remove/re-add with the same ID and identity is still a distinct incarnation.
	c.reconcile(nil)
	c.reconcile([]physicalClaudeAuth{a})
	if _, _, ok := c.bindRevisionForIncarnation(a, "revision-three", g, ownInc); ok {
		t.Fatal("old binding survived remove/re-add")
	}
	if c.commitWindowBatch(a, g, ownInc, seed, now.Add(2*time.Second), now.Add(time.Second), true) {
		t.Fatal("old batch survived remove/re-add")
	}
}
