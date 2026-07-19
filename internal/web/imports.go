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
	"cuento/internal/i18n"
	"cuento/internal/ids"
	"cuento/internal/money"
	"cuento/internal/store"
)

// p17.2 bank-CSV import: upload -> mapping UI -> 20-row preview -> stage (D17,
// DECISIONS p17.2). A TxnWrite user uploads a bank statement CSV, assigns columns
// (which column is date/amount/description/memo), picks the target account +
// subsidiary,
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
	ID   int64
	Name string
	// Path (p28.2) is the dotted ancestor chain; the target picker is a shared
	// combobox that fuzzy-ranks on it, like every account picker.
	Path          string
	SubsidiaryIDs []ids.SubsidiaryID
}

// importProfileOption is one saved mapping profile the user may load.
type importProfileOption struct {
	ID   int64
	Name string
}

// importPreviewModel is the POST /import/preview result: the file's COLUMNS (for the
// horizontal "maps to" mapping UI, p26.64), the parsed rows (capped to 20 for DISPLAY
// -- staging persists all), the total parsed count, the carried CSV + mapping (as
// hidden fields for confirm), and the target labels.
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
	// AmountModeStr is the current amount mode ("single"/"debit_credit") for the mode
	// select in the fragment (p26.64: mode is editable on the map+preview step).
	AmountModeStr string
	// Columns is the file's real columns (header + sample) each with its "maps to"
	// options -- the horizontal mapping UI (p26.64). Empty on the legacy path.
	Columns []importColumnMap
	// HasPreview is true when the required roles are mapped and the file parsed, so the
	// preview table + confirm form render; false shows the mapping selects only.
	HasPreview bool
	Rows       []importPreviewRow
	TotalRows  int
	ShownRows  int
	MoreRows   int // TotalRows - ShownRows (rows staged but not previewed)
	ErrorKey   string
	ErrorArg   string
}

// importColumnMap is one CSV column in the horizontal mapping UI: its index, header
// name, a sample value, and the "maps to" options (already gated to the current mode
// and label-resolved, with the current pick marked Selected).
type importColumnMap struct {
	Index   int
	Name    string
	Sample  string
	Options []importColumnRoleOption
}

// importColumnRoleOption is one rendered "maps to" <option>: its role value, localized
// label, and whether it is the column's current pick.
type importColumnRoleOption struct {
	Value    bankimport.Role
	Label    string
	Selected bool
}

// importRoleOption is one "maps to" choice. AmountMode names the single amount mode it
// belongs to ("" == valid in every mode: Ignore / Date / Description), so the options
// are gated to the current mode server-side (NO-JS-safe; a mode change re-POSTs and
// re-renders).
type importRoleOption struct {
	Value      bankimport.Role
	LabelKey   string
	AmountMode bankimport.AmountMode
}

// importRoleOptions is the fixed "maps to" option set. Amount belongs to single mode;
// Debit + Credit to debit_credit mode; the rest are mode-independent.
var importRoleOptions = []importRoleOption{
	{Value: bankimport.RoleIgnore, LabelKey: "import.role.ignore"},
	{Value: bankimport.RoleDate, LabelKey: "import.role.date"},
	{Value: bankimport.RoleDescription, LabelKey: "import.role.desc"},
	{Value: bankimport.RoleAmount, LabelKey: "import.role.amount", AmountMode: bankimport.AmountSingle},
	{Value: bankimport.RoleDebit, LabelKey: "import.role.debit", AmountMode: bankimport.AmountDebitCredit},
	{Value: bankimport.RoleCredit, LabelKey: "import.role.credit", AmountMode: bankimport.AmountDebitCredit},
	// Memo is OPTIONAL and mode-independent (p26.65): a mapped column feeds the split's
	// memo; unmapped is fine (default Ignore, empty memo, no validation error).
	{Value: bankimport.RoleMemo, LabelKey: "import.role.memo"},
}

// roleValidInMode reports whether a role is offered/usable in the given amount mode.
// Amount belongs to single mode, Debit/Credit to debit_credit; the rest (Ignore, Date,
// Description) are valid in every mode. An empty/unknown mode is treated as single
// (parseAmount's default case).
func roleValidInMode(role bankimport.Role, mode bankimport.AmountMode) bool {
	dc := mode == bankimport.AmountDebitCredit
	switch role {
	case bankimport.RoleAmount:
		return !dc
	case bankimport.RoleDebit, bankimport.RoleCredit:
		return dc
	default:
		return true
	}
}

// roleOptionsFor builds the "maps to" <option> list for one column: the mode-gated
// options (label-resolved in lang), with the column's current pick marked Selected.
func roleOptionsFor(lang string, mode bankimport.AmountMode, selected bankimport.Role) []importColumnRoleOption {
	var out []importColumnRoleOption
	for _, opt := range importRoleOptions {
		if !roleValidInMode(opt.Value, mode) {
			continue
		}
		out = append(out, importColumnRoleOption{
			Value:    opt.Value,
			Label:    i18n.T(lang, opt.LabelKey),
			Selected: opt.Value == selected,
		})
	}
	return out
}

// importPreviewRow is one previewed parsed row: formatted for display.
type importPreviewRow struct {
	Date        string
	AmountFmt   string
	Description string // bank line descriptive text (the mapped desc_col)
	Memo        string
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
	Date        string
	AmountFmt   string
	Description string // bank line descriptive text (the mapped desc_col)
	Memo        string
	Duplicate   bool
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
		model.Subsidiaries = append(model.Subsidiaries, importSubOption{ID: int64(sub.ID), Name: sub.Name})
	}

	// All leaf+active accounts (union across subsidiaries). AccountEditorOptions is
	// per-subsidiary; a nil-safe union is built by iterating each subsidiary once,
	// de-duplicating by account id.
	seen := make(map[int64]bool)
	for _, sub := range model.Subsidiaries {
		opts, err := s.store.AccountEditorOptions(ctx, lang, ids.SubsidiaryID(sub.ID))
		if err != nil {
			return importUploadModel{}, err
		}
		for _, o := range opts {
			if seen[o.ID] {
				continue
			}
			seen[o.ID] = true
			model.Accounts = append(model.Accounts, importAccountOption{
				ID: o.ID, Name: o.Name, Path: o.Path, SubsidiaryIDs: o.SubsidiaryIDs,
			})
		}
	}

	profiles, err := s.store.ListMappingProfiles(ctx)
	if err != nil {
		return importUploadModel{}, err
	}
	for _, p := range profiles {
		model.Profiles = append(model.Profiles, importProfileOption{ID: int64(p.ID), Name: p.Name})
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
	if pid := ids.MappingProfileID(parseID(r.FormValue("profile_id"))); pid > 0 {
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
		DescCol:   atoiDefault(r.FormValue("desc_col"), -1),
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

// importPreview handles POST /import/preview (TxnWrite). It has TWO paths:
//
//   - LEGACY (any *_col field present -- only the Go handler tests send these): parse
//     the CSV under the typed index mapping and show the 20-row preview, or a bare
//     import-error 422 on a parse/mapping error (the pre-p26.64 behavior, byte-for-byte).
//
//   - NEW (p26.64, the shipped UI): the file's REAL columns are shown horizontally with
//     a "maps to" select each. The mapping is derived from the per-column role picks
//     (else a loaded profile, else all-Ignore on the first upload). When the required
//     roles (Date + Amount, or Date + Debit + Credit) are mapped and the file parses,
//     the 20-row preview + confirm form co-render in the same fragment; otherwise the
//     selects render alone (no preview yet). A bad column MAPPING is an IN-FRAGMENT hint,
//     NOT a workspace-wiping 422 (that stays reserved for FILE-level problems -- no file,
//     too large, unreadable CSV, zero rows -- where columns can't even be shown).
//
// Both paths render only a BARE fragment into #import-workspace (p26.62: never the shell).
// The first upload is multipart (carries the file); a remap re-POST is urlencoded and
// carries the CSV forward as csv_b64, so raw comes from the file OR that hidden field.
func (s *server) importPreview(w http.ResponseWriter, r *http.Request) {
	raw, filename, key, arg := s.importPreviewRaw(r)
	if key != "" {
		s.renderImportError(w, r, key, arg)
		return
	}

	accountID := parseID(r.FormValue("account_id"))
	subsidiaryID := parseID(r.FormValue("subsidiary_id"))

	// LEGACY path: a typed *_col field means the old index-based form (tests only).
	if hasLegacyColumnFields(r) {
		cfg := s.mappingFrom(r)
		model, key, arg := s.parseImportPreview(r, raw, accountID, subsidiaryID, cfg)
		if key != "" {
			s.renderImportError(w, r, key, arg)
			return
		}
		s.finishPreviewModel(&model, r, filename, raw)
		s.render(w, r, http.StatusOK, "import-preview", model)
		return
	}

	// NEW path: the horizontal column-mapping UI.
	s.importMapPreview(w, r, raw, filename, accountID, subsidiaryID)
}

// importPreviewRaw resolves the raw CSV bytes and the filename from the request: the
// multipart file on the first upload, or the carried csv_b64 on a remap re-POST. A
// FILE-level problem returns an (errKey, errArg) to render as a bare import-error 422.
func (s *server) importPreviewRaw(r *http.Request) (raw []byte, filename, errKey, errArg string) {
	// A file upload is multipart; a remap re-POST is urlencoded (csv_b64). Try the
	// multipart file first, then fall back to the carried base64.
	if err := r.ParseMultipartForm(maxImportUpload); err == nil {
		if file, header, ferr := r.FormFile("file"); ferr == nil {
			defer func() { _ = file.Close() }()
			b, rerr := io.ReadAll(io.LimitReader(file, maxImportUpload+1))
			if rerr != nil || len(b) == 0 {
				return nil, "", "import.error.no_file", ""
			}
			if len(b) > maxImportUpload {
				return nil, "", "import.error.too_large", ""
			}
			return b, header.Filename, "", ""
		}
	}
	// No multipart file: a remap re-POST carrying the CSV base64.
	if b64 := r.FormValue("csv_b64"); b64 != "" {
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(b) == 0 {
			return nil, "", "import.error.no_file", ""
		}
		if len(b) > maxImportUpload {
			return nil, "", "import.error.too_large", ""
		}
		return b, r.FormValue("filename"), "", ""
	}
	return nil, "", "import.error.no_file", ""
}

// finishPreviewModel stamps the carry fields (filename, profile save + name, the base64
// CSV) onto a preview model before it renders.
func (s *server) finishPreviewModel(model *importPreviewModel, r *http.Request, filename string, raw []byte) {
	model.Filename = filename
	model.ProfileName = r.FormValue("profile_name")
	model.SaveProfile = r.FormValue("save_profile") == "1"
	model.CSVBase64 = base64.StdEncoding.EncodeToString(raw)
}

// hasLegacyColumnFields reports whether the request carries a typed column-index field
// (date_col/amount_col/...). Only the pre-p26.64 tests submit these; the shipped UI
// never does, so their presence selects the legacy index-based preview path.
func hasLegacyColumnFields(r *http.Request) bool {
	for _, f := range []string{"date_col", "amount_col", "debit_col", "credit_col", "desc_col", "memo_col"} {
		if r.FormValue(f) != "" {
			return true
		}
	}
	return false
}

// importMapPreview is the NEW horizontal-mapping preview (p26.64). It reads the file's
// columns, derives the mapping from the per-column role picks (or a loaded profile, or
// all-Ignore), and co-renders the selects with a preview when the required roles are
// mapped and the file parses. Bad/incomplete mapping shows the selects only -- never a
// workspace-wiping 422.
func (s *server) importMapPreview(w http.ResponseWriter, r *http.Request, raw []byte, filename string, accountID, subsidiaryID int64) {
	ctx := r.Context()
	lang := langOf(ctx)

	delim := bankimport.Delimiter(r.FormValue("delimiter"))
	hasHeader := r.FormValue("has_header") == "1"
	mode := bankimport.AmountMode(r.FormValue("amount_mode"))
	signFlip := r.FormValue("sign_flip") == "1"
	dateFmt := bankimport.DateLayout(r.FormValue("date_format"))

	cols, cerr := bankimport.Columns(raw, delim, hasHeader)
	if cerr != nil {
		// Unreadable structure / zero rows: a FILE-level problem -> bare error 422.
		s.renderImportError(w, r, "import.error.parse", "")
		return
	}

	roles := s.rolesForColumns(r, cols)
	// Coerce any role that is not valid in the current mode to Ignore (e.g. a Debit
	// pick after switching to single) so the select never carries a value with no
	// matching option.
	for i, role := range roles {
		if !roleValidInMode(role, mode) {
			roles[i] = bankimport.RoleIgnore
		}
	}
	cfg := bankimport.ConfigFromRoles(roles, delim, hasHeader, mode, signFlip, dateFmt)

	model := importPreviewModel{
		AccountID:     accountID,
		AccountName:   s.accountName(ctx, accountID, lang),
		SubsidiaryID:  subsidiaryID,
		SubsidiaryNm:  s.subsidiaryName(ctx, ids.SubsidiaryID(subsidiaryID)),
		Mapping:       cfg,
		AmountModeStr: string(cfg.Amount),
	}
	for i, c := range cols {
		model.Columns = append(model.Columns, importColumnMap{
			Index: c.Index, Name: c.Name, Sample: c.Sample,
			Options: roleOptionsFor(lang, mode, roles[i]),
		})
	}
	s.finishPreviewModel(&model, r, filename, raw)

	// Only attempt a preview once the REQUIRED roles for the mode are mapped and the
	// target is chosen; otherwise show the selects alone (the map step).
	if accountID != 0 && subsidiaryID != 0 && requiredRolesMapped(cfg) {
		if prev, key, arg := s.parseImportPreview(r, raw, accountID, subsidiaryID, cfg); key == "" {
			model.HasPreview = true
			model.Rows = prev.Rows
			model.TotalRows = prev.TotalRows
			model.ShownRows = prev.ShownRows
			model.MoreRows = prev.MoreRows
		} else {
			// A parse/mapping error on the NEW path is an in-fragment hint (the selects
			// stay so the user can fix the mapping), not a 422. Carry the arg too (e.g.
			// import.error.row's row number) so the message is complete.
			model.ErrorKey = key
			model.ErrorArg = arg
		}
	}

	s.render(w, r, http.StatusOK, "import-map-preview", model)
}

// rolesForColumns resolves the per-column role slice for the columns: from the posted
// per-column role fields (role_0, role_1, ...) when present; else from a loaded profile
// (reverse-mapped to roles); else all-Ignore (the first upload). len(result)==len(cols).
func (s *server) rolesForColumns(r *http.Request, cols []bankimport.ColumnInfo) []bankimport.Role {
	roles := make([]bankimport.Role, len(cols))
	posted := false
	for i := range cols {
		if v := r.FormValue("role_" + strconv.Itoa(i)); v != "" {
			roles[i] = bankimport.Role(v)
			posted = true
		}
	}
	if posted {
		return roles
	}
	// No per-column picks: a loaded profile pre-selects the roles (reverse map).
	if pid := ids.MappingProfileID(parseID(r.FormValue("profile_id"))); pid > 0 {
		if prof, err := s.store.GetMappingProfile(r.Context(), pid); err == nil {
			return bankimport.RolesFromConfig(prof.Config, len(cols))
		}
	}
	return roles // all Ignore: the first upload, nothing mapped yet
}

// requiredRolesMapped reports whether cfg has the columns a preview needs: a Date
// column and, per the amount mode, either the single Amount column or BOTH the Debit
// and Credit columns.
func requiredRolesMapped(cfg bankimport.Config) bool {
	if cfg.DateCol < 0 {
		return false
	}
	if cfg.Amount == bankimport.AmountDebitCredit {
		return cfg.DebitCol >= 0 && cfg.CreditCol >= 0
	}
	return cfg.AmountCol >= 0
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
		SubsidiaryNm: s.subsidiaryName(ctx, ids.SubsidiaryID(subsidiaryID)),
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
			Date:        money.FormatDate(parseISOForDisplay(row.Date), df),
			AmountFmt:   money.FormatMoney(row.AmountMinor, acct.DefaultCurrency, exp, opts),
			Description: row.Description,
			Memo:        row.Memo,
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
	batchID, err := s.store.CreateImportBatch(actorCtx, filename, accountID, ids.SubsidiaryID(subsidiaryID), profileID, time.Now().UTC().Format(time.RFC3339))
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
		s.buildImportResult(r, int64(batchID), filename, acct.DefaultCurrency, exp, staged))
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
			Date:        money.FormatDate(parseISOForDisplay(row.Date), df),
			AmountFmt:   money.FormatMoney(row.AmountMinor, currency, exp, opts),
			Description: row.Description,
			Memo:        row.Memo,
			Duplicate:   row.Duplicate,
		})
	}
	return model
}

// renderImportError renders the BARE import-error fragment at 422 with the message.
// Its two callers (importPreview, importConfirm) are htmx POSTs targeting
// #import-workspace with hx-swap=innerHTML, so a full shell page would nest a whole
// document inside the workspace (the duplicate page-frame bug, p26.62 — the p26.35
// class). The fragment swaps cleanly and shows the error in place; the NO-JS fallback
// still gets a valid 422 body with the message. The file input cannot be echoed back
// either way (it never survives an htmx swap), so re-picking the file is expected.
func (s *server) renderImportError(w http.ResponseWriter, r *http.Request, key, arg string) {
	s.render(w, r, http.StatusUnprocessableEntity, "import-error",
		importPreviewModel{ErrorKey: key, ErrorArg: arg})
}

// importProfileDelete handles POST /import/profiles/{id}/delete (TxnWrite):
// soft-delete (deactivate) a saved mapping profile so it stops appearing in the load
// list; the batch FK that referenced it stays intact (its audit). On success it
// redirects back to the upload page (HX-Redirect for the htmx delete control, a plain
// 303 for the NO-JS form). A missing/already-gone profile is a clean 404.
func (s *server) importProfileDelete(w http.ResponseWriter, r *http.Request) {
	id := ids.MappingProfileID(parseID(r.PathValue("id")))
	if err := s.store.DeactivateMappingProfile(s.actorCtx(r.Context()), id); err != nil {
		if errors.Is(err, store.ErrMappingProfileNotFound) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/import")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/import", http.StatusSeeOther)
}

// subsidiaryName returns a subsidiary's name, or "" on any error (display only).
func (s *server) subsidiaryName(ctx context.Context, id ids.SubsidiaryID) string {
	sub, err := s.store.GetSubsidiary(ctx, id)
	if err != nil {
		return ""
	}
	return sub.Name
}
