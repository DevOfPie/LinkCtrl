package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
)

// The Add-on manager (M68).
//
// # Where it lives, and why not at /addons
//
// `/instance/addons`. `/addons/` is an installed add-on's own prefix (M64) and
// `/addons/{addon}` is already a route, so a manager at `/addons` could not have a
// detail page: `/addons/oidc` is that add-on's own page. `/instance/` is where the
// one other thing belonging to the box already sits — `POST
// /instance/update-check` — and it is the right prefix for the same reason that
// one took it: what this page administers is the machine, not a workspace.
//
// # Two states of one table, and the column that does not move
//
// The owner's amendment on 2026-08-18: **Remove is select-mode**, in line with
// Install, and pressing it turns each row's trailing chevron into a checkbox *in
// the same column*. The template renders that cell once — `addon_row_end` in
// pages/addons.html — with the state as its only argument, so the two states
// cannot come to have different widths.
// `tools/agent-browser/specs/addon-manager.spec.mjs` measures the laid-out column
// in both states, which is what the bullet asks for and what no Go test can do;
// `TestTheSelectColumnIsOneTemplateInBothStates` holds the mechanism behind the
// measurement, because a matched pair of hard-coded widths would pass the
// measurement today and drift the first time either was edited.
//
// # Confirmation is a page, not a dialog
//
// One confirmation for one or many, carrying a purge choice per selected module
// with the box **unticked**, and a `required`-class module's consequence spelled
// out beside its name. It is rendered as a state of this page rather than as a
// `<dialog>`, because everything here has to work with scripting off — the same
// standard every other destructive act in this product meets — and a dialog that
// only opens with JavaScript would make the one irreversible operation on the page
// the one that needs a feature the rest does not.
//
// # Everything writes through the same service the API calls
//
// The inherited rule, and here it is load-bearing rather than tidy: m67.md's
// bullet says this page "calls what M67 ships and has no private side door". The
// install handler builds an [addon.InstallRequest] and hands it to the same
// `Install`; the remove handler calls the same `Remove`; and the scope is checked
// in internal/addon for both, so nothing on this page can be reached by somebody
// the API would refuse.

// AddonManagerPath is where the manager lives. Named so the handler, the nav entry
// and the redirect targets cannot drift apart by a spelling.
const AddonManagerPath = "/instance/addons"

// AddonRemoveSegment and AddonPurgeSegment are the two path segments the manager's
// destructive forms post to, and **both carry a hyphen for the reason
// [AddonOrphanPath] does**.
//
// `GET /instance/addons/{name}` is an add-on's detail page, so anything else
// mounted directly under this prefix is a segment an add-on's *name* could claim.
// api_addon_manager.go argues that such a reservation must be a property of the
// name grammar rather than of routing precedence, and these two failed it: they
// were spelled `remove` and `purge`, both of which match
// `^[a-z][a-z0-9_]{1,30}$`. Nothing collided only because the methods differed —
// which is precedence by another name, and is exactly the resolution D263 refuses.
//
// A hyphen is not in the grammar, so no manifest can name an add-on either of
// these. TestTheManagersOwnPathsCannotBeAddonNames holds all three against
// [store.ValidAddonName] rather than against this comment.
const (
	AddonRemoveSegment = "remove-selected"
	AddonPurgeSegment  = "purge-data"
)

// addonRow is one installed add-on as the list draws it.
type addonRow struct {
	Name    string
	Version string
	// Declaration and Failure are the two badge columns.
	Declaration string
	Failure     string
	// Permissions is what the add-on's manifest declared, which is the column
	// m68.md asks for and is M62's visibility promise kept here. Withheld is the
	// subset of those this build does not grant — empty today, and drawn when it is
	// not, because a permission an add-on declared and does not hold is behaviour
	// somebody agreed to and is not getting.
	Permissions []string
	Withheld    map[string]bool
	// P99 and Kills are the performance columns. Both are strings because the
	// page draws a dash for a module that has never run on the redirect path, and
	// "—" is not a number.
	P99   string
	Kills string
	// Observed is whether this add-on has any redirect-path record at all, so the
	// row can say *not on the redirect path* rather than showing two dashes that
	// look like missing data.
	Observed bool
	// Schema and SchemaSize describe the add-on's own storage, empty for one that
	// declared none.
	Schema     string
	SchemaSize string
	// Settings is "3 of 5 set", or empty for an add-on that declares none.
	Settings string
	// Required marks a `required`-class module, which the confirmation uses to
	// state the consequence of removing one.
	Required bool
}

// addonPurgeChoice is one module's purge box on the confirmation.
type addonPurgeChoice struct {
	Name    string
	Version string
	// Schema is what would be dropped, and Size is how big it is *now* — measured
	// at the moment of the prompt, which is m68.md's second risk.
	Schema string
	Size   string
	// HasData is false for a module that declared no storage, and then the row
	// offers no box: there is nothing to purge and a ticked box that did nothing
	// would be the worst kind of confirmation.
	HasData bool
	// Consequence is the sentence a `required`-class module's removal earns, and
	// is empty for a `degrade` one.
	Consequence string
}

// addonOrphanRow is one leftover schema.
type addonOrphanRow struct {
	Name   string
	Schema string
	Size   string
	// LargeObjects, IdentityLinks and StoredSettings are what a purge does **not**
	// take. They are on the list row's type because the confirmation is a state of
	// this page and reads the same row, and all three are drawn on the confirmation
	// whatever their value: docs/SECURITY.md says the purge's confirmation names
	// all four things a drop leaves — the login role is the fourth and has no
	// count — and a sentence that appears only when a number is non-zero leaves the
	// commonest case saying one.
	LargeObjects   int64
	IdentityLinks  int64
	StoredSettings int64
}

type addonsPageData struct {
	shell
	Rows    []addonRow
	Orphans []addonOrphanRow
	// Selecting draws the trailing column as checkboxes and the button as
	// "Remove selected".
	Selecting bool
	// Confirming is the removal confirmation, non-nil only in that state.
	Confirming []addonPurgeChoice
	// PurgingOrphan is the orphan confirmation, non-nil only in that state.
	PurgingOrphan *addonOrphanRow
	// MaxUpload is the install form's stated bound, from the same constant the
	// API refuses on.
	MaxUpload string
	// RemoveGrace is what a removal waits for invocations already inside a module,
	// said on the confirmation rather than described.
	RemoveGrace string
	Notice      string
	Error       string
}

type addonDetailPageData struct {
	shell
	Row addonRow
	// Classes is the per-class performance breakdown — one entry per class this
	// module has actually run in.
	Classes []addonClassRow
	// KillsInstantiate and KillsCall are F326's split, which the list column adds
	// together and the detail page does not.
	KillsInstantiate uint64
	KillsCall        uint64
	Settings         []addon.SettingView
	// SettingMaxLength is the `maxlength` the text and secret inputs carry, and it
	// is **not** the bound the save is refused on.
	//
	// [addon.MaxSettingValueBytes] is bytes; `maxlength` counts UTF-16 code units,
	// which is a different number for anything outside the Basic Multilingual Plane
	// and for every non-ASCII character in it. So the attribute is a rough stop
	// against a runaway paste and nothing else — a value inside it and outside the
	// server's bound is refused by `checkSettingValue`, with a message naming the
	// setting, which is where the real answer has to come from anyway because the
	// form works with scripting off.
	//
	// Carried from the constant rather than written into the template, so the two
	// cannot come to differ in magnitude as well as in unit.
	SettingMaxLength int
	// FieldErrors puts a refusal beside the input that earned it.
	FieldErrors map[string]string
	Notice      string
	Error       string
}

// addonClassRow is one class's figures on the detail page.
type addonClassRow struct {
	Class string
	Count uint64
	P99   string
	Mean  string
}

// AddonsPage draws the manager.
func (h *Web) AddonsPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadAddonsPage(w, r)
	if !ok {
		return
	}
	data.Selecting = r.URL.Query().Get("select") == "1"
	data.Notice, data.Error = addonNotice(r.URL.Query())
	h.render(w, r, http.StatusOK, "addons", data)
}

// loadAddonsPage reads everything the list draws. The bool is false when an error
// page has already been written.
func (h *Web) loadAddonsPage(w http.ResponseWriter, r *http.Request) (addonsPageData, bool) {
	data := addonsPageData{
		shell:       h.shell(r, "Add-ons", "addons"),
		MaxUpload:   byteSize(addon.MaxUploadBytes),
		RemoveGrace: addon.RemoveGrace.String(),
	}
	if h.AddonAdmin == nil {
		// Unreachable: the route is registered only when a host exists. Answered
		// rather than assumed, because the field is an interface and a nil one has
		// been the shape of a defect in this package before.
		h.errorPage(w, r, http.StatusNotFound, "Not found", "This instance runs no add-on host.")
		return data, false
	}
	actor := IdentityFrom(r.Context())
	managed, err := h.AddonAdmin.List(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	for _, m := range managed {
		data.Rows = append(data.Rows, addonRowFrom(m))
	}
	orphans, err := h.AddonAdmin.Orphans(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	for _, o := range orphans {
		data.Orphans = append(data.Orphans, addonOrphanRow{
			Name: o.Name, Schema: o.Schema, Size: addonSize(o.Bytes),
			LargeObjects: o.LargeObjects, IdentityLinks: o.IdentityLinks,
			StoredSettings: o.StoredSettings,
		})
	}
	return data, true
}

// addonRowFrom renders one managed add-on.
func addonRowFrom(m addon.Managed) addonRow {
	row := addonRow{
		Name:        m.Name,
		Version:     m.Version,
		Declaration: string(m.Declaration),
		Failure:     string(m.FailureClass),
		Permissions: m.Declared,
		Withheld:    withheldPermissions(m),
		Schema:      m.Schema,
		P99:         "—",
		Kills:       "—",
		Observed:    m.Performance.Observed(),
		Required:    m.FailureClass == addon.ClassRequired,
	}
	if m.Schema != "" {
		row.SchemaSize = addonSize(m.SchemaBytes)
	}
	if m.DeclaredSettings > 0 {
		row.Settings = fmt.Sprintf("%d of %d set", m.ConfiguredCount, m.DeclaredSettings)
	}
	// The worst class's p99, because the list has one column and the number an
	// operator is looking for is the one a visitor waited on. The detail page
	// splits it.
	var worst time.Duration
	for _, c := range m.Performance.Classes {
		if c.P99 > worst {
			worst = c.P99
		}
	}
	if worst > 0 {
		row.P99 = shortDuration(worst)
	}
	if m.Performance.Observed() {
		row.Kills = strconv.FormatUint(m.Performance.Kills.Total(), 10)
	}
	return row
}

// withheldPermissions is what an add-on declared and does not hold.
//
// Empty for every add-on today, and computed rather than assumed empty: the two
// sets diverged for a whole phase while `redirect.inline` was declarable and
// ungranted, and a page that printed the declaration as though it were the grant
// would have promised behaviour the host was refusing.
func withheldPermissions(m addon.Managed) map[string]bool {
	held := make(map[string]bool, len(m.Permissions))
	for _, p := range m.Permissions {
		held[p] = true
	}
	out := map[string]bool{}
	for _, p := range m.Declared {
		if !held[p] {
			out[p] = true
		}
	}
	return out
}

// shortDuration writes a latency the way an operator reads one: microseconds
// below a millisecond, milliseconds below a second, and one decimal place either
// way.
//
// time.Duration's own String is wrong for this column — `1.0845ms` is four
// significant figures of an estimate read off ten buckets, which reads as a
// precision the number does not have.
func shortDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d < time.Millisecond:
		return fmt.Sprintf("%.0fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}

// AddonDetailPage draws one add-on: its performance, its settings, its
// permissions and its data.
func (h *Web) AddonDetailPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadAddonDetail(w, r)
	if !ok {
		return
	}
	data.Notice, data.Error = addonNotice(r.URL.Query())
	h.render(w, r, http.StatusOK, "addon_manager", data)
}

func (h *Web) loadAddonDetail(w http.ResponseWriter, r *http.Request) (addonDetailPageData, bool) {
	var data addonDetailPageData
	if h.AddonAdmin == nil {
		h.errorPage(w, r, http.StatusNotFound, "Not found", "This instance runs no add-on host.")
		return data, false
	}
	name := r.PathValue("name")
	m, err := h.AddonAdmin.Detail(r.Context(), IdentityFrom(r.Context()), name)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	data.shell = h.shell(r, m.Name, "addons")
	data.Row = addonRowFrom(m)
	data.Settings = m.Settings
	data.SettingMaxLength = addon.MaxSettingValueBytes
	data.FieldErrors = map[string]string{}
	data.KillsInstantiate = m.Performance.Kills.Instantiate
	data.KillsCall = m.Performance.Kills.Call
	for _, c := range m.Performance.Classes {
		row := addonClassRow{Class: c.Class, Count: c.Count, P99: shortDuration(c.P99)}
		if c.Count > 0 {
			// float64 rather than a Duration division, because the count is a uint64
			// off a Prometheus histogram and converting one to a signed Duration is a
			// conversion nothing bounds. A mean of a latency is an approximation
			// anyway, and this one cannot overflow.
			row.Mean = shortDuration(time.Duration(float64(c.Sum) / float64(c.Count)))
		}
		data.Classes = append(data.Classes, row)
	}
	return data, true
}

// AddonInstall takes the upload the manager's Install control sends.
//
// The same two parts the API takes, read by the same reader and handed to the same
// service. There is no dashboard-only path into the host, which is what m67.md's
// "no private side door" means and is why this handler is nine lines.
func (h *Web) AddonInstall(w http.ResponseWriter, r *http.Request) {
	req, err := readAddonUpload(w, r)
	if err != nil {
		h.addonRedirect(w, r, "", err)
		return
	}
	out, err := h.AddonAdmin.Install(r.Context(), IdentityFrom(r.Context()), req)
	if err != nil {
		h.addonRedirect(w, r, "", err)
		return
	}
	seeOther(w, r, AddonManagerPath+"?installed="+url.QueryEscape(out.Name))
}

// AddonRemove is both halves of select-mode: the confirmation, and the act.
//
// One handler and one address, because they are one decision. The first POST
// carries the selected names and renders the confirmation; the second carries the
// same names plus the purge boxes and `confirm=1`, and performs it.
func (h *Web) AddonRemove(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	names := dedupe(r.PostForm["name"])
	if len(names) == 0 {
		// Nothing ticked. Back to select-mode rather than to a refusal page: the
		// reader pressed a button without choosing, which is a slip and not an error.
		seeOther(w, r, AddonManagerPath+"?select=1&nothing=1")
		return
	}

	if r.PostFormValue("confirm") != "1" {
		h.addonConfirmRemoval(w, r, names)
		return
	}

	actor := IdentityFrom(r.Context())
	var purged, removed []string
	for _, name := range names {
		out, err := h.AddonAdmin.Remove(r.Context(), actor, name)
		if err != nil {
			// **What already happened goes with the refusal.** A bulk removal is a
			// sequence of separate acts with no transaction over them, so a run that
			// stops at the third module has removed two — and a page answering with a
			// failure code alone would tell the reader nothing was changed while two
			// modules were missing from the table below it.
			h.addonRemovalStopped(w, r, err, len(removed), len(purged))
			return
		}
		removed = append(removed, name)
		// The purge is a second act on purpose, and it happens only for a module
		// whose box was ticked. Order matters: the schema is an orphan only once the
		// add-on is out of the loaded set, so purging before removing would be
		// refused by the service — which is the check doing its job rather than an
		// ordering to remember.
		if out.Schema == "" || r.PostFormValue("purge_"+name) == "" {
			continue
		}
		if _, err := h.AddonAdmin.PurgeData(r.Context(), actor, name); err != nil {
			// The removal happened and the purge did not. Not rolled back, because
			// there is nothing to roll back to — and not silent either: the count of
			// what was removed travels with the refusal, and the add-on's data is now
			// an orphan, which is a row on the page the reader lands on. Logged with
			// the name, because the refusal the reader sees is from a closed
			// vocabulary and cannot carry one.
			observability.LoggerFrom(r.Context()).Error(
				"an add-on was removed and the purge that was asked for failed",
				slog.String("addon", name), slog.Any("error", err))
			h.addonRemovalStopped(w, r, err, len(removed), len(purged))
			return
		}
		purged = append(purged, name)
	}
	seeOther(w, r, AddonManagerPath+"?removed="+strconv.Itoa(len(removed))+
		"&purged="+strconv.Itoa(len(purged)))
}

// addonConfirmRemoval renders the one confirmation, for one module or many.
func (h *Web) addonConfirmRemoval(w http.ResponseWriter, r *http.Request, names []string) {
	data, ok := h.loadAddonsPage(w, r)
	if !ok {
		return
	}
	actor := IdentityFrom(r.Context())
	for _, name := range names {
		m, err := h.AddonAdmin.Detail(r.Context(), actor, name)
		if errors.Is(err, domain.ErrNotFound) {
			// Removed by somebody else between the list and the button. Skipped
			// rather than refused: the reader's intention is satisfied for that row.
			continue
		}
		if err != nil {
			h.webError(w, r, err)
			return
		}
		choice := addonPurgeChoice{
			Name: m.Name, Version: m.Version,
			Schema: m.Schema, HasData: m.Schema != "",
			Size: addonSize(m.SchemaBytes),
		}
		if m.FailureClass == addon.ClassRequired {
			// The consequence, stated. `required` is what stops this instance
			// starting when the add-on will not load, so what removing one costs is
			// whatever it was doing — and for the case the class exists for, that is
			// sign-in.
			choice.Consequence = "This add-on is required-class: whatever it provides " +
				"stops when it goes, and for an authentication add-on that means " +
				"external sign-in stops."
		}
		data.Confirming = append(data.Confirming, choice)
	}
	if len(data.Confirming) == 0 {
		seeOther(w, r, AddonManagerPath)
		return
	}
	h.render(w, r, http.StatusOK, "addons", data)
}

// AddonPurge is the orphan list's own purge, in the same two halves.
func (h *Web) AddonPurge(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	name := r.PostFormValue("name")
	if name == "" {
		seeOther(w, r, AddonManagerPath)
		return
	}
	actor := IdentityFrom(r.Context())

	if r.PostFormValue("confirm") != "1" {
		data, ok := h.loadAddonsPage(w, r)
		if !ok {
			return
		}
		for _, o := range data.Orphans {
			if o.Name == name {
				row := o
				data.PurgingOrphan = &row
			}
		}
		if data.PurgingOrphan == nil {
			seeOther(w, r, AddonManagerPath)
			return
		}
		h.render(w, r, http.StatusOK, "addons", data)
		return
	}

	out, err := h.AddonAdmin.PurgeData(r.Context(), actor, name)
	if err != nil {
		h.addonRedirect(w, r, "", err)
		return
	}
	seeOther(w, r, AddonManagerPath+"?purged_schema="+url.QueryEscape(out.Schema))
}

// AddonSettingsSubmit saves the detail page's settings form.
//
// Every editable field is posted, so an empty box means *unset* — the same reading
// the API's PUT takes, because it is the same call underneath.
func (h *Web) AddonSettingsSubmit(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	name := r.PathValue("name")
	values := map[string]string{}
	// The form names every editable setting in a hidden input, and which of the
	// three it uses decides how a blank arriving is read. That is the whole of the
	// logic here, and it is in three loops rather than one branch because the three
	// readings are genuinely different rules rather than cases of one.
	//
	// `field` — text and select. Always carried; blank means *unset*, which is the
	// same reading the environment route gives a set-and-empty variable.
	for _, field := range r.PostForm["field"] {
		values[field] = r.PostFormValue("setting_" + field)
	}
	// `toggle` — an unticked checkbox sends nothing at all, so absence is "false"
	// and any value is "true". Without the hidden input, switching one off would be
	// indistinguishable from a form that never mentioned it.
	for _, field := range r.PostForm["toggle"] {
		values[field] = strconv.FormatBool(r.PostFormValue("setting_"+field) != "")
	}
	// `secret` — blank means *keep what is stored*, because the input is never
	// populated with the stored value and cannot be: a form that read blank as
	// unset would delete every secret on the instance the first time somebody
	// changed a neighbouring text field. Clearing one is its own checkbox, which is
	// the deliberate act the reading above refuses to infer.
	for _, field := range r.PostForm["secret"] {
		switch v := r.PostFormValue("setting_" + field); {
		case r.PostFormValue("clear_"+field) != "":
			values[field] = ""
		case v != "":
			values[field] = v
		}
	}
	if _, err := h.AddonAdmin.SaveSettings(r.Context(), IdentityFrom(r.Context()), name, values); err != nil {
		var ve domain.ValidationErrors
		if errors.As(err, &ve) {
			data, ok := h.loadAddonDetail(w, r)
			if !ok {
				return
			}
			// **The form comes back as it was typed.** loadAddonDetail reads the
			// *stored* state, which is by definition not what was just submitted, so
			// rendering it would answer a refusal by throwing away the work — every
			// field a reader had changed, not only the one that was wrong. Every other
			// form in this product re-renders what arrived; this one has one exception
			// and it is the whole point of the exception: a secret is never echoed,
			// so a rejected save leaves the password box empty and the reader retypes
			// the credential. Nothing else does.
			keepSubmittedSettings(data.Settings, values)
			data.FieldErrors, data.Error = fieldErrors(err)
			h.render(w, r, http.StatusUnprocessableEntity, "addon_manager", data)
			return
		}
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, AddonManagerPath+"/"+url.PathEscape(name)+"?saved=1")
}

// keepSubmittedSettings puts what an operator typed back into the render model.
//
// In place, over the views loadAddonDetail built from the stored state, so the
// type, the options, the environment's answer and the *set / not set* placeholder
// all stay whatever the host says they are — only the value moves. A `secret` is
// skipped, because [addon.SettingView] promises it is never rendered back and a
// re-render is still a render.
func keepSubmittedSettings(views []addon.SettingView, values map[string]string) {
	for i := range views {
		v := &views[i]
		if v.IsSecret() || !v.Editable() {
			continue
		}
		if raw, ok := values[v.Name]; ok {
			v.Value = raw
		}
	}
}

// addonRemovalStopped sends a refusal back to the list, carrying how far the run
// got.
//
// The counts ride the same query string the successful path uses and are re-parsed
// as integers before anything is printed, so nothing from a URL is rendered — the
// property [addonNotice] exists to keep. Both sentences are drawn in the **red**
// flash: a run that stopped is a refusal however much of it landed, and splitting
// it across the two flashes would put a green sentence on a failed operation.
func (h *Web) addonRemovalStopped(
	w http.ResponseWriter, r *http.Request, err error, removed, purged int,
) {
	to := AddonManagerPath + "?failed=" + url.QueryEscape(addonFailureCode(err))
	if removed > 0 {
		to += "&removed=" + strconv.Itoa(removed) + "&purged=" + strconv.Itoa(purged)
	}
	seeOther(w, r, to)
}

// addonRedirect sends a refusal back to the page it came from.
//
// A redirect rather than a rendered page, so a reload does not re-post an install
// or a removal. The message travels in the query string and is re-read from a
// closed set below — never echoed — because everything on this surface is
// attacker-influenced text and a message assembled from a URL is the classic way
// a refusal becomes an injection.
func (h *Web) addonRedirect(w http.ResponseWriter, r *http.Request, name string, err error) {
	to := AddonManagerPath
	if name != "" {
		to += "/" + url.PathEscape(name)
	}
	seeOther(w, r, to+"?failed="+url.QueryEscape(addonFailureCode(err)))
}

// addonFailureCode maps a service refusal to a code the page turns back into a
// sentence. A closed vocabulary, so nothing a module or an uploaded manifest
// contains reaches a rendered message through this path.
//
// # One validation code is carried through rather than collapsed
//
// Everything the install refuses is [domain.ValidationErrors], which maps to
// `invalid`, which the page words as *the manifest, the module or the digest did
// not check out*. That sentence is true of a bad digest and false of the one
// refusal an operator can act on: an add-on that ships `.sql` migration files is
// refused because an upload carries only two parts, and the route on is to place
// its directory in `LINKCTRL_ADDONS_DIR` and restart. Collapsing it told the
// reader to check a digest that is fine and named neither the cause nor the way
// round it — while the API, answering the same refusal, carried both.
//
// [addonValidationCode] is the whole of the mechanism: the field error's own code
// is looked at, and only a code this file knows is admitted. Nothing from the
// manifest is read, so the closure the rest of this comment describes is intact.
func addonFailureCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrForbidden):
		return "forbidden"
	case errors.Is(err, domain.ErrNotFound):
		return "not_found"
	case errors.Is(err, domain.ErrConflict):
		return "conflict"
	case errors.Is(err, domain.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, domain.ErrValidation):
		return addonValidationCode(err)
	default:
		return "failed"
	}
}

// addonValidationCode picks the one refusal the install form words specifically.
//
// A closed set of one, matched against [addon.CodeMigrationsUnsupported] rather
// than against a substring of a message: the message is assembled from a manifest
// and the code is not.
func addonValidationCode(err error) string {
	var ve domain.ValidationErrors
	if errors.As(err, &ve) {
		for _, fe := range ve {
			if fe.Code == addon.CodeMigrationsUnsupported {
				return "migrations_unsupported"
			}
		}
	}
	return "invalid"
}

// addonNotice turns the query string into the two sentences at the top of the
// page: one that went well, and one that did not. Exactly one is ever non-empty.
//
// **Every sentence is a literal in this function.** Nothing from the URL is
// rendered: a refusal arrives as a code and is looked up in a closed set, and the
// counts are re-parsed as integers and re-printed. This surface renders
// attacker-influenced text everywhere else — an add-on's name, its version, every
// field of an uploaded manifest — and a message assembled out of a query string is
// the classic way one of those becomes an injection.
//
// The split matters as much as the escaping: a refusal drawn in the green flash
// reads as a success, and the one operation on this page that deletes data is not
// a place to be ambiguous about which happened.
func addonNotice(q url.Values) (notice, failure string) {
	switch {
	case q.Get("failed") != "":
		// A bulk removal that stopped partway carries how far it got, and that
		// sentence leads: the message after it says what the failing act did, and
		// what the *run* did is the part a reader cannot recover from anywhere else.
		if n, err := strconv.Atoi(q.Get("removed")); err == nil && n > 0 {
			p, _ := strconv.Atoi(q.Get("purged"))
			return "", addonPartialRemoval(n, p) + " " + addonFailureMessage(q.Get("failed"))
		}
		return "", addonFailureMessage(q.Get("failed"))
	case q.Get("nothing") == "1":
		return "", "Nothing was selected, so nothing was removed. Tick the add-ons to remove."
	case q.Get("installed") != "":
		return "The add-on was installed and is running now. No restart was needed.", ""
	case q.Get("purged_schema") != "":
		return "The schema was dropped. Its data is gone and cannot be recovered.", ""
	case q.Get("removed") != "":
		n, err := strconv.Atoi(q.Get("removed"))
		if err != nil || n <= 0 {
			return "", ""
		}
		p, _ := strconv.Atoi(q.Get("purged"))
		msg := fmt.Sprintf("%s removed.", plural(n, "add-on"))
		switch {
		case p == n:
			return msg + " Their data was deleted too.", ""
		case p > 0:
			return msg + fmt.Sprintf(" %d of the schemas were deleted; the rest is "+
				"listed below as orphaned data.", p), ""
		default:
			return msg + " Their data is listed below as orphaned data.", ""
		}
	case q.Get("saved") == "1":
		return "Saved. The add-on reads the new values on its next invocation.", ""
	}
	return "", ""
}

// addonPartialRemoval words what a stopped bulk removal did before it stopped.
//
// A literal, like every other sentence on this page: the two counts arrive as
// integers and are re-printed, and nothing else from the query string reaches it.
func addonPartialRemoval(removed, purged int) string {
	msg := fmt.Sprintf("%s removed before this stopped", plural(removed, "add-on"))
	if purged > 0 {
		msg += fmt.Sprintf(", and %d of the schemas deleted", purged)
	}
	return msg + "; that part stands and the rest were not touched."
}

// addonFailureMessage is the closed set of refusals this page words itself.
func addonFailureMessage(code string) string {
	switch code {
	case "forbidden":
		return "Managing add-ons needs the " + auth.PermAddonsManage +
			" permission, which belongs to the account that administers this instance."
	case "not_found":
		return "That add-on is not installed on this instance."
	case "conflict":
		return "An add-on with that name is already installed. Replacing one is a " +
			"removal and an install."
	case "unavailable":
		return "This instance cannot write to its add-ons directory, so that change " +
			"was refused. See the add-ons section of the configuration reference."
	case "invalid":
		return "The upload was refused: the manifest, the module or the digest " +
			"between them did not check out. Nothing was written."
	case "migrations_unsupported":
		return "This add-on declares migration files, and an upload carries only " +
			"the module and its manifest. Install it by placing its directory in " +
			"LINKCTRL_ADDONS_DIR and restarting. Nothing was written."
	default:
		return "That did not work, and it changed nothing. The reason is in the server log."
	}
}

// addonSize writes a byte count the way an operator reads one.
//
// [byteSize] is not this: it renders exact multiples only, because it words a
// *bound* an operator set — "32 MiB" — and a bound that rendered as 33554432
// bytes would not match the documentation. A schema's size is a measurement and is
// never a round number, so it is rounded to one decimal place at the largest unit
// that leaves a figure above one.
func addonSize(n int64) string {
	switch {
	case n <= 0:
		return "0 B"
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

// plural writes "1 add-on" or "3 add-ons".
//
// Only the two nouns this page counts, both of which take a bare `s`. A general
// pluralizer would be a general pluralizer.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// dedupe keeps the first occurrence of each name, so a doubled checkbox does not
// mean two removals.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
