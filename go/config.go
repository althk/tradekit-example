package main

import (
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/harness"

	"github.com/joho/godotenv"
)

// config is loaded from the environment via harness.Overlay: struct tags
// declare the env var and whether it's required, and harness does the
// parsing and validation. tradekit itself has no dotenv support (see
// go/harness's own config.go), so .env is this example's own addition,
// loaded before the overlay runs.
type config struct {
	Mode string `env:"MODE"` // "live" or "backtest"

	APIKey    string         `env:"KITE_API_KEY" validate:"required"`
	APISecret harness.Secret `env:"KITE_API_SECRET" validate:"required"`
	// RedirectURL is the redirect registered on the Kite Connect app; the
	// login callback server listens on its port and path. See main.go.
	RedirectURL string `env:"KITE_REDIRECT_URL"`

	Exchange     string  `env:"SYMBOL_EXCHANGE"`
	Symbol       string  `env:"SYMBOL"`
	FastPeriod   int     `env:"FAST_SMA"`
	SlowPeriod   int     `env:"SLOW_SMA"`
	StopPct      float64 `env:"STOP_PCT"`
	RiskFraction float64 `env:"RISK_FRACTION"`
	MaxTrades    int     `env:"MAX_TRADES_PER_DAY"`
	DBPath       string  `env:"DB_PATH"`
	ReportPath   string  `env:"REPORT_PATH"`

	// money.Money fields read a decimal string ("2000.00"); Overlay parses
	// them through money.Parse, so bare paise are never mistaken for rupees.
	DailyLossLimit money.Money `env:"DAILY_LOSS_LIMIT"`
	BacktestCash   money.Money `env:"BACKTEST_CASH"`
}

func loadConfig() (config, error) {
	// Missing .env is fine — real environment variables still work, e.g. in CI.
	_ = godotenv.Load()

	cfg := config{
		Mode:           "live",
		RedirectURL:    "http://127.0.0.1:9880/kite/callback",
		Exchange:       "NSE",
		Symbol:         "RELIANCE",
		FastPeriod:     20,
		SlowPeriod:     50,
		StopPct:        0.03,
		RiskFraction:   0.01,
		MaxTrades:      3,
		DBPath:         "bot.db",
		ReportPath:     "report.html",
		DailyLossLimit: money.MustParse("2000.00"),
		BacktestCash:   money.MustParse("1000000.00"),
	}

	// Overlay applies every `env:"..."` tag on top of the defaults above and
	// fails with every missing `validate:"required"` field named at once,
	// rather than one at a time across repeated restarts.
	return cfg, harness.Overlay(&cfg)
}
