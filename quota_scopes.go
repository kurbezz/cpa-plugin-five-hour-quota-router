package main

import (
	"strings"
	"time"
)

type quotaModelFamily string

const (
	quotaFamilyUnknown quotaModelFamily = ""
	quotaFamilyFable   quotaModelFamily = "fable"
	quotaFamilyOpus    quotaModelFamily = "opus"
	quotaFamilySonnet  quotaModelFamily = "sonnet"
)

func familyTokens(s string) map[quotaModelFamily]bool {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
	out := map[quotaModelFamily]bool{}
	for _, x := range fields {
		switch x {
		case "fable":
			out[quotaFamilyFable] = true
		case "opus":
			out[quotaFamilyOpus] = true
		case "sonnet":
			out[quotaFamilySonnet] = true
		}
	}
	return out
}

func resolveQuotaModelFamily(model string) quotaModelFamily {
	m := familyTokens(model)
	if len(m) != 1 {
		return quotaFamilyUnknown
	}
	for f := range m {
		return f
	}
	return quotaFamilyUnknown
}

func resolveQuotaScopeFamily(id, display string) quotaModelFamily {
	all := familyTokens(id)
	for family := range familyTokens(display) {
		all[family] = true
	}
	if len(all) != 1 {
		return quotaFamilyUnknown
	}
	for family := range all {
		return family
	}
	return quotaFamilyUnknown
}

type quotaWindow struct {
	Percent   float64
	ResetAt   time.Time
	SampledAt time.Time
	Source    string
	Valid     bool
}

type quotaWindowBatch struct{ FiveHour, Weekly, FableWeekly, OpusWeekly, SonnetWeekly quotaWindow }
type quotaDecision struct {
	Excluded, ConfirmedExhausted, RecoveryKnown bool
	RecoveryAt                                  time.Time
}

func evaluateQuotaDecision(batch quotaWindowBatch, family quotaModelFamily, now time.Time, cutoff float64) quotaDecision {
	windows := []quotaWindow{batch.FiveHour, batch.Weekly}
	switch family {
	case quotaFamilyFable:
		windows = append(windows, batch.FableWeekly)
	case quotaFamilyOpus:
		windows = append(windows, batch.OpusWeekly)
	case quotaFamilySonnet:
		windows = append(windows, batch.SonnetWeekly)
	}
	var d quotaDecision
	blocking, allBlockingResetsKnown := false, true
	for _, w := range windows {
		if !w.Valid {
			continue
		}
		if w.Percent < cutoff {
			continue
		}
		if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
			continue
		}
		blocking = true
		if w.ResetAt.IsZero() {
			allBlockingResetsKnown = false
		} else if w.ResetAt.After(d.RecoveryAt) {
			d.RecoveryAt = w.ResetAt
		}
	}
	d.Excluded = blocking || !batch.FiveHour.Valid
	d.ConfirmedExhausted = batch.FiveHour.Valid && blocking
	d.RecoveryKnown = d.ConfirmedExhausted && allBlockingResetsKnown && !d.RecoveryAt.IsZero()
	if !d.RecoveryKnown {
		d.RecoveryAt = time.Time{}
	}
	return d
}
