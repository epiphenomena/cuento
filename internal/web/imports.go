package web

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"cuento/internal/bankimport"
	"cuento/internal/money"
	"cuento/internal/store"
)

// p17.2 bank-CSV import: upload -> mapping UI -> 20-row preview -> stage (D17,
// DECISIONS p17.2). A TxnWrite user uploads a bank statement CSV, assigns columns
// (which column is date/amount/payee/memo), picks the target account + subsidiary,
// chooses a delimiter / header / amount mode / sign-flip / date format (optionally
// saving the mapping as a reusable profile), previews the first 20 parsed rows, and
// confirms to STAGE the rows (batch + import_rows persisted, duplicates flagged).
// This step STOPS at staging: posting is p17.3 (the review queue). The multipart
// upload reuses p14.2's admin-rates pattern (stdlib r.FormFile / r.ParseMultipartForm).
//
// URL scheme (DECISIONS p17.2):
//   GET  /import          upload + mapping form
//   POST /import/preview  parse the multipart CSV under the mapping, show 20 rows
//   POST /import          confirm: create the batch, stage all rows (dupes flagged)
//
// Preview -> confirm carry: an htmx swap loses the <input type=file> selection, so
// the preview response carries the uploaded CSV forward as a base64 hidden field
// (StdEncoding, one token) under the same size cap, plus the mapping fields as
// hidden inputs; confirm RE-PARSES the carried bytes (preview-parse == stage-parse,
// deterministic) rather than asking the user to re-pick the file.
//
// All-or-nothing (point-8 decision): the parser flags a malformed row per-row
// without aborting the file, but staging has no 'error' status, so the PREVIEW
// handler rejects the WHOLE upload with a clean 422 (no batch created) if ANY row
// fails to parse. A duplicate row is NOT an error -- it stages, flagged advisory.

// maxImportUpload caps the multipart parse (and the decoded carried bytes) so a
// huge upload cannot exhaust memory. Bank statements are small; 2 MiB is generous.
const maxImportUpload = 2 << 20

// importUploadModel is the GET /import form model: the subsidiary + account option
// lists (subsidiary-first selection structurally guarantees the account maps to the
// subsidiary), the saved profiles offered for reuse, and a form-error slot.
type importUploadModel struct {
	Subsidiaries []importSubOption
	Accounts     []importAccountOption
	Profiles     []importProfileOption
	ErrorKey     string
	ErrorArg     string
}

// importSubOption is one subsidiary in the target select.
type importSubOption struct {
	ID   int64
	Name string
}

// importAccountOption is one leaf account in the target select. SubsidiaryIDs is
// stamped so the picker can (client-side, later) filter by the chosen subsidiary;
// the store re-validates on confirm regardless.
type importAccountOption struct {
	ID            int64
	Name          string
	SubsidiaryIDs []int64
}

// importProfileOption is one saved mapping profile the user may load.
type importProfileOption struct {
	ID   int64
	Name string
}

// importPreviewModel is the POST /import/preview result: the parsed rows (capped to
// 20 for DISPLAY -- staging persists all), the total parsed count, the carried CSV
// + mapping (as hidden fields for confirm), and the target labels.
type importPreviewModel struct {
	AccountID    int64
	AccountName  string
	SubsidiaryID int64
	SubsidiaryNm string
	Filename     string
	ProfileName  string
	SaveProfile  bool
	CSVBase64    string
	Mapping      bankimport.Config
	Rows         []importPreviewRow
	TotalRows    int
	ShownRows    int
	MoreRows     int // TotalRows - ShownRows (rows staged but not previewed)
	ErrorKey     string
	ErrorArg     string
}

// importPreviewRow is one previewed parsed row: formatted for display.
type importPreviewRow struct {
	Date      string
	AmountFmt string
	Payee     string
	Memo      string
}

// importResultModel is the POST /import confirm result: the persisted batch, the
// staged-row count, and the duplicate count (advisory), plus a per-row list marking
// duplicates.
type importResultModel struct {
	BatchID    int64
	Filename   string
	Staged     int
	Duplicates int
	Rows       []importResultRow
}

// importResultRow is one staged row in the confirm result: its display fields and
// whether it was flagged a duplicate.
type importResultRow struct {
	Date      string
	AmountFmt string
	Payee     string
	Memo      string
	Duplicate bool
}

// importPreviewCap is the number of parsed rows shown in the preview (all rows are
// still staged on confirm).
const importPreviewCap = 20

// importPage handles GET /import (TxnWrite): the upload + mapping form.
func (s *server) importPage(w http.ResponseWriter, r *http.Request) {
	model, err := s.buildImportUpload(r)
	if err != nil {
		s.serverError(w)
		return
	}
	s.render(w, r, http.StatusOK, "import.tmpl", s.newShellPage(r, model))
}

// buildImportUpload assembles the upload form options: subsidiaries, all leaf
// accounts (with their subsidiary sets), and saved profiles.
func (s *server) buildImportUpload(r *http.Request) (importUploadModel, error) {
	ctx := r.Context()
	lang := langOf(ctx)

	subs, err := s.store.SubTree(ctx)
	if err != nil {
		return importUploadModel{}, err
	}
	model := importUploadModel{}
	for _, sub := range subs {
		if sub.Active == 0 {
			continue
		}
		model.Subsidiaries = append(model.Subsidiaries, importSubOption{ID: sub.ID, Name: sub.Name})
	}

	// All leaf+active accounts (union across subsidiaries). AccountEditorOptions is
	// per-subsidiary; a nil-safe union is built by iterating each subsidiary once,
	// de-duplicating by account id.
	seen := make(map[int64]bool)
	for _, sub := range model.Subsidiaries {
		opts, err := s.store.AccountEditorOptions(ctx, lang, sub.ID)
		if err != nil {
			return importUploadModel{}, err
		}
		for _, o := range opts {
			if seen[o.ID] {
				continue
			}
			seen[o.ID] = true
			model.Accounts = append(model.Accounts, importAccountOption{
				ID: o.ID, Name: o.Name, SubsidiaryIDs: o.SubsidiaryIDs,
			})
		}
	}

	profiles, err := s.store.ListMappingProfiles(ctx)
	if err != nil {
		return importUploadModel{}, err
	}
	for _, p := range profiles {
		model.Profiles = append(model.Profiles, importProfileOption{ID: p.ID, Name: p.Name})
	}
	return model, nil
}

// mappingFrom resolves the Config for a request: when a saved profile is selected
// (profile_id set), it loads that profile's stored Config so a reused mapping needs
// no column re-entry; otherwise it reads the mapping form fields. The preview
// carries the resolved fields forward as hidden inputs, so the confirm step reads
// the SAME concrete field values (profile_id is not re-consulted on confirm --
// preview already flattened the profile into the carried fields).
func (s *server) mappingFrom(r *http.Request) bankimport.Config {
	if pid := parseID(r.FormValue("profile_id")); pid > 0 {
		if prof, err := s.store.GetMappingProfile(r.Context(), pid); err == nil {
			return prof.Config
		}
	}
	return importMapping(r)
}

// importMapping reads the mapping Config from the request form (present on both the
// preview POST and the confirm POST as either file-form fields or carried hiddens).
func importMapping(r *http.Request) bankimport.Config {
	return bankimport.Config{
		Delimiter: bankimport.Delimiter(r.FormValue("delimiter")),
		HasHeader: r.FormValue("has_header") == "1",
		Amount:    bankimport.AmountMode(r.FormValue("amount_mode")),
		SignFlip:  r.FormValue("sign_flip") == "1",
		DateFmt:   bankimport.DateLayout(r.FormValue("date_format")),
		DateCol:   atoiDefault(r.FormValue("date_col"), 0),
		AmountCol: atoiDefault(r.FormValue("amount_col"), 0),
		DebitCol:  atoiDefault(r.FormValue("debit_col"), 0),
		CreditCol: atoiDefault(r.FormValue("credit_col"), 0),
		PayeeCol:  atoiDefault(r.FormValue("payee_col"), -1),
		MemoCol:   atoiDefault(r.FormValue("memo_col"), -1),
	}
}

// atoiDefault parses s as an int, returning def on any error (unmapped optional
// columns arrive as "" -> the caller's -1 default).
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// importPreview handles POST /import/preview (TxnWrite): parse the uploaded CSV
// under the submitted mapping and show the first 20 parsed rows. ANY parse error
// (or a bad file) is a clean 422 with the whole upload rejected -- no batch is
// created here (batches are created only on confirm). On success the CSV is carried
// forward base64 for the confirm step.
func (s *server) importPreview(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxImportUpload); err != nil {
		s.renderImportError(w, r, "import.error.no_file", "")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		s.renderImportError(w, r, "import.error.no_file", "")
		return
	}
	defer func() { _ = file.Close() }()

	raw, err := io.ReadAll(io.LimitReader(file, maxImportUpload+1))
	if err != nil || len(raw) == 0 {
		s.renderImportError(w, r, "import.error.no_file", "")
		return
	}
	if len(raw) > maxImportUpload {
		s.renderImportError(w, r, "import.error.too_large", "")
		return
	}

	accountID := parseID(r.FormValue("account_id"))
	subsidiaryID := parseID(r.FormValue("subsidiary_id"))
	cfg := s.mappingFrom(r)

	model, key, arg := s.parseImportPreview(r, raw, accountID, subsidiaryID, cfg)
	if key != "" {
		s.renderImportError(w, r, key, arg)
		return
	}
	model.Filename = header.Filename
	model.ProfileName = r.FormValue("profile_name")
	model.SaveProfile = r.FormValue("save_profile") == "1"
	model.CSVBase64 = base64.StdEncoding.EncodeToString(raw)

	// The preview is swapped into #import-workspace via htmx innerHTML, so render
	// JUST the fragment (no shell).
	s.render(w, r, http.StatusOK, "import-preview", model)
}

// parseImportPreview parses raw under cfg for the account's currency exponent and
// builds the preview model, or returns a (key, arg) error to render at 422. It
// rejects the WHOLE file if any row has a parse error (all-or-nothing).
func (s *server) parseImportPreview(r *http.Request, raw []byte, accountID, subsidiaryID int64, cfg bankimport.Config) (importPreviewModel, string, string) {
	ctx := r.Context()
	u := currentUser(ctx)
	lang := langOf(ctx)

	if accountID == 0 || subsidiaryID == 0 {
		return importPreviewModel{}, "import.error.no_target", ""
	}

	acct, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return importPreviewModel{}, "import.error.no_target", ""
	}
	exp := s.currencyExponent(ctx, acct.DefaultCurrency)

	rows, perr := bankimport.Parse(raw, cfg, exp)
	if perr != nil {
		return importPreviewModel{}, "import.error.parse", ""
	}
	// All-or-nothing: any per-row error rejects the whole upload (no 'error' status
	// exists in the staging schema; a staged row must carry a parsed date+amount).
	for i, row := range rows {
		if row.Err != nil {
			return importPreviewModel{}, "import.error.row", strconv.Itoa(i + 1)
		}
	}

	opts := formatOptsFor(u)
	df := dateFormatFor(u)
	model := importPreviewModel{
		AccountID:    accountID,
		AccountName:  s.accountName(ctx, accountID, lang),
		SubsidiaryID: subsidiaryID,
		SubsidiaryNm: s.subsidiaryName(ctx, subsidiaryID),
		Mapping:      cfg,
		TotalRows:    len(rows),
	}
	shown := rows
	if len(shown) > importPreviewCap {
		shown = shown[:importPreviewCap]
	}
	model.ShownRows = len(shown)
	model.MoreRows = len(rows) - len(shown)
	for _, row := range shown {
		model.Rows = append(model.Rows, importPreviewRow{
			Date:      money.FormatDate(parseISOForDisplay(row.Date), df),
			AmountFmt: acct.DefaultCurrency + " " + money.Format(row.AmountMinor, exp, opts),
			Payee:     row.Payee,
			Memo:      row.Memo,
		})
	}
	return model, "", ""
}

// importConfirm handles POST /import (TxnWrite): re-parse the carried CSV, optionally
// save the mapping profile, create the batch (validating the account maps to the
// subsidiary), and stage all rows with duplicates flagged. The confirm POST is
// urlencoded (only the initial upload is multipart), carrying the base64 CSV.
func (s *server) importConfirm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderImportError(w, r, "import.error.parse", "")
		return
	}
	ctx := r.Context()

	raw, err := base64.StdEncoding.DecodeString(r.FormValue("csv_b64"))
	if err != nil || len(raw) == 0 {
		s.renderImportError(w, r, "import.error.no_file", "")
		return
	}
	// Never trust the client-sent hidden field's size: re-apply the cap to the
	// DECODED bytes.
	if len(raw) > maxImportUpload {
		s.renderImportError(w, r, "import.error.too_large", "")
		return
	}

	accountID := parseID(r.FormValue("account_id"))
	subsidiaryID := parseID(r.FormValue("subsidiary_id"))
	cfg := importMapping(r)

	acct, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		s.renderImportError(w, r, "import.error.no_target", "")
		return
	}
	exp := s.currencyExponent(ctx, acct.DefaultCurrency)

	rows, perr := bankimport.Parse(raw, cfg, exp)
	if perr != nil {
		s.renderImportError(w, r, "import.error.parse", "")
		return
	}
	for i, row := range rows {
		if row.Err != nil {
			s.renderImportError(w, r, "import.error.row", strconv.Itoa(i+1))
			return
		}
	}

	actorCtx := s.actorCtx(ctx)

	// Optionally persist the mapping as a reusable profile; else create a throwaway
	// unnamed profile so the batch's profile_id FK is satisfied (every batch records
	// the exact mapping that produced it).
	profileName := r.FormValue("profile_name")
	if r.FormValue("save_profile") != "1" || profileName == "" {
		profileName = "import " + time.Now().UTC().Format("2006-01-02")
	}
	profileID, err := s.store.CreateMappingProfile(actorCtx, profileName, cfg)
	if err != nil {
		s.serverError(w)
		return
	}

	filename := r.FormValue("filename")
	batchID, err := s.store.CreateImportBatch(actorCtx, filename, accountID, subsidiaryID, profileID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		if errors.Is(err, store.ErrBatchSubsidiaryMismatch) {
			s.renderImportError(w, r, "import.error.sub_mismatch", "")
			return
		}
		s.serverError(w)
		return
	}

	staged, err := s.store.StageImportRows(actorCtx, batchID, accountID, rows)
	if err != nil {
		s.serverError(w)
		return
	}

	// The result is swapped into #import-workspace via htmx innerHTML (replacing the
	// preview), so render JUST the fragment (no shell).
	s.render(w, r, http.StatusOK, "import-result",
		s.buildImportResult(r, batchID, filename, acct.DefaultCurrency, exp, staged))
}

// buildImportResult formats the staged-row result for the confirm page.
func (s *server) buildImportResult(r *http.Request, batchID int64, filename, currency string, exp int, staged []store.StagedRow) importResultModel {
	u := currentUser(r.Context())
	opts := formatOptsFor(u)
	df := dateFormatFor(u)

	model := importResultModel{BatchID: batchID, Filename: filename, Staged: len(staged)}
	for _, row := range staged {
		if row.Duplicate {
			model.Duplicates++
		}
		model.Rows = append(model.Rows, importResultRow{
			Date:      money.FormatDate(parseISOForDisplay(row.Date), df),
			AmountFmt: currency + " " + money.Format(row.AmountMinor, exp, opts),
			Payee:     row.Payee,
			Memo:      row.Memo,
			Duplicate: row.Duplicate,
		})
	}
	return model
}

// renderImportError re-renders the upload page at 422 with the error message. The
// file input cannot be echoed back, so this is a full-page 422 (like admin-rates),
// not an inline field swap.
func (s *server) renderImportError(w http.ResponseWriter, r *http.Request, key, arg string) {
	model, err := s.buildImportUpload(r)
	if err != nil {
		s.serverError(w)
		return
	}
	model.ErrorKey = key
	model.ErrorArg = arg
	s.render(w, r, http.StatusUnprocessableEntity, "import.tmpl", s.newShellPage(r, model))
}

// subsidiaryName returns a subsidiary's name, or "" on any error (display only).
func (s *server) subsidiaryName(ctx context.Context, id int64) string {
	sub, err := s.store.GetSubsidiary(ctx, id)
	if err != nil {
		return ""
	}
	return sub.Name
}
