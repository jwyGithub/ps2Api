package api

import (
	"testing"
	"time"

	"ps2api/internal/store"
)

func TestBuildQuotaForecastUsesRemainingDaysAndSuggestsAccounts(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	accounts := []*store.Account{
		{Enabled: true, QuotaLimit: 100, QuotaUsed: 75, QuotaRemaining: 25},
		{Enabled: true}, // 未采集账号不参与预测
		{Enabled: false, QuotaLimit: 100, QuotaUsed: 100},
	}

	forecast := buildQuotaForecast(accounts, now)
	if forecast.Status != "refill" || !forecast.NeedsRefill {
		t.Fatalf("status=%q needsRefill=%v", forecast.Status, forecast.NeedsRefill)
	}
	if forecast.DaysInMonth != 31 || forecast.DaysElapsed != 15 || forecast.DaysRemaining != 16 {
		t.Fatalf("period=%+v", forecast)
	}
	if forecast.ObservedAccounts != 1 || forecast.TotalAccounts != 3 {
		t.Fatalf("accounts=%+v", forecast)
	}
	if forecast.DailyConsumption != 5 || forecast.ForecastAdditional != 80 || forecast.Shortfall != 55 || forecast.SuggestedAccounts != 1 {
		t.Fatalf("forecast=%+v", forecast)
	}
}

func TestBuildQuotaForecastReportsInsufficientData(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	forecast := buildQuotaForecast([]*store.Account{{Enabled: true}}, now)
	if forecast.Status != "insufficient_data" || forecast.ObservedAccounts != 0 || forecast.NeedsRefill {
		t.Fatalf("forecast=%+v", forecast)
	}
}

func TestBuildQuotaForecastUsesOfficialCycleStart(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	cycleStart := time.Date(2026, 8, 13, 0, 0, 0, 0, now.Location())
	forecast := buildQuotaForecast([]*store.Account{{
		Enabled: true, QuotaLimit: 1000, QuotaUsed: 300, QuotaRemaining: 700, QuotaCycleStart: &cycleStart,
	}}, now)
	if forecast.DailyConsumption != 100 || forecast.ForecastAdditional != 1600 {
		t.Fatalf("cycle-based forecast=%+v", forecast)
	}
}
