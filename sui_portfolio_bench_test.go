package portfolio

import (
	"encoding/json"
	"os"
	"testing"
)

// Optional real-data benchmark: SUI_PORTFOLIO_INPUT points at a SuiPortfolioInput
// JSON dump. Skipped when unset so CI never depends on private fixtures.
func BenchmarkSuiDailyCalculateFromDump(b *testing.B) {
	path := os.Getenv("SUI_PORTFOLIO_INPUT")
	if path == "" {
		b.Skip("SUI_PORTFOLIO_INPUT not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	var input SuiPortfolioInput
	if err := json.Unmarshal(raw, &input); err != nil {
		b.Fatal(err)
	}
	calculator, err := NewSuiPortfolioCalculator(input)
	if err != nil {
		b.Fatal(err)
	}
	accounts := calculator.Accounts()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		event, err := calculator.Calculate(accounts[i%len(accounts)])
		if err != nil {
			b.Fatal(err)
		}
		if event.Account == "" {
			b.Fatal("empty event")
		}
	}
}
