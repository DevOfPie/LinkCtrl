package httpx

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/instance"
	"github.com/DevOfPie/LinkCtrl/internal/link"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// overviewDays is the dashboard window.
const overviewDays = 30

// fillSeries expands a sparse day series into one entry per day.
//
// The rollup query returns only days that have rows, and a bar chart drawn
// from that lies: a week of silence between two busy days vanishes, and the
// two bars sit side by side as if the traffic were continuous. Absent days
// must render as zero-height bars, so the gap is visible.
func fillSeries(points []analytics.DayPoint, from, to time.Time) []ui.DayCount {
	byDay := make(map[string]analytics.DayPoint, len(points))
	for _, p := range points {
		byDay[p.Day] = p
	}

	from = from.UTC().Truncate(24 * time.Hour)
	to = to.UTC().Truncate(24 * time.Hour)

	var out []ui.DayCount
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		day := d.Format(time.DateOnly)
		p := byDay[day]
		out = append(out, ui.DayCount{
			Day:      day,
			Clicks:   p.Clicks,
			Visitors: p.UniqueVisitors,
			Bots:     p.BotClicks,
		})
	}
	return out
}

// --- dashboard ---------------------------------------------------------------

type dashboardPageData struct {
	shell
	Overview   *analytics.WorkspaceOverview
	Series     []ui.DayCount
	Recent     []domain.Link
	TotalLinks *int64
	// AskUpdateCheck draws the question an instance upgrading into 0.3.0 is put
	// at its first administrative sign-in (M55, D164). See updateCheckAsked.
	AskUpdateCheck bool
}

// updateCheckAsked reports whether this reader is the one being asked whether
// the instance may check for releases (M55, D164).
//
// Four things have to hold, and the order is the cost order rather than the
// argument's.
//
//  1. The deployment allows the check at all. `LINKCTRL_UPDATE_CHECK=false` has
//     already answered, from the side that outranks the browser (D160), so
//     asking would be theatre.
//  2. There is a service to record the answer with.
//  3. The reader holds `instance.admin`. Whether the box phones home is the
//     operator's to decide and nobody else's, which is the same bound the
//     release notification itself is under.
//  4. Nobody has answered yet — and this is the only one that costs a query,
//     which is why it is last. A fresh instance answered at setup and never
//     reaches it; an upgraded one reaches it once per page render until somebody
//     answers, and then never again.
//
// A failed read draws no prompt. This is one control on a page the reader asked
// for something else from, and failing the whole dashboard because the database
// would not answer a question about a checkbox is the wrong trade — the same one
// the notification badge in `shell` makes, for the same reason.
func (h *Web) updateCheckAsked(r *http.Request, actor *auth.Identity) bool {
	if !h.Config.UpdateCheck || h.Instance == nil || !actor.Can(instance.PermAdmin) {
		return false
	}
	answered, err := h.Instance.UpdateCheckAnswered(r.Context(), actor)
	return err == nil && !answered
}

func (h *Web) Dashboard(w http.ResponseWriter, r *http.Request) {
	actor := IdentityFrom(r.Context())
	to := time.Now().UTC()
	from := to.AddDate(0, 0, -(overviewDays - 1))

	overview, err := h.Stats.Overview(r.Context(), actor, from, to)
	if err != nil {
		h.webError(w, r, err)
		return
	}

	recent, err := h.Links.List(r.Context(), actor, domain.LinkFilter{
		Limit: 5, Sort: domain.SortNewest, IncludeTotal: true,
	})
	if err != nil {
		h.webError(w, r, err)
		return
	}

	h.render(w, r, http.StatusOK, "dashboard", dashboardPageData{
		shell:          h.shell(r, "Dashboard", "dashboard"),
		Overview:       overview,
		Series:         fillSeries(overview.Series, from, to),
		Recent:         recent.Items,
		TotalLinks:     recent.Total,
		AskUpdateCheck: h.updateCheckAsked(r, actor),
	})
}

// UpdateCheckAnswer records the answer to the prompt above (M55, D164).
//
// **On the dashboard rather than on a settings page**, because the dashboard is
// where a sign-in lands — `Root` sends a signed-in visitor to `/dashboard` — so
// *at the first administrative sign-in* is where the question actually appears
// rather than where a route diagram says it does. The prompt stays until it is
// answered, so an administrator who signed in with a `?next=` and went straight
// somewhere else meets it the next time they are on the page. D161 refused a
// settings page and this is not one: it is one question, asked once, and
// `instance.AnswerUpdateCheck` refuses a second answer.
//
// Both buttons post here and the value is what differs, so *no* costs the same
// one click *yes* does. A prompt whose refusal is harder than its acceptance is
// a prompt with a preferred answer, which is not what an operator is being asked
// to decide about their own egress.
//
// An answer already given is not an error: the reader wanted the question
// settled and it is. They go back to the dashboard, where the prompt is now
// gone, which is the honest report of what happened.
func (h *Web) UpdateCheckAnswer(w http.ResponseWriter, r *http.Request) {
	if h.Instance == nil {
		h.errorPage(w, r, http.StatusNotFound, "Not found",
			"This instance has no update-check setting to answer.")
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	// Absence is not "no" here, unlike the setup form's checkbox: this is two
	// buttons and a missing value means neither was pressed, which is a request
	// that did not come from the prompt. Recording it as a refusal would let a
	// stray POST answer for the operator.
	answer := r.PostFormValue("answer")
	if answer != "yes" && answer != "no" {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request",
			"That form did not carry an answer.")
		return
	}

	err := h.Instance.AnswerUpdateCheck(r.Context(), IdentityFrom(r.Context()), answer == "yes")
	if err != nil && !errors.Is(err, instance.ErrAlreadyAnswered) {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/dashboard")
}

// --- links list and create ---------------------------------------------------

type linkFormData struct {
	URL, Alias, Title, Description, ExpiresAt, Tags string
	ForwardQuery                                    bool
	ForwardPath                                     bool
	// BotBlocking is the link's own setting as the select renders it: inherit,
	// on or off.
	BotBlocking string
	// FolderID is the folder select's current value: a folder id, or empty for
	// "no folder", which is the option that unfiles a link (M38).
	FolderID string
	// CampaignID is the campaign select's current value: a campaign id, or empty
	// for "no campaign", which is the option that unlabels a link (M41).
	CampaignID string

	// The gates (M35).
	//
	// **There is no Password field and there will not be one.** The form cannot
	// render a password it has no way to read, so the box is always empty, an
	// empty box means "no change", and removing one is a separate checkbox
	// rather than a value. HasPassword is what the page says instead: whether
	// there is one, never what it is.
	HasPassword      bool
	ClearPassword    bool
	MaxClicks        string
	OneTime          bool
	RequireSignature bool
}

type linksPageData struct {
	shell
	Links   []domain.Link
	HasMore bool
	NextURL string
	Total   *int64
	Search  string
	Status  string
	Sort    string
	// Domain is the hostname filter as the query string spells it: a domain id,
	// or empty for no filter (M40). No `none` counterpart, unlike Folder:
	// links.domain_id is NOT NULL, so every link is on exactly one hostname.
	Domain        string
	DomainOptions []domainOption
	// Folder is the folder filter as the query string spells it: a folder id,
	// the word `none` for the links in no folder, or empty for no filter.
	// FolderOptions is the select that sets it, loaded from the same tree the
	// folders page renders.
	Folder        string
	FolderOptions []folderOption
	// Campaign is the campaign filter as the query string spells it: a campaign
	// id, the word `none` for the links carrying none, or empty for no filter
	// (M41). Spelled with the same `none` sentinel the folder filter uses, so
	// the two controls read the same way.
	Campaign        string
	CampaignOptions []campaignOption
	Filtered        bool
	Form            linkFormData
	FieldErrors     map[string]string
	// DisputeURL is the destination an appeal may be filed about, or "" when
	// there is nothing to appeal. Set only after a low-confidence refusal, which
	// is the one tier M31 gives a dispute path — the other two protect a party
	// that is not the one appealing.
	DisputeURL string
	Notice     string
	Error      string
}

// disputableURL reports the destination a refusal may be appealed about.
//
// The tier is the first half of the reason code M30 mints, so reading it here
// puts the button exactly where a dispute would be accepted. It is a rendering
// hint and never an authorization: dispute.File re-judges the URL server-side,
// so a wrong guess here costs a refused POST rather than opening a path.
func disputableURL(err error, raw string) string {
	var ve domain.ValidationErrors
	if raw == "" || !errors.As(err, &ve) {
		return ""
	}
	for _, fe := range ve {
		if strings.HasPrefix(fe.Code, string(link.TierLowConfidence)+".") {
			return raw
		}
	}
	return ""
}

func (h *Web) loadLinksPage(w http.ResponseWriter, r *http.Request) (linksPageData, bool) {
	actor := IdentityFrom(r.Context())
	q := r.URL.Query()

	data := linksPageData{
		shell:       h.shell(r, "Links", "links"),
		Search:      strings.TrimSpace(q.Get("search")),
		Status:      q.Get("status"),
		Sort:        q.Get("sort"),
		Folder:      q.Get("folder"),
		Campaign:    q.Get("campaign"),
		Domain:      q.Get("domain"),
		FieldErrors: map[string]string{},
	}
	if data.Sort == "" {
		data.Sort = "newest"
	}
	if q.Get("deleted") == "1" {
		// Says what recovery actually is. There is no trash view in Phase 1 and
		// RestoreLink refuses soft-deleted rows by design, so "restorable for 30
		// days" sent people looking for a button that does not exist.
		data.Notice = "Link deleted. Its alias stays reserved for 30 days, then the link " +
			"is purged permanently. Recovery inside that window is a database operation."
	}
	// Where an appeal lands. It comes back here rather than to a page of its own
	// because there is nothing to show: the interesting state is now in somebody
	// else's queue, and the person who filed it hears the outcome as a
	// notification rather than by watching a page.
	switch q.Get("dispute") {
	case "filed":
		data.Notice = "Sent for review. You will be notified when the instance owner decides."
	case "duplicate":
		// Not "that destination", which stopped being true when the bound moved
		// from the typed host to the blocklist entry: somebody appealing
		// mail.evil.example is told this because login.evil.example is queued,
		// and the two are one decision rather than one destination.
		data.Notice = "That refusal is already waiting for review. Allowing it will " +
			"clear this destination too."
	case "refused":
		data.Error = "That refusal cannot be appealed. Private addresses, non-web schemes " +
			"and destinations on the curated list are refused for everyone, and no review changes that."
	}

	f := domain.LinkFilter{
		Search:       stripTagPrefix(data.Search),
		Cursor:       q.Get("cursor"),
		Sort:         domain.LinkSort(data.Sort),
		IncludeTotal: true,
	}
	if data.Status != "" {
		f.Status = domain.LinkStatus(data.Status)
	}
	// The folder filter, spelled exactly as the API spells it so a URL copied
	// from one surface works on the other. An id that no longer parses drops the
	// filter rather than emptying the list: a bookmark to a deleted folder should
	// show the links, not suggest they are gone too.
	if data.Folder == folderFilterNone {
		f.Unfiled = true
	} else if id, perr := uuid.Parse(data.Folder); perr == nil {
		f.FolderID = &id
	} else {
		data.Folder = ""
	}
	// The campaign filter (M41), resolved exactly as the folder filter above is
	// and dropping just as quietly when its id no longer names anything.
	if data.Campaign == campaignFilterNone {
		f.Uncampaigned = true
	} else if id, perr := uuid.Parse(data.Campaign); perr == nil {
		f.CampaignID = &id
	} else {
		data.Campaign = ""
	}
	// The hostname filter (M40), spelled as the API spells it so a URL copied
	// from one surface works on the other.
	if id, perr := uuid.Parse(data.Domain); perr == nil {
		f.DomainID = &id
	} else {
		data.Domain = ""
	}

	// The hostname select, on the same terms: a failed read leaves the control
	// off rather than replacing the page. Only when there is more than one
	// hostname to choose between — a filter with one option is a control that
	// cannot change anything, and on a default instance that is every workspace.
	if doms, derr := h.Links.Domains(r.Context(), actor); derr == nil && len(doms) > 1 {
		for _, d := range doms {
			id := d.ID.String()
			data.DomainOptions = append(data.DomainOptions, domainOption{
				ID: id, Hostname: d.Hostname, Selected: id == data.Domain,
			})
		}
	}

	// The folder select. Failing to read the tree leaves the control off the
	// page rather than replacing the page: this is a filter beside a list
	// somebody asked for, and the same trade the shell makes for its switcher.
	if tree, ferr := h.Links.Folders(r.Context(), actor); ferr == nil {
		data.FolderOptions = folderOptions(tree, 0)
		for i := range data.FolderOptions {
			data.FolderOptions[i].Selected = data.FolderOptions[i].ID == data.Folder
		}
	}

	// The campaign select, on the same terms as the folder select above: a
	// failed read leaves the control off the page rather than replacing the page.
	if campaigns, cerr := h.Links.Campaigns(r.Context(), actor); cerr == nil {
		data.CampaignOptions = campaignOptions(campaigns, nil)
		for i := range data.CampaignOptions {
			data.CampaignOptions[i].Selected = data.CampaignOptions[i].ID == data.Campaign
		}
	}

	page, err := h.Links.List(r.Context(), actor, f)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}

	data.Links = page.Items
	data.HasMore = page.HasMore
	data.Total = page.Total
	data.Filtered = data.Search != "" || data.Status != "" || data.Folder != "" ||
		data.Campaign != "" || data.Domain != "" || f.Cursor != ""
	if page.HasMore {
		next := url.Values{}
		if data.Search != "" {
			next.Set("search", data.Search)
		}
		if data.Status != "" {
			next.Set("status", data.Status)
		}
		if data.Folder != "" {
			next.Set("folder", data.Folder)
		}
		if data.Campaign != "" {
			next.Set("campaign", data.Campaign)
		}
		if data.Domain != "" {
			next.Set("domain", data.Domain)
		}
		if data.Sort != "newest" {
			next.Set("sort", data.Sort)
		}
		next.Set("cursor", page.NextCursor)
		data.NextURL = "/links?" + next.Encode()
	}
	return data, true
}

func (h *Web) LinksPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadLinksPage(w, r)
	if !ok {
		return
	}
	// The search box and filters swap just the table; a full navigation gets
	// the whole page. hx-push-url keeps the address bar honest, so a reload of
	// the pushed URL re-renders the complete page through this same branch.
	if isHTMX(r) {
		h.renderPartial(w, r, "links", "links_table", data)
		return
	}
	h.render(w, r, http.StatusOK, "links", data)
}

func (h *Web) LinkCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	created, err := h.Links.Create(r.Context(), IdentityFrom(r.Context()), link.CreateInput{
		URL:   strings.TrimSpace(r.PostFormValue("url")),
		Alias: strings.TrimSpace(r.PostFormValue("alias")),
	})
	if err != nil {
		fields, general := fieldErrors(err)
		if len(fields) == 0 && general == "" {
			h.webError(w, r, err)
			return
		}
		data, ok := h.loadLinksPage(w, r)
		if !ok {
			return
		}
		data.Form = linkFormData{
			URL:   r.PostFormValue("url"),
			Alias: r.PostFormValue("alias"),
		}
		data.FieldErrors = fields
		data.Error = general
		// Offered only where the service would accept one, and only when the
		// dispute surface is wired at all. A button that leads to a 404 is worse
		// than no button.
		if h.Disputes != nil {
			data.DisputeURL = disputableURL(err, strings.TrimSpace(r.PostFormValue("url")))
		}
		h.render(w, r, http.StatusUnprocessableEntity, "links", data)
		return
	}

	seeOther(w, r, "/links/"+created.ID.String()+"?created=1")
}

// --- link detail -------------------------------------------------------------

// statsWindows are the selectable ranges on the detail page.
var statsWindows = []int{7, 30, 90}

type linkDetailPageData struct {
	shell
	Link         *domain.Link
	Stats        *analytics.LinkStats
	Series       []ui.DayCount
	RecentClicks []analytics.RecentClick
	Days         int
	Windows      []int
	Form         linkFormData
	FieldErrors  map[string]string
	Notice       string
	Error        string

	// The tab strip and the panel it has open (M47, reopened). Tabs is built
	// from the same permission gates that guarded the sections when they were a
	// stack, so the strip never names a section this identity would not have
	// been shown; Tab is one of its IDs. Tab state lives in the URL (?tab=) and
	// in nothing else — see activeLinkTab and D178.
	Tabs []linkTab
	Tab  string

	// BotsEnforced says the domain overrules this link's own setting, which is
	// what disables the control rather than letting somebody store a value that
	// does nothing. BotDomainOn is the weaker case: the domain blocks, but the
	// link may still opt out — the template needs it to say what "inherit"
	// currently resolves to.
	BotsEnforced bool
	BotDomainOn  bool
	// BotHost names the domain in the explanation. Empty when the settings could
	// not be read, in which case the control renders with no extra prose rather
	// than with a sentence about nothing.
	BotHost string

	// Routing rules (M34), in the order the redirect path evaluates them.
	Rules []ruleView
	// RuleWeekdays and RuleHelp are the form's own vocabulary, passed in rather
	// than written into the template so that the day names the form offers and
	// the ones the validator accepts cannot drift apart.
	RuleWeekdays []string
	RuleHelp     string
	// GeoAvailable says whether this instance has a MaxMind database at all. A
	// country, region or city rule on an instance without one never matches, and
	// the page says so beside the boxes rather than letting somebody discover it
	// from traffic that did not move.
	//
	// **It is a fact about routing, not about reporting**, and F160 is what
	// happens when the two are conflated. A rule matches going forward, which
	// depends on the database configured now; a breakdown reports backwards,
	// which depends on the rows already written. The map and the ranked list
	// therefore branch on data presence — see fillLinkAnalytics — and only the
	// rule form reads this.
	GeoAvailable bool

	// The country choropleth and the per-dimension rings (M37). Laid out in Go
	// so the templates stay dumb loops and the CSP stays as it is.
	Map    ui.WorldMap
	Donuts map[string]ui.Donut
	// GeoBase is this page's URL with its window already on it, so the map's
	// metric toggle does not have to know how the window is spelled. GeoList is
	// the anchor of the ranked country list, which stays one click from the map.
	GeoBase string
	GeoList string
	// GeoUnavailable is the sentence the ranked list shows when this link's
	// window holds no country to report and the instance has no way to acquire
	// one. It comes from the same constant the map uses, so the two views of this
	// data cannot disagree about whether a country can be reported for it.
	GeoUnavailable string
	// Countries is the ranked country list, and it is deliberately not
	// Stats.Dimensions.country.
	//
	// The list has carried a "no GeoIP database is configured" empty state since
	// it was written, and on an instance with clicks that state was unreachable:
	// a click whose address does not resolve is rolled up under the value
	// "unknown", so the list rendered "unknown — 4,102" and never the sentence.
	// M37 asks the map to say it "exactly as the ranked list already does",
	// which turned out to be a claim about something the list did not do. So the
	// list is given nothing to rank when no country can be reported for this
	// link's window, and its empty state finally means what it says.
	//
	// **"Cannot resolve" is about the rows, not the setting** (F160). The test
	// was `GeoAvailable`, which withheld the list from every instance holding
	// country history without a database configured today — the demo among them.
	Countries []analytics.DimensionValue
	// ReturningAvailable is the same honesty for the returning-visitor
	// condition, which needs Redis.
	ReturningAvailable bool

	// SignedURL is a freshly minted signed link (M35), shown on the page rather
	// than carried back through a redirect. A capability belongs in a response
	// body, not in a URL that lands in browser history, a proxy log and the
	// Referer header of whatever the person clicks next.
	SignedURL     string
	SignedExpires string
	// MinPasswordLength is the floor the service enforces, passed in rather than
	// written into the template so the number the form promises and the number
	// the validator applies cannot drift.
	MinPasswordLength int

	// The split test (M36): the arms in rotation order, the fallback, and the
	// vocabulary the form offers. Nil for every link that has none, which is what
	// keeps the section off a page nobody asked to see it on.
	Split *domain.Split
	// SplitKinds and SplitHelp are the form's own vocabulary, passed in rather
	// than written into the template so the kinds the form offers and the ones
	// the validator accepts cannot drift apart.
	SplitKinds []string
	SplitHelp  string
	// MaxWeight bounds the weight box, so the form refuses what the service
	// would refuse rather than posting it to find out.
	MaxWeight int

	// FolderOptions is every folder this link may be filed in (M38), from the
	// same tree the folders page renders. Empty when there are none, which is
	// what keeps a workspace that has never made one from carrying a select with
	// a single meaningless entry.
	FolderOptions []folderOption

	// CampaignOptions is every campaign this link may carry (M41), on the same
	// terms FolderOptions is loaded on. Empty when the workspace has made none.
	CampaignOptions []campaignOption

	// The link's QR code (M41), as its own embedded view since M48 — the panel
	// and the page at /links/{id}/qr render the same block from the same fields,
	// and a second copy of the field list is a second thing that can disagree.
	linkQRView
}

// linkQRView is everything the `link_qr_body` block reads.
//
// Embedded in two page structs, which is the whole reason it is a type: the QR
// panel on a link's page and the page at /links/{id}/qr are the same markup, and
// m48.md's requirement is that they stay the same markup. Field names are
// promoted, so the template's `.QRSVG` reads identically from either.
//
// It declares no name the shell declares — see TestNoPageDataStructShadowsTheShell,
// which reads embedded mixins as well as named fields for exactly this shape.
type linkQRView struct {
	// QRSVG is the drawing, inlined rather than fetched: it is bytes this
	// process just generated, so an <img> pointing at the API endpoint would be
	// a second authenticated request for something already in hand.
	//
	// Safe as template.HTML because internal/qr builds the document out of
	// integers and colours it has parsed itself; nothing a workspace controls
	// reaches the markup. See the package comment there.
	QRSVG template.HTML
	// QRThumbSVG is the same code drawn small, and it is what the link page puts
	// in its heading row (M48): the owner asked for *"a small render of the QR
	// code near the top that I can click on to open the settings/download button
	// in a pop-up"*, and the picture is what is clicked.
	//
	// A second render rather than the same bytes twice, so the drawing has an
	// intrinsic size of its own for the case where the stylesheet has not
	// applied. It carries `ui.QRThumbClass`, which is what bounds it when the
	// stylesheet has — see the comment where it is built for why the page needs
	// both numbers rather than either one.
	QRThumbSVG template.HTML
	QRContent  string
	// QRSourceLabel is the value a scan appears under in the referrers
	// breakdown, passed in rather than written into the template so the word the
	// page promises and the word the redirect path writes cannot drift apart.
	QRSourceLabel string
	QRStyle       qr.Style
	// QRSize is the output size in pixels the code is drawn at, and since M49 it
	// is the only size the form asks about: the quiet zone and the pixels per
	// module are the arithmetic behind it and have left the interface. QRMinSize
	// and QRMaxSize bound the input, passed in rather than written into the
	// template so the numbers the form offers cannot drift from the ones
	// internal/qr accepts.
	QRSize    int
	QRMinSize int
	QRMaxSize int
	QRStored  bool
	// QRDownload is the SVG and QRDownloadPNG the raster (M49). Both, because
	// the vector is the right file for anything that will be resized again and
	// the raster is the one every other program can open — which is the gap the
	// milestone is closing, and offering only the replacement would leave the
	// people who wanted the SVG converting in the other direction.
	QRDownload    string
	QRDownloadPNG string
	// QRError is what went wrong drawing the code, if anything. The page keeps
	// its analytics and says so, rather than failing over a picture.
	QRError string
	// QRReturn is where the style form lands after a save: the link page when
	// the panel was opened over it, the panel's own page when that is where the
	// reader is standing. It travels on the form because the form is the only
	// thing that knows which, and the handler compares it against the two paths
	// it builds itself rather than following it.
	QRReturn string

	// The link's codes (M50), default first, and which of them the form below
	// them is editing.
	//
	// **The list is the section and the form is one row of it.** m50.md asks for
	// the codes with their labels, sizes and downloads, inside whatever M48
	// decided a panel is — so this is the same `link_qr_body` block, and picking
	// a code is a link back to the panel's own route rather than a second
	// pattern for opening things.
	QRCodes []qrCodeView
	// QRSlug is the code the style form writes to. Empty is the default code,
	// which is what the link page and an unqualified panel show.
	QRSlug string
	// QRMaxCodes is the cap and QRMaxLabel the label bound, both passed in so
	// the numbers the panel shows and the ones the service enforces cannot
	// drift.
	QRMaxCodes int
	QRMaxLabel int
	// QRLabel is the selected code's name, which the rename control edits.
	QRLabel string
	// QRHasLogo says whether the selected code carries an uploaded image
	// (M50.5), and it is what decides whether the panel draws a remove control.
	//
	// A boolean and not the picture: nothing in this milestone serves a stored
	// logo back, so there is nothing to show. What the panel can honestly say is
	// that there is one, and offer to take it away.
	QRHasLogo bool
	// QRMaxLogoBytes, QRMaxLogoDimension and QRMaxLogoPixels are the upload's
	// bounds, passed in like QRMinSize and QRMaxSize so the numbers the panel
	// states and the ones internal/qr enforces cannot drift. Stated in bytes
	// rather than rounded to a megabyte, because a rounded figure is one that
	// stops being true the first time the constant moves and nothing fails.
	//
	// **Two of these are refusals and one is not** (F214). The bytes and the side
	// turn an upload away; the pixel count is the size an image is resized *to*,
	// and the panel's prose has to say which is which or it repeats the refusal
	// that could not be acted on.
	QRMaxLogoBytes     int
	QRMaxLogoDimension int
	QRMaxLogoPixels    int
}

// qrCodeView is one row of the panel's list of codes (M50).
type qrCodeView struct {
	Slug  string
	Label string
	Size  int
	// Name is what the row is titled: the label if it has one, and otherwise a
	// standing description. A row headed with an empty string would be a row
	// nobody can point at, and the default code starts unnamed for every link
	// that existed before this milestone.
	Name string
	// Default marks the code whose payload carries no code parameter — the one
	// every already-printed picture of this link resolves to. It cannot be
	// removed, and the row says so by not offering the control.
	Default bool
	// Selected marks the code the style form below the list is editing.
	Selected bool
	// HasLogo says whether this code carries an uploaded image (M50.5), so a
	// reader can see at a glance which of a link's codes have one without
	// opening each in turn.
	HasLogo     bool
	Panel       string
	Download    string
	DownloadPNG string
	// Clicks is what this code has been scanned, over the window the page is
	// showing. It is the whole point of the milestone — telling two codes apart
	// is telling their numbers apart — and it is read from the same rollup every
	// other breakdown on the page comes from.
	Clicks int64
	// Counted is false when the page had no statistics to read, which is the
	// panel opened at its own route: that page is one section and deliberately
	// does not pay for an analytics read. The template shows nothing rather than
	// a zero, because zero is a claim.
	Counted bool
}

// linkTab is one entry in the strip M47's reopening put on the link page,
// and since M47.5 it carries the tab's own state, because the badge is both
// the answer and the way in: you read `Routing 2` and click `Routing 2`.
type linkTab struct {
	ID, Label string
	// Badge is the strip's vocabulary word for this tab — "count", "check",
	// "cross", "weighted" or "sequential" — and "" for a deliberately bare
	// tab: Danger, whose deletability is a permission rather than state, and
	// Edit, whose protections count did not survive the owner's use (F211). The
	// values are filled by attachTabBadges after the sections' own reads, so
	// a badge shows what its section shows and never a value assembled twice.
	Badge string
	// Count is read only when Badge is "count". Zero renders as a muted `0`
	// rather than no badge at all: a missing badge and a badge reading zero
	// are different claims and a reader cannot tell them apart.
	Count int64
}

// linkTabs is the strip, in the owner-set order of 2026-08-11: seven tabs over
// eight section partials, recent activity folding into Analytics because it is
// the same data one row at a time. The gates are exactly the ones the page put
// on the sections when they were a stack, so a tab exists precisely where its
// section would have rendered — a strip naming a section somebody cannot open
// would be a permission gate drawn as furniture.
func linkTabs(actor *auth.Identity) []linkTab {
	can := func(p string) bool { return actor != nil && actor.Can(p) }
	tabs := make([]linkTab, 0, 7)
	if can("links.update") {
		tabs = append(tabs, linkTab{ID: "edit", Label: "Edit"})
	}
	if can("links.read") {
		tabs = append(tabs,
			linkTab{ID: "qr", Label: "QR"}, linkTab{ID: "routing", Label: "Routing"},
			linkTab{ID: "split", Label: "Split"})
	}
	if can("links.update") {
		tabs = append(tabs, linkTab{ID: "signed", Label: "Signed"})
	}
	tabs = append(tabs, linkTab{ID: "analytics", Label: "Analytics"})
	if can("links.delete") {
		tabs = append(tabs, linkTab{ID: "danger", Label: "Danger"})
	}
	return tabs
}

// attachTabBadges puts each tab's state on its entry in the strip (M47.5).
//
// After the section fills, deliberately, so every badge reads the value its
// section renders rather than a second copy of it — five values assembled
// wrongly would make the strip confidently misleading, which the stack never
// was. Everything here is already computed or one cheap count away; nothing is
// read again.
//
// The sources, position by position:
//
//   - **Edit** takes no badge, since M47.5's reopening: it counted the
//     protections that are on, and the owner judged a count of enabled
//     booleans not worth a badge (F211). It joins Danger as a deliberately
//     bare tab.
//   - **QR** counts the codes the panel lists — the default is among them, the
//     way ListQRCodes answers — so the number on the tab is the number of rows
//     behind it. A failed-soft codes read leaves 0, which is then also what
//     the section shows.
//   - **Routing** counts the rules the section's table draws.
//   - **Split** is not binary: SplitKinds is exactly weighted and sequential,
//     and a link with no split has neither, so the badge is the kind itself —
//     the glyph pair — or the cross. The cross means *the section is empty*,
//     never *no*.
//   - **Signed** is the strip's one true binary and the one badge carrying
//     colour: a check when this link requires signed access, the cross when
//     not. It reads the stored RequireSignature — signed access is a security
//     property worth reading at a glance, and the freshly minted URL the
//     section can hold is deliberately not state (it is shown once and never
//     stored).
//   - **Analytics** is the click count over the window the page is showing,
//     the same figure the section's own totals render.
//   - **Danger** takes no badge; the loop leaves its Badge empty.
func attachTabBadges(data *linkDetailPageData) {
	l := data.Link
	for i := range data.Tabs {
		t := &data.Tabs[i]
		switch t.ID {
		case "qr":
			t.Badge, t.Count = "count", int64(len(data.QRCodes))
		case "routing":
			t.Badge, t.Count = "count", int64(len(data.Rules))
		case "split":
			// Kind, not nil-ness: GetSplit answers a link with no split with an
			// empty Split whose Kind is "", which is exactly the third state the
			// milestone counts. Caught by the kept spec against the running
			// product — the nil test rendered five badges where six are claimed.
			t.Badge = "cross"
			if data.Split != nil && data.Split.Kind != "" {
				t.Badge = data.Split.Kind
			}
		case "signed":
			t.Badge = "cross"
			if l.RequireSignature {
				t.Badge = "check"
			}
		case "analytics":
			t.Badge = "count"
			if data.Stats != nil {
				t.Count = data.Stats.Totals.Clicks
			}
		}
	}
}

// activeLinkTab picks the panel to draw from ?tab=. The default is the strip's
// first entry — the edit form for anyone who may edit, which is the landing the
// design chose because editing is the common task — and an unknown or
// unpermitted value falls back there rather than 404ing: the tab is
// presentation state over one resource, not an object with an existence to
// dispute.
func activeLinkTab(q url.Values, tabs []linkTab) string {
	want := q.Get("tab")
	for _, t := range tabs {
		if t.ID == want {
			return want
		}
	}
	if len(tabs) == 0 {
		return ""
	}
	return tabs[0].ID
}

// loadLinkDetail assembles the link page.
//
// One function per section, and the sections are the partials the page renders
// (M47). It was 207 lines filling eight of them in a single pass, which is the
// state m47.md names: "a partial whose data is assembled by a quarter of a
// 250-line function is not decomposed, it is relocated."
//
// **The split is along the failure rule as much as along the sections**, and
// that is what makes it a seam rather than a tidy-up. Three reads replace the
// page when they fail — the link, its statistics and its recent clicks —
// because without them there is nothing to render. Every read below them fails
// soft and leaves its own section empty: this page is analytics somebody asked
// for, and losing it over a select, a picture or a list of rules would be the
// wrong trade. So the helpers return nothing at all. They fill what they can
// and say nothing about what they could not, which is exactly the contract the
// partials are written against.
func (h *Web) loadLinkDetail(w http.ResponseWriter, r *http.Request) (linkDetailPageData, bool) {
	actor := IdentityFrom(r.Context())

	var data linkDetailPageData
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}

	l, err := h.Links.Get(r.Context(), actor, id)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}

	days := overviewDays
	switch r.URL.Query().Get("days") {
	case "7":
		days = 7
	case "90":
		days = 90
	}
	to := time.Now().UTC()
	from := to.AddDate(0, 0, -(days - 1))

	stats, err := h.Stats.LinkStats(r.Context(), actor, id, from, to)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}
	recent, err := h.Stats.RecentClicks(r.Context(), actor, id, 20)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}

	data = linkDetailPageData{
		shell:        h.shell(r, "/"+l.Alias, "links"),
		Link:         l,
		Stats:        stats,
		RecentClicks: recent,
		Days:         days,
		Windows:      statsWindows,
		FieldErrors:  map[string]string{},
		// Read once, here, because two sections branch on it: the rule form
		// warns that a country condition will never match, and the ranked
		// country list has its own empty state for the same fact.
		GeoAvailable: h.Config.Analytics.GeoIPPath != "",
	}
	data.Tabs = linkTabs(actor)
	data.Tab = activeLinkTab(r.URL.Query(), data.Tabs)

	h.fillLinkEdit(r.Context(), actor, &data)
	// The link page always shows the default code. Which code the *panel* is
	// open on is the panel route's business, and a query parameter on the link
	// page would put the same state in two places.
	data.linkQRView = h.linkQR(r.Context(), actor, l, "/links/"+l.ID.String(), "")
	h.fillLinkRouting(r.Context(), actor, &data)
	fillLinkAnalytics(r, &data, from, to)
	attachQRCounts(&data.linkQRView, data.Stats)
	// Last, so every badge reads the value its section just filled (M47.5).
	attachTabBadges(&data)
	data.Notice = linkDetailNotice(r.URL.Query(), l)

	return data, true
}

// fillLinkEdit is the `link_edit` partial's data: the link's own values as the
// form renders them, the two selects that file it, and what the domain above it
// says about automated clients.
func (h *Web) fillLinkEdit(ctx context.Context, actor *auth.Identity, data *linkDetailPageData) {
	l := data.Link

	form := linkFormData{
		URL:              l.URL,
		Alias:            l.Alias,
		Title:            l.Title,
		Description:      l.Description,
		ForwardQuery:     l.ForwardQuery,
		ForwardPath:      l.ForwardPath,
		BotBlocking:      string(l.BotBlocking),
		HasPassword:      l.HasPassword,
		OneTime:          l.OneTime,
		RequireSignature: l.RequireSignature,
	}
	if l.MaxClicks != nil {
		form.MaxClicks = strconv.FormatInt(*l.MaxClicks, 10)
	}
	if l.FolderID != nil {
		form.FolderID = l.FolderID.String()
	}
	if l.CampaignID != nil {
		form.CampaignID = l.CampaignID.String()
	}
	if l.ExpiresAt != nil {
		form.ExpiresAt = l.ExpiresAt.UTC().Format("2006-01-02T15:04")
	}
	names := make([]string, 0, len(l.Tags))
	for _, t := range l.Tags {
		names = append(names, t.Name)
	}
	form.Tags = strings.Join(names, ", ")
	data.Form = form
	data.MinPasswordLength = auth.MinPasswordLength

	// What the domain above this link says. A read failure leaves the control
	// rendering as an ordinary select: this is one page's explanatory text, not
	// a reason to replace the page, and the service refuses an unhonourable
	// value on submit regardless of what was rendered.
	//
	// **This link's domain, not the instance default (F89).** It read
	// `DomainSettings` — the default row — for every link, so a link on a
	// verified custom hostname had its control disabled or enabled by another
	// hostname's policy and the sentence beneath it named a hostname the link is
	// not served on. `LinkDomainBots` reads the same row the API's own refusal
	// reads, which is what makes the two surfaces agree by construction rather
	// than by both happening to look at the default.
	if bots, serr := h.Links.LinkDomainBots(ctx, actor, l.ID); serr == nil {
		policy := domain.DomainBots(bots.BlockBots, bots.BlockBotsEnforced)
		data.BotsEnforced = domain.BotPolicyLocked(policy)
		data.BotDomainOn = bots.BlockBots
		data.BotHost = bots.Hostname
	}

	// The folders this link could be filed in, and the campaigns it could carry.
	// Both are read and failed the way everything below the three page-replacing
	// reads is: losing the page over a select would be the wrong trade.
	if tree, ferr := h.Links.Folders(ctx, actor); ferr == nil {
		data.FolderOptions = folderOptions(tree, 0)
		for i := range data.FolderOptions {
			data.FolderOptions[i].Selected = data.FolderOptions[i].ID == form.FolderID
		}
	}
	if campaigns, cerr := h.Links.Campaigns(ctx, actor); cerr == nil {
		data.CampaignOptions = campaignOptions(campaigns, nil)
		for i := range data.CampaignOptions {
			data.CampaignOptions[i].Selected = data.CampaignOptions[i].ID == form.CampaignID
		}
	}
}

// qrThumbScale is the module size the heading row's thumbnail is drawn at (M48).
//
// **It is the fallback size and not the rendered one.** What a reader sees is
// `ui.QRThumbClass`, because a stylesheet rule beats a presentation attribute.
// This is what a page whose stylesheet never arrived draws, and it exists so
// that page does not put the full 296–328px code above the destination box.
//
// Three, which puts a short URL with `?src=qr` on it at 111px square including
// its quiet zone, and the demo's longer host at 123px — fifteen to
// twenty-seven pixels above the 96px the class states, which is the closest a
// scale can come to a class it cannot see. It cannot come closer, and that
// asymmetry is the reason the class exists: the pixel count is a function of
// the encoded version and the box the heading row can afford is not. Two would
// undershoot by as much in the other direction, and internal/qr's MinScale
// refuses anything below it, so no clamping happens behind the number.
const qrThumbScale = 3

// qrThumb draws the heading row's thumbnail: the reader's own colours and
// error-correction level, small, and boxed by a class.
//
// The quiet zone travels with it rather than being trimmed — a code without one
// is a code scanners refuse, and a thumbnail that could not be scanned would be
// a picture of a QR code rather than one.
//
// **Both the scale and the class, and they are not the same statement.**
// `ui.QRThumbClass` is what the heading row's height is *read off*, because it
// is the same for every link; the scale is what the picture measures when no
// stylesheet applies, and without it that fallback is the full 296–328px code
// where the class says 96.
//
// A failure draws nothing and says nothing: the full code has already rendered
// by the time this is called, so the section has its subject, and reporting
// that a decoration could not be drawn would be reporting a problem the reader
// cannot act on. The QR section draws its worded trigger regardless, so the
// panel is never unreachable.
//
// A function rather than four lines in linkQR because the class is the half of
// this that a test can hold on to without a database — see
// TestTheQRThumbnailStatesItsOwnHeight.
//
// The logo travels with it (M50.6): the thumbnail is a picture of the code, and
// one that left the logo out would be a picture of a different code. It costs
// almost nothing — internal/qr resamples the image to the box it is drawn in,
// and this box is a fifth of a 96-pixel drawing.
func qrThumb(content string, style qr.Style, logo []byte) template.HTML {
	style.Scale = qrThumbScale
	svg, err := qr.RenderClassWithLogo(content, style, ui.QRThumbClass, logo)
	if err != nil {
		return ""
	}
	//nolint:gosec // G203: internal/qr emits integers, parsed colours, a checked class and base64
	return template.HTML(svg)
}

// linkQR is the `link_qr` and `link_qr_body` blocks' data.
//
// The two failure messages are different on purpose: a code that cannot be read
// and a code that cannot be drawn are different problems, and the second one
// still has content to show beneath it.
//
// `back` is where the style form returns to, which differs by the surface that
// is rendering: the link page passes its own path, the panel's page passes that.
// It is a parameter rather than something read off the request, so the value the
// form carries is always one this function chose.
func (h *Web) linkQR(
	ctx context.Context, actor *auth.Identity, l *domain.Link, back, slug string,
) linkQRView {
	view := linkQRView{
		QRSourceLabel:      domain.ClickSourceQR,
		QRMinSize:          qr.MinSize,
		QRMaxSize:          qr.MaxSize,
		QRMaxCodes:         domain.MaxQRCodesPerLink,
		QRMaxLabel:         domain.MaxQRCodeLabelLength,
		QRMaxLogoBytes:     qr.MaxLogoUploadBytes,
		QRMaxLogoDimension: qr.MaxLogoDimension,
		QRMaxLogoPixels:    qr.MaxLogoPixels,
		QRReturn:           back,
		QRSlug:             slug,
		QRDownload:         qrDownloadPath(l.ID, slug, "svg"),
		QRDownloadPNG:      qrDownloadPath(l.ID, slug, "png"),
	}

	// The list first, so a panel opened on a code that has since been removed
	// falls back to the default rather than showing an error where a picture
	// goes. Failed soft like every other read on this page: a list that cannot
	// be read leaves the section showing one code, which is what the link had
	// before this milestone.
	if codes, cerr := h.Links.ListQRCodes(ctx, actor, l.ID); cerr == nil {
		view.QRCodes = qrCodeViews(l.ID, codes, slug)
		if !qrCodeExists(codes, slug) {
			view.QRSlug, slug = "", ""
			view.QRCodes = qrCodeViews(l.ID, codes, "")
			view.QRDownload = qrDownloadPath(l.ID, "", "svg")
			view.QRDownloadPNG = qrDownloadPath(l.ID, "", "png")
		}
	}

	code, err := h.Links.QRCodeBySlug(ctx, actor, l.ID, slug)
	if err != nil {
		view.QRError = "The QR code could not be read."
		return view
	}
	view.QRContent = code.Content
	view.QRStyle = code.Style
	view.QRSize = code.Size
	view.QRStored = code.Stored
	view.QRLabel = code.Label
	view.QRHasLogo = code.HasLogo

	// The image, for the code the panel is open on (M50.6). Read only when there
	// is one, and failed soft like every other read here: a picture without its
	// logo is still the code, and losing the section over a megabyte that would
	// not load would be the wrong trade.
	var logo []byte
	if code.HasLogo {
		logo, _ = h.Links.QRCodeLogo(ctx, actor, l.ID, slug)
	}

	svg, err := qr.RenderWithLogo(code.Content, code.Style, logo)
	if err != nil {
		view.QRError = "The QR code could not be drawn."
		return view
	}
	//nolint:gosec // G203: internal/qr emits integers, parsed colours and base64
	view.QRSVG = template.HTML(svg)

	// The thumbnail is always the default code. It is the link page's heading
	// row and it stands for "this link has a QR code", not for whichever one the
	// panel was last left open on.
	if slug == "" {
		view.QRThumbSVG = qrThumb(code.Content, code.Style, logo)
	} else if def, derr := h.Links.QRCode(ctx, actor, l.ID); derr == nil {
		var defLogo []byte
		if def.HasLogo {
			defLogo, _ = h.Links.QRCodeLogo(ctx, actor, l.ID, "")
		}
		view.QRThumbSVG = qrThumb(def.Content, def.Style, defLogo)
	}
	return view
}

// qrCodeExists says whether the list holds the slug the panel was opened on.
func qrCodeExists(codes []link.QRCode, slug string) bool {
	for _, c := range codes {
		if c.Slug == slug {
			return true
		}
	}
	return false
}

// qrDownloadPath is where one code's picture is fetched from.
//
// The default code keeps the paths M41 and M49 shipped; a named code uses the
// collection M50 added. One function, because the panel builds two of these per
// row and a second copy of the branch is a second answer to "where is this
// picture".
func qrDownloadPath(linkID uuid.UUID, slug, ext string) string {
	if slug == "" {
		return fmt.Sprintf("%s/links/%s/qr.%s", APIPrefix, linkID, ext)
	}
	return fmt.Sprintf("%s/links/%s/qr/codes/%s/image.%s", APIPrefix, linkID, slug, ext)
}

// qrCodeViews turns the service's codes into the panel's rows.
func qrCodeViews(linkID uuid.UUID, codes []link.QRCode, selected string) []qrCodeView {
	out := make([]qrCodeView, 0, len(codes))
	for _, c := range codes {
		name := c.Label
		if name == "" {
			if c.Slug == "" {
				name = "The original code"
			} else {
				name = "Unnamed code"
			}
		}
		panel := fmt.Sprintf("/links/%s/qr", linkID)
		if c.Slug != "" {
			panel += "?code=" + url.QueryEscape(c.Slug)
		}
		out = append(out, qrCodeView{
			Slug: c.Slug, Label: c.Label, Size: c.Size, Name: name, HasLogo: c.HasLogo,
			Default: c.Slug == "", Selected: c.Slug == selected, Panel: panel,
			Download:    qrDownloadPath(linkID, c.Slug, "svg"),
			DownloadPNG: qrDownloadPath(linkID, c.Slug, "png"),
		})
	}
	return out
}

// attachQRCounts puts each code's scans on its row (M50).
//
// Read off the statistics the page already loaded rather than by asking again:
// the per-code breakdown is a filter over the referrer dimension that was rolled
// up with everything else, so the number is already in hand by the time this
// runs. A code the breakdown does not mention has not been scanned in the
// window, which is a zero rather than a blank — Counted is what distinguishes
// that from the panel's own page, which loads no statistics at all.
func attachQRCounts(view *linkQRView, stats *analytics.LinkStats) {
	if stats == nil {
		return
	}
	byCode := make(map[string]int64, len(stats.QRCodes))
	for _, c := range stats.QRCodes {
		byCode[c.Slug] = c.Clicks
	}
	for i := range view.QRCodes {
		view.QRCodes[i].Clicks = byCode[view.QRCodes[i].Slug]
		view.QRCodes[i].Counted = true
	}
}

// fillLinkRouting is the data behind `link_rules` and `link_split`, which are
// one section in two halves: a rule says who goes where and a split says how
// many, and the redirect path evaluates them in that order.
//
// The vocabularies are passed in rather than written into the templates so the
// days, kinds and bounds the forms offer cannot drift from the ones the
// validator accepts.
func (h *Web) fillLinkRouting(ctx context.Context, actor *auth.Identity, data *linkDetailPageData) {
	id := data.Link.ID

	// Read and failed the way the folder tree is: this page is analytics
	// somebody asked for, and losing it over a list of rules or a list of arms
	// would be the wrong trade.
	if rules, rerr := h.Links.ListRules(ctx, actor, id); rerr == nil {
		data.Rules = ruleViews(rules)
	}
	if split, serr := h.Links.GetSplit(ctx, actor, id); serr == nil {
		data.Split = split
	}

	data.RuleWeekdays = domain.RuleWeekdays
	data.RuleHelp = ruleConditionHelp
	data.SplitKinds = domain.SplitKinds
	data.SplitHelp = splitHelp
	data.MaxWeight = domain.MaxDestinationWeight
	// The returning-visitor condition needs Redis, and the form says so beside
	// the control rather than letting somebody discover it from traffic that did
	// not move.
	data.ReturningAvailable = h.Config.Redis.URL != ""
}

// fillLinkAnalytics is the `link_analytics` partial's data: the series, the
// choropleth, the rings and the ranked country list.
//
// No receiver and no context, and that is the point of the seam rather than an
// accident of it — every figure here is already in `data.Stats`, so this half of
// the page performs no I/O and cannot fail. It is laid out in Go so the
// templates stay dumb loops and the CSP stays as it is.
func fillLinkAnalytics(r *http.Request, data *linkDetailPageData, from, to time.Time) {
	stats := data.Stats
	data.Series = fillSeries(stats.Series, from, to)

	// **Whether there is a country breakdown to show is a question about the
	// data, not about the configuration** (F160).
	//
	// Both surfaces used to be suppressed on `GeoAvailable` alone, so an
	// instance holding country history with no database configured *today* was
	// shown "Geographic data is unavailable" over rows that are present and
	// correct. That is the demo — `.env.demo` sets no GeoIP path while the
	// database holds 8,123 country rows and two demoCoverage() rows exist to
	// guarantee the choropleth is worth looking at — and it is not
	// demo-specific: any instance that configured GeoIP, accumulated history and
	// later removed the file saw the same sentence over real data.
	//
	// Configuration still counts, because it answers the other half: an instance
	// with a database and no clicks yet is a link nobody has clicked, and its
	// empty state is the ordinary "No data yet" rather than a claim about the
	// instance. So the sentence is reached only when both are false — nothing
	// resolved, and nothing can resolve.
	//
	// **"Nothing resolved" is this link, in the window `stats` was read for**,
	// which is narrower than F160's fix note and the claims written around it.
	// On a database-less instance holding country history, a link with no country
	// inside the selected window meets the sentence while the link beside it
	// draws a map. Widening the test to the link's whole history costs a query,
	// and the seam this function is is that it makes none, so the residue is
	// recorded as F195 rather than closed by adding one here.
	geoShowable := data.GeoAvailable || hasResolvedCountries(stats.Dimensions["country"])
	if !geoShowable {
		data.GeoUnavailable = ui.GeoUnavailable
	}
	// tab=analytics because every URL built from this base re-renders the page
	// for the sake of something the analytics panel draws (M47, reopened): the
	// map's metric toggle and the ranked-list anchor would otherwise land the
	// reader back on the strip's landing tab, holding the answer out of sight.
	data.GeoBase = fmt.Sprintf("/links/%s?tab=analytics&days=%d", data.Link.ID, data.Days)
	data.GeoList = data.GeoBase + "#countries"

	// The choropleth. Shaded by clicks unless asked for visitors, and the
	// caveat travels with the figures rather than being retyped here: shading a
	// map by a daily-resolution estimate without repeating the sentence that
	// says so would launder an estimate into a fact.
	geoMetric := "clicks"
	if r.URL.Query().Get("geo") == "visitors" {
		geoMetric = "visitors"
	}
	data.Map = ui.Choropleth(
		countryValues(stats.Dimensions["country"], geoMetric),
		geoMetric, stats.Caveat, geoShowable)
	if geoShowable {
		data.Countries = stats.Dimensions["country"]
	}

	// The rings. Every dimension the page ranks gets one, including countries:
	// the map answers "where", the ring answers "how concentrated", and the list
	// answers "exactly how many".
	data.Donuts = make(map[string]ui.Donut, len(stats.Dimensions))
	for dim, values := range stats.Dimensions {
		data.Donuts[dim] = ui.DonutChart(dimensionSlices(values), stats.Totals.Clicks, 100)
	}
}

// linkDetailNotice is the flash the page opens with, chosen from what the last
// redirect put in the query string.
//
// First match wins and the order is the order it was written in, which matters
// only for a hand-made URL carrying two of them.
func linkDetailNotice(q url.Values, l *domain.Link) string {
	switch {
	case q.Get("rule") != "":
		return ruleNotice(q.Get("rule"))
	case q.Get("split") != "":
		return splitNotice(q.Get("split"))
	case q.Get("qr") != "":
		return qrNotice(q)
	case q.Get("created") == "1":
		return "Link created: " + l.ShortURL
	case q.Get("saved") == "1":
		return "Changes saved."
	case q.Get("archived") == "1":
		return "Link archived. It no longer redirects; the alias stays reserved."
	case q.Get("restored") == "1":
		return "Link restored."
	}
	return ""
}

// hasResolvedCountries reports whether this breakdown holds a country at all
// (F160).
//
// "unknown" is what the rollup writes for a click whose address did not
// resolve, and it is not a place — a window made entirely of it is a window in
// which nothing resolved, which is the state the unavailable sentence describes.
// The same exclusion countryValues makes, for the same reason, and it is why
// this is not `len(values) > 0`.
//
// Clicks rather than the metric on screen: the toggle chooses which figure to
// shade, not whether there is anything to shade, and a country with clicks and a
// visitor estimate of zero is still a country somebody came from.
func hasResolvedCountries(values []analytics.DimensionValue) bool {
	for _, v := range values {
		if v.Value == "" || v.Value == "unknown" {
			continue
		}
		if v.Clicks > 0 || v.UniqueVisitors > 0 {
			return true
		}
	}
	return false
}

// countryValues turns the country breakdown into the per-code figures the
// choropleth shades.
//
// The rollup already caps the breakdown at the top twenty values, which is the
// right bound for a list and the wrong one for a map: a country outside the top
// twenty is simply not drawn, and the map says so through the total in its
// legend rather than pretending nobody came from there. That bound is the
// reader's, not this function's — see GetLinkDimensions' row_limit.
//
// The literal value "unknown" is dropped rather than mapped to a shape. It is
// what the rollup writes for a click whose address did not resolve, and it is
// not a place.
func countryValues(values []analytics.DimensionValue, metric string) map[string]int64 {
	out := make(map[string]int64, len(values))
	for _, v := range values {
		if v.Value == "" || v.Value == "unknown" {
			continue
		}
		n := v.Clicks
		if metric == "visitors" {
			n = v.UniqueVisitors
		}
		if n > 0 {
			out[v.Value] += n
		}
	}
	return out
}

// dimensionSlices converts a breakdown into the two fields the ring needs.
//
// Three assignments in a loop, so that a template change can never pull the
// analytics package into `ui`, which depends on nothing outside the standard
// library and is meant to keep doing so.
func dimensionSlices(values []analytics.DimensionValue) []ui.DimensionSlice {
	out := make([]ui.DimensionSlice, 0, len(values))
	for _, v := range values {
		out = append(out, ui.DimensionSlice{Name: v.Value, Count: v.Clicks})
	}
	return out
}

func (h *Web) LinkDetail(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadLinkDetail(w, r)
	if !ok {
		return
	}
	h.render(w, r, http.StatusOK, "link_detail", data)
}

func (h *Web) LinkUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	// The form posts every field, so every field is set — this is a full
	// update, unlike the API's PATCH. An empty expiry clears it, which is the
	// only way a form can express "remove".
	urlv := strings.TrimSpace(r.PostFormValue("url"))
	alias := strings.TrimSpace(r.PostFormValue("alias"))
	title := strings.TrimSpace(r.PostFormValue("title"))
	desc := strings.TrimSpace(r.PostFormValue("description"))
	tags := splitTags(r.PostFormValue("tags"))
	// A checkbox is absent when unchecked, so its mere presence is the value —
	// and since this form posts every field, absence really does mean "off"
	// rather than "leave alone".
	forward := r.PostFormValue("forward_query") != ""
	forwardPath := r.PostFormValue("forward_path") != ""
	oneTime := r.PostFormValue("one_time") != ""
	requireSig := r.PostFormValue("require_signature") != ""
	clearPassword := r.PostFormValue("clear_password") != ""

	in := link.UpdateInput{
		URL: &urlv, Alias: &alias, Title: &title, Description: &desc, Tags: &tags,
		ForwardQuery: &forward, ForwardPath: &forwardPath,
		OneTime: &oneTime, RequireSignature: &requireSig,
		ClearPassword: clearPassword,
	}

	// The password box (M35), and the asymmetry with every other field on this
	// form is the whole design. The form posts every field and absence means
	// "off" — except here, because nobody can re-type a password they cannot
	// read. An empty box therefore means "leave it", and removal is the
	// checkbox beside it.
	if pw := r.PostFormValue("password"); pw != "" && !clearPassword {
		in.Password = &pw
	}
	rawMax := strings.TrimSpace(r.PostFormValue("max_clicks"))
	switch rawMax {
	case "":
		in.ClearMaxClicks = true
	default:
		n, cerr := strconv.ParseInt(rawMax, 10, 64)
		if cerr != nil {
			n = 0 // refused by the service, with the field error the form shows
		}
		in.MaxClicks = &n
	}

	// Which campaign the link carries (M41), read exactly as the folder select
	// below is and for the same reason: this form posts every field.
	if raw := strings.TrimSpace(r.PostFormValue("campaign_id")); raw == "" {
		in.ClearCampaign = true
	} else {
		campaignID, cerr := uuid.Parse(raw)
		if cerr != nil {
			h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
			return
		}
		in.CampaignID = &campaignID
	}

	// Where the link is filed (M38). This form posts every field, so an empty
	// select really is "take it out of every folder" rather than "leave it
	// alone" — the same rule the checkboxes above follow, and the reason
	// UpdateInput carries an explicit ClearFolder.
	if raw := strings.TrimSpace(r.PostFormValue("folder_id")); raw == "" {
		in.ClearFolder = true
	} else {
		folderID, ferr := uuid.Parse(raw)
		if ferr != nil {
			h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
			return
		}
		in.FolderID = &folderID
	}

	// The one field this form may legitimately omit. A disabled select posts
	// nothing, which is exactly how the enforced case has to reach the service:
	// as "leave it alone", not as a value the service would then have to refuse
	// on a form the browser never let anybody change. An unreadable value is
	// refused rather than ignored, because the only way one arrives is a
	// hand-made POST.
	if raw := r.PostFormValue("bot_blocking"); raw != "" {
		policy, ok := domain.ParseBotPolicy(raw)
		if !ok {
			h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
			return
		}
		in.BotBlocking = &policy
	}

	rerender := func(fields map[string]string, general string) {
		data, ok := h.loadLinkDetail(w, r)
		if !ok {
			return
		}
		data.Form = linkFormData{
			URL: urlv, Alias: alias, Title: title, Description: desc,
			ExpiresAt:    r.PostFormValue("expires_at"),
			Tags:         r.PostFormValue("tags"),
			ForwardQuery: forward,
			ForwardPath:  forwardPath,
			BotBlocking:  r.PostFormValue("bot_blocking"),
			FolderID:     r.PostFormValue("folder_id"),
			CampaignID:   r.PostFormValue("campaign_id"),
			// Re-rendered from what was posted, except the password: the box
			// comes back empty because there is nothing safe to put in it and
			// nothing the form is entitled to remember.
			HasPassword:      data.Link != nil && data.Link.HasPassword,
			ClearPassword:    clearPassword,
			MaxClicks:        rawMax,
			OneTime:          oneTime,
			RequireSignature: requireSig,
		}
		data.FieldErrors = fields
		data.Error = general
		h.render(w, r, http.StatusUnprocessableEntity, "link_detail", data)
	}

	if raw := strings.TrimSpace(r.PostFormValue("expires_at")); raw == "" {
		in.ClearExpiry = true
	} else {
		// datetime-local submits "2006-01-02T15:04" with no zone. The field is
		// labelled UTC, so that is how it is read.
		at, err := time.ParseInLocation("2006-01-02T15:04", raw, time.UTC)
		if err != nil {
			rerender(map[string]string{"expires_at": "Enter a date and time, or leave it empty for no expiry."}, "")
			return
		}
		in.ExpiresAt = &at
	}

	if _, err := h.Links.Update(r.Context(), IdentityFrom(r.Context()), id, in); err != nil {
		fields, general := fieldErrors(err)
		if len(fields) == 0 && general == "" {
			h.webError(w, r, err)
			return
		}
		rerender(fields, general)
		return
	}

	seeOther(w, r, "/links/"+id.String()+"?saved=1")
}

// LinkSign mints a signed URL and shows it on the link's own page (M35).
//
// **Deliberately not a POST-redirect-GET**, which is what every other write on
// this page does. The thing being produced is a bearer capability, and carrying
// it back through a redirect would put it in the browser's address bar, its
// history, the proxy log in between and the Referer header of whatever the
// person clicks next. Rendering it in the response body keeps it in one place
// the person can copy from and nowhere else.
func (h *Web) LinkSign(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}

	var ttl time.Duration
	if raw := strings.TrimSpace(r.PostFormValue("ttl_hours")); raw != "" {
		hours, perr := strconv.Atoi(raw)
		if perr != nil || hours <= 0 {
			h.errorPage(w, r, http.StatusBadRequest, "Bad request",
				"The signature lifetime could not be read.")
			return
		}
		// Saturated, not multiplied raw: the form's min/max live in the
		// browser, and hours large enough to wrap the nanosecond product could
		// otherwise come out beneath the service's ceiling. Pinning them just
		// past it lets the service refuse with its own out_of_range message.
		ttl = saturateDuration(int64(hours), time.Hour, link.MaxSignatureTTL)
	}

	signed, err := h.Links.Sign(r.Context(), IdentityFrom(r.Context()), id, ttl)
	if err != nil {
		fields, general := fieldErrors(err)
		if len(fields) == 0 && general == "" {
			h.webError(w, r, err)
			return
		}
		data, ok := h.loadLinkDetail(w, r)
		if !ok {
			return
		}
		// A POST carries no ?tab=, and both of this handler's renders exist to
		// show something the signed section draws — the refusal beside its
		// form, the minted URL in its box. Re-derived here rather than read
		// from the form, the way every section-owned write on this page does
		// it (D178).
		data.Tab = "signed"
		data.FieldErrors = fields
		data.Error = general
		h.render(w, r, http.StatusUnprocessableEntity, "link_detail", data)
		return
	}

	data, ok := h.loadLinkDetail(w, r)
	if !ok {
		return
	}
	data.Tab = "signed"
	data.SignedURL = signed.URL
	data.SignedExpires = signed.ExpiresAt.Format("2006-01-02 15:04 UTC")
	h.render(w, r, http.StatusOK, "link_detail", data)
}

func splitTags(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (h *Web) LinkArchive(w http.ResponseWriter, r *http.Request) {
	h.linkLifecycle(w, r, "archived", func(r *http.Request) error {
		id, err := pathUUID(r, "id")
		if err != nil {
			return err
		}
		_, err = h.Links.Archive(r.Context(), IdentityFrom(r.Context()), id)
		return err
	})
}

func (h *Web) LinkRestore(w http.ResponseWriter, r *http.Request) {
	h.linkLifecycle(w, r, "restored", func(r *http.Request) error {
		id, err := pathUUID(r, "id")
		if err != nil {
			return err
		}
		_, err = h.Links.Restore(r.Context(), IdentityFrom(r.Context()), id)
		return err
	})
}

func (h *Web) linkLifecycle(w http.ResponseWriter, r *http.Request, notice string, op func(*http.Request) error) {
	if err := op(r); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/links/"+r.PathValue("id")+"?"+notice+"=1")
}

func (h *Web) LinkDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	if err := h.Links.Delete(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		h.webError(w, r, err)
		return
	}
	seeOther(w, r, "/links?deleted=1")
}

// domainOption is one entry in the links page's hostname filter (M40).
type domainOption struct {
	ID       string
	Hostname string
	Selected bool
}
