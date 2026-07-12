package rates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"cuento/internal/store"
)

// SourceYahoo labels rates fetched from Yahoo Finance in exchange_rates.source, so
// the audit trail records the origin (vs "manual" CSV backfill).
const SourceYahoo = "yahoo"

// yahooBaseURL is Yahoo Finance's UNOFFICIAL chart endpoint. It is prefixed to a
// "<BASE><QUOTE>=X" FX symbol (e.g. USDMXN=X). Unofficial => it will break someday;
// keeping it here (behind RateSource) confines the breakage to this file. Settable
// on the Yahoo value so a test can point Fetch at a local httptest server instead of
// the network (the parser itself needs no network at all).
const yahooBaseURL = "https://query1.finance.yahoo.com/v8/finance/chart/"

// Yahoo is the RateSource backed by Yahoo Finance's chart endpoint. BaseURL and
// Client are settable so tests can inject a local server / stub transport; zero
// values fall back to the real endpoint and a default client with a timeout.
type Yahoo struct {
	BaseURL string
	Client  *http.Client
}

// NewYahoo returns a Yahoo source with a bounded-timeout HTTP client. The timeout
// keeps a hung unofficial endpoint from stalling `ratesync` (which a systemd timer
// runs unattended, p18).
func NewYahoo() *Yahoo {
	return &Yahoo{
		BaseURL: yahooBaseURL,
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Fetch pulls each pair from Yahoo and parses the body into a store.Rate. A pair
// Yahoo has no data for (ErrNoData) is SKIPPED, not fatal -- one delisted symbol
// must not sink the whole batch. A transport/HTTP error (network down, non-200) IS
// returned so the caller writes nothing rather than a partial batch.
func (y *Yahoo) Fetch(ctx context.Context, pairs []Pair) ([]store.Rate, error) {
	base := y.BaseURL
	if base == "" {
		base = yahooBaseURL
	}
	client := y.Client
	if client == nil {
		client = http.DefaultClient
	}

	var out []store.Rate
	for _, p := range pairs {
		body, err := fetchBody(ctx, client, base, p)
		if err != nil {
			return nil, fmt.Errorf("fetch %s%s: %w", p.Base, p.Quote, err)
		}
		rate, err := parseYahoo(p, body)
		if err != nil {
			// No data for this pair (unknown/delisted symbol, missing price):
			// skip it rather than failing the whole run.
			continue
		}
		out = append(out, rate)
	}
	return out, nil
}

// fetchBody performs the single HTTP GET for one pair against base+"<BASE><QUOTE>=X"
// and returns the raw body. A non-200 status is an error (so the caller aborts the
// batch); JSON shape is the parser's problem, not this shim's.
func fetchBody(ctx context.Context, client *http.Client, base string, p Pair) ([]byte, error) {
	url := base + p.Base + p.Quote + "=X"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

// yahooChart is the minimal shape of the chart response we depend on. Pointers on
// the optional-in-practice fields let the parser tell "field absent" from "field
// zero" -- an unofficial endpoint omits things, and a zero price must NOT be read
// as a real 0.0 rate.
type yahooChart struct {
	Chart struct {
		Result []struct {
			Meta struct {
				RegularMarketTime  *int64   `json:"regularMarketTime"`
				RegularMarketPrice *float64 `json:"regularMarketPrice"`
			} `json:"meta"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	} `json:"chart"`
}

// parseYahoo turns one recorded chart body into a store.Rate for the given pair.
// This is the TESTED unit (yahoo_test.go against synthetic testdata/ bodies): no
// network involved. It is defensive because the endpoint is unofficial --
//
//   - non-JSON body           -> a wrapped decode error;
//   - Yahoo error channel set  -> ErrNoData;
//   - empty result array       -> ErrNoData;
//   - missing price / time     -> ErrNoData (never a zero-valued or dateless rate).
//
// The rate_date is DERIVED from the body's regularMarketTime (Unix seconds) rendered
// in UTC, so the stored date reflects the market's timestamp, not the machine clock.
func parseYahoo(p Pair, body []byte) (store.Rate, error) {
	var c yahooChart
	if err := json.Unmarshal(body, &c); err != nil {
		return store.Rate{}, fmt.Errorf("decode yahoo body: %w", err)
	}
	// Yahoo's own error channel: a non-null, non-empty error means no data.
	if len(c.Chart.Error) > 0 && string(c.Chart.Error) != "null" {
		return store.Rate{}, fmt.Errorf("%s%s: %w", p.Base, p.Quote, ErrNoData)
	}
	if len(c.Chart.Result) == 0 {
		return store.Rate{}, fmt.Errorf("%s%s: empty result: %w", p.Base, p.Quote, ErrNoData)
	}
	meta := c.Chart.Result[0].Meta
	if meta.RegularMarketPrice == nil || meta.RegularMarketTime == nil {
		return store.Rate{}, fmt.Errorf("%s%s: missing price or time: %w", p.Base, p.Quote, ErrNoData)
	}

	rateDate := time.Unix(*meta.RegularMarketTime, 0).UTC().Format("2006-01-02")
	return store.Rate{
		RateDate: rateDate,
		Base:     p.Base,
		Quote:    p.Quote,
		Value:    *meta.RegularMarketPrice,
		Source:   SourceYahoo,
	}, nil
}
