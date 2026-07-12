package rates

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// readBody loads a recorded (SYNTHETIC, hand-authored -- DATA RULE 11) Yahoo chart
// response body from testdata/. No test in this package ever hits the network; the
// parser is exercised entirely against these bodies.
func readBody(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return b
}

// TestYahooParse: a well-formed chart body for the USDMXN=X pair yields exactly one
// rate carrying the pair's base/quote, the price as the value, and the rate_date
// DERIVED from the body's market timestamp (regularMarketTime) in UTC -- NOT the
// query date. The recorded body's timestamp 1704067200 is 2024-01-01 UTC.
func TestYahooParse(t *testing.T) {
	body := readBody(t, "yahoo_usdmxn.json")

	rate, err := parseYahoo(Pair{Base: "USD", Quote: "MXN"}, body)
	if err != nil {
		t.Fatalf("parseYahoo: %v", err)
	}
	if rate.Base != "USD" || rate.Quote != "MXN" {
		t.Errorf("pair = %s->%s, want USD->MXN", rate.Base, rate.Quote)
	}
	if rate.Value != 17.125 {
		t.Errorf("value = %v, want 17.125", rate.Value)
	}
	if rate.RateDate != "2024-01-01" {
		t.Errorf("rate_date = %q, want 2024-01-01 (from regularMarketTime, UTC)", rate.RateDate)
	}
	if rate.Source != SourceYahoo {
		t.Errorf("source = %q, want %q", rate.Source, SourceYahoo)
	}
}

// TestYahooParseEmpty: an empty result array (Yahoo returns this for an unknown or
// delisted symbol) is a clear typed error (ErrNoData), never a panic or a zero rate.
func TestYahooParseEmpty(t *testing.T) {
	body := readBody(t, "yahoo_empty.json")

	_, err := parseYahoo(Pair{Base: "USD", Quote: "MXN"}, body)
	if !errors.Is(err, ErrNoData) {
		t.Errorf("empty result: err = %v, want ErrNoData", err)
	}
}

// TestYahooParseError: an error object in the body (Yahoo's own error channel) is
// surfaced as ErrNoData rather than silently producing a rate.
func TestYahooParseError(t *testing.T) {
	body := readBody(t, "yahoo_error.json")

	_, err := parseYahoo(Pair{Base: "USD", Quote: "MXN"}, body)
	if !errors.Is(err, ErrNoData) {
		t.Errorf("error body: err = %v, want ErrNoData", err)
	}
}

// TestYahooParseMalformed: non-JSON garbage is a clean error (not a panic).
func TestYahooParseMalformed(t *testing.T) {
	_, err := parseYahoo(Pair{Base: "USD", Quote: "MXN"}, []byte("<html>not json</html>"))
	if err == nil {
		t.Error("malformed body: want an error, got nil")
	}
}

// TestYahooParseMissingPrice: a result with no regularMarketPrice (a Yahoo quirk we
// must tolerate defensively) is ErrNoData, not a zero-valued rate.
func TestYahooParseMissingPrice(t *testing.T) {
	body := []byte(`{"chart":{"result":[{"meta":{"symbol":"USDMXN=X","regularMarketTime":1704067200}}],"error":null}}`)
	_, err := parseYahoo(Pair{Base: "USD", Quote: "MXN"}, body)
	if !errors.Is(err, ErrNoData) {
		t.Errorf("missing price: err = %v, want ErrNoData", err)
	}
}
