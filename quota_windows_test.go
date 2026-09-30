package main

import (
	"testing"
	"time"
)

func TestModelFamily(t *testing.T) {
	tests := []struct {
		input string
		want  quotaModelFamily
	}{
		{"claude-fable-5", quotaFamilyFable}, {"CLAUDE-FABLE-5-1", quotaFamilyFable},
		{"claude-opus-4-1-20250805", quotaFamilyOpus}, {"claude-sonnet-4-6", quotaFamilySonnet},
		{"custom-chat", quotaFamilyUnknown}, {"claude-sonnetish", quotaFamilyUnknown},
		{"Fable", quotaFamilyFable}, {"Claude 3.5 Fable", quotaFamilyFable},
	}
	for _, tt := range tests {
		if got := resolveQuotaModelFamily(tt.input); got != tt.want {
			t.Errorf("resolve(%q)=%q want %q", tt.input, got, tt.want)
		}
	}
	if got := resolveQuotaScopeFamily("opus", "Sonnet"); got != quotaFamilyUnknown {
		t.Fatalf("conflict resolved to %q", got)
	}
}

func TestQuotaDecision(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	blocked := func(p float64, reset time.Time) quotaWindow {
		return quotaWindow{Percent: p, ResetAt: reset, SampledAt: now, Valid: true}
	}
	a := quotaWindowBatch{FiveHour: blocked(99, now.Add(time.Hour)), FableWeekly: blocked(99, now.Add(8*time.Hour))}
	b := quotaWindowBatch{FableWeekly: blocked(99, now.Add(3*time.Hour)), FiveHour: blocked(10, now.Add(time.Hour))}
	if d := evaluateQuotaDecision(a, quotaFamilyFable, now, 95); !d.Excluded || !d.ConfirmedExhausted || !d.RecoveryKnown || !d.RecoveryAt.Equal(now.Add(8*time.Hour)) {
		t.Fatalf("A decision: %+v", d)
	}
	if d := evaluateQuotaDecision(b, quotaFamilyFable, now, 95); !d.RecoveryAt.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("B decision: %+v", d)
	}
	if d := evaluateQuotaDecision(a, quotaFamilySonnet, now, 95); !d.Excluded || d.RecoveryAt.Equal(now.Add(8*time.Hour)) {
		t.Fatalf("Sonnet decision: %+v", d)
	}
	shared := quotaWindowBatch{Weekly: blocked(99, now.Add(time.Hour))}
	if d := evaluateQuotaDecision(shared, quotaFamilyUnknown, now, 95); !d.Excluded {
		t.Fatalf("shared weekly didn't block unknown: %+v", d)
	}
	if d := evaluateQuotaDecision(quotaWindowBatch{FiveHour: blocked(99, time.Time{})}, quotaFamilyFable, now, 95); d.ConfirmedExhausted || d.RecoveryKnown {
		t.Fatalf("missing reset confirmed: %+v", d)
	}
	if d := evaluateQuotaDecision(quotaWindowBatch{}, quotaFamilyFable, now, 95); d.Excluded || d.ConfirmedExhausted {
		t.Fatalf("missing windows blocked: %+v", d)
	}
	if d := evaluateQuotaDecision(quotaWindowBatch{FiveHour: blocked(99, now)}, quotaFamilyFable, now, 95); d.Excluded {
		t.Fatalf("expired window blocked: %+v", d)
	}
}
