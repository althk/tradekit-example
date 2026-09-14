module github.com/althk/tradekit-example/go

go 1.26.0

require (
	github.com/althk/tradekit/go/backtest v0.0.0-20260914102604-834b2a4779b0
	github.com/althk/tradekit/go/core v0.1.0
	github.com/althk/tradekit/go/harness v0.0.0-20260914102604-834b2a4779b0
	github.com/althk/tradekit/go/marketdata v0.0.0-20260914102604-834b2a4779b0
	github.com/althk/tradekit/go/store v0.0.0-20260914102604-834b2a4779b0
	github.com/althk/tradekit/go/zerodha v0.0.0-20260914102604-834b2a4779b0
	github.com/joho/godotenv v1.5.1
	modernc.org/sqlite v1.58.0
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/gocarina/gocsv v0.0.0-20260908110832-9ab82d65b1cc // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/zerodha/gokiteconnect/v4 v4.4.2 // indirect
	golang.org/x/sys v0.48.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

// tradekit has no tagged releases yet, so pull each module straight from the
// sibling checkout. Once it publishes tags, replace these with real versions
// (or `go get github.com/althk/tradekit/go/core@go/core/v0.1.0`, etc).
replace (
	github.com/althk/tradekit/go/backtest => ../../tradekit/go/backtest
	github.com/althk/tradekit/go/core => ../../tradekit/go/core
	github.com/althk/tradekit/go/harness => ../../tradekit/go/harness
	github.com/althk/tradekit/go/marketdata => ../../tradekit/go/marketdata
	github.com/althk/tradekit/go/store => ../../tradekit/go/store
	github.com/althk/tradekit/go/zerodha => ../../tradekit/go/zerodha
)
