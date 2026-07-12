package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cuento/internal/i18n"
)

// p10.3 tests the reusable form-error convention: on a failed POST validation the
// handler returns HTTP 422 and re-renders ONLY the form region as an htmx partial
// (not a full page), with LOCALIZED field errors (Go returns i18n error KEYS; the
// template renders them via {{t}}) and `autofocus` on the FIRST invalid field.
// Input ids stay stable across the swap (anti-jank, Appendix C).
//
// The demonstrator is a POST on the -dev-only /styleguide (Public), so it goes
// through the real registry + middleware without adding a production endpoint or a
// permission-matrix entry. It is exercised here through the real mounted dev app.

// postStyleguide submits the demonstrator form to the dev app and returns the
// recorder. It sets the htmx request header the real client would send, though the
// convention does not depend on it (the partial renders regardless).
func postStyleguide(t *testing.T, h http.Handler, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/styleguide", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestFormErrorPartial: an invalid POST returns 422 with the FORM-REGION PARTIAL
// (a fragment, not a full <html> page), carrying the LOCALIZED field error(s)
// (resolved via the catalog, never the raw key) and `autofocus` on the first
// invalid field. A valid POST returns success (200), not 422.
func TestFormErrorPartial(t *testing.T) {
	dh, _, _ := newDevApp(t)

	// Invalid: empty name (required) AND a bad email. The first invalid field is
	// name, so autofocus lands on the name input, not email.
	rec := postStyleguide(t, dh, "name=&email=notanemail")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid POST status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()

	// It is the form-region FRAGMENT, not a full page: no doctype / <html> / <nav>.
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "<html") || strings.Contains(body, "<nav") {
		t.Errorf("422 body is a full page, want only the form-region partial:\n%s", body)
	}
	// It must be the form region itself.
	if !strings.Contains(body, `id="demo-form"`) {
		t.Errorf("422 body missing the stable form region id=\"demo-form\":\n%s", body)
	}

	// LOCALIZED field errors: the catalog strings, never the raw keys.
	wantRequired := i18n.T("en", "error.required")
	wantEmail := i18n.T("en", "error.email")
	if !strings.Contains(body, wantRequired) {
		t.Errorf("422 body missing localized required error %q:\n%s", wantRequired, body)
	}
	if !strings.Contains(body, wantEmail) {
		t.Errorf("422 body missing localized email error %q:\n%s", wantEmail, body)
	}
	if strings.Contains(body, "error.required") || strings.Contains(body, "error.email") {
		t.Errorf("422 body leaks a raw i18n error KEY (should render via {{t}}):\n%s", body)
	}

	// autofocus on the FIRST invalid field (name), and ONLY there (not email too).
	if got := strings.Count(body, "autofocus"); got != 1 {
		t.Errorf("422 body has %d autofocus attributes, want exactly 1 (first invalid field):\n%s", got, body)
	}
	nameIdx := strings.Index(body, `id="demo-name"`)
	emailIdx := strings.Index(body, `id="demo-email"`)
	focusIdx := strings.Index(body, "autofocus")
	if nameIdx < 0 || emailIdx < 0 {
		t.Fatalf("422 body missing stable input ids demo-name/demo-email:\n%s", body)
	}
	// The single autofocus must sit within the name field's markup (before email).
	if focusIdx <= nameIdx || focusIdx >= emailIdx {
		t.Errorf("autofocus (idx %d) not on the first invalid field name (idx %d, before email idx %d):\n%s",
			focusIdx, nameIdx, emailIdx, body)
	}

	// Localized errors resolve for es too (the convention renders through {{t}} for
	// the request language): the same invalid POST under ?lang=es shows es strings.
	esReq := httptest.NewRequest(http.MethodPost, "/styleguide?lang=es", strings.NewReader("name=&email=x"))
	esReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	esRec := httptest.NewRecorder()
	dh.ServeHTTP(esRec, esReq)
	if esRec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("es invalid POST status = %d, want 422", esRec.Code)
	}
	esBody := esRec.Body.String()
	esRequired := i18n.T("es", "error.required")
	if esRequired == wantRequired {
		t.Fatalf("catalog precondition: en and es error.required are equal (%q)", esRequired)
	}
	if !strings.Contains(esBody, esRequired) {
		t.Errorf("es 422 body missing es required error %q:\n%s", esRequired, esBody)
	}

	// VALID POST: a non-empty name and a well-formed email -> success (200), not 422.
	okRec := postStyleguide(t, dh, "name=Ada&email=ada@example.org")
	if okRec.Code == http.StatusUnprocessableEntity {
		t.Fatalf("valid POST returned 422, want success (200):\n%s", okRec.Body.String())
	}
	if okRec.Code != http.StatusOK {
		t.Fatalf("valid POST status = %d, want 200", okRec.Code)
	}
	if okBody := okRec.Body.String(); !strings.Contains(okBody, i18n.T("en", "form.demo.ok")) {
		t.Errorf("valid POST missing the success message:\n%s", okBody)
	}
}
