package main

import (
	"testing"
	"time"
)

func TestModelFamily(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  quotaModelFamily
	}{
		{"claude-fable-5", quotaFamilyFable}, {"CLAUDE-FABLE-5-1", quotaFamilyFable}, {"claude-opus-4-1-20250805", quotaFamilyOpus}, {"claude-sonnet-4-6", quotaFamilySonnet}, {"custom-chat", quotaFamilyUnknown}, {"claude-sonnetish", quotaFamilyUnknown}, {"Fable", quotaFamilyFable}, {"Claude 3.5 Fable", quotaFamilyFable},
	} {
		if got := resolveQuotaModelFamily(tt.input); got != tt.want {
			t.Errorf("resolve %q=%q want %q", tt.input, got, tt.want)
		}
	}
	for _, pair := range [][2]string{{"opus", "Sonnet"}, {"opus-sonnet", "Opus"}, {"Opus", "Opus Sonnet"}} {
		if got := resolveQuotaScopeFamily(pair[0], pair[1]); got != quotaFamilyUnknown {
			t.Errorf("conflict %q/%q resolved %q", pair[0], pair[1], got)
		}
	}
	for _, pair := range [][2]string{{"custom-opus", "unknown"}, {"opus opus", "Opus"}} {
		if got := resolveQuotaScopeFamily(pair[0], pair[1]); got != quotaFamilyOpus {
			t.Errorf("same family %q/%q resolved %q", pair[0], pair[1], got)
		}
	}
}

func TestQuotaDecision(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	window := func(p float64, reset time.Time) quotaWindow {
		return quotaWindow{Percent: p, ResetAt: reset, SampledAt: now, Valid: true}
	}
	at := func(d time.Duration) time.Time { return now.Add(d) }
	assert := func(name string, b quotaWindowBatch, f quotaModelFamily, excluded, confirmed, recovery bool, want time.Time) {
		t.Helper()
		got := evaluateQuotaDecision(b, f, now, 95)
		if got.Excluded != excluded || got.ConfirmedExhausted != confirmed || got.RecoveryKnown != recovery || !got.RecoveryAt.Equal(want) {
			t.Errorf("%s: got %+v", name, got)
		}
	}
	healthy := window(10, at(time.Hour))
	assert("empty", quotaWindowBatch{}, quotaFamilyFable, true, false, false, time.Time{})
	assert("optional windows absent", quotaWindowBatch{FiveHour: healthy}, quotaFamilyFable, false, false, false, time.Time{})
	fable := quotaWindowBatch{FiveHour: healthy, FableWeekly: window(99, at(8*time.Hour))}
	assert("Fable scope", fable, quotaFamilyFable, true, true, true, at(8*time.Hour))
	for _, family := range []quotaModelFamily{quotaFamilySonnet, quotaFamilyOpus, quotaFamilyUnknown} {
		assert("Fable does not affect other family", fable, family, false, false, false, time.Time{})
	}
	shared := quotaWindowBatch{FiveHour: healthy, Weekly: window(99, at(2*time.Hour))}
	for _, family := range []quotaModelFamily{quotaFamilyFable, quotaFamilySonnet, quotaFamilyOpus, quotaFamilyUnknown} {
		assert("shared weekly applies", shared, family, true, true, true, at(2*time.Hour))
	}
	assert("healthy reset ignored", quotaWindowBatch{FiveHour: healthy, Weekly: window(10, at(12*time.Hour)), FableWeekly: window(99, at(3*time.Hour))}, quotaFamilyFable, true, true, true, at(3*time.Hour))
	assert("account A recovery is max blocking reset", quotaWindowBatch{FiveHour: window(99, at(time.Hour)), FableWeekly: window(99, at(8*time.Hour))}, quotaFamilyFable, true, true, true, at(8*time.Hour))
	assert("account B recovery", quotaWindowBatch{FiveHour: healthy, FableWeekly: window(99, at(3*time.Hour))}, quotaFamilyFable, true, true, true, at(3*time.Hour))
	assert("exhausted five hour missing reset", quotaWindowBatch{FiveHour: window(99, time.Time{})}, quotaFamilyFable, true, true, false, time.Time{})
	assert("two blockers one reset", quotaWindowBatch{FiveHour: window(99, time.Time{}), FableWeekly: window(99, at(3*time.Hour))}, quotaFamilyFable, true, true, false, time.Time{})
	assert("unknown five hour plus weekly blocker", quotaWindowBatch{Weekly: window(99, at(time.Hour))}, quotaFamilyFable, true, false, false, time.Time{})
	assert("expired reset", quotaWindowBatch{FiveHour: window(99, now)}, quotaFamilyFable, false, false, false, time.Time{})
}
