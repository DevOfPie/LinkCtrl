package httpx

import (
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
		shell:      h.shell(r, "Dashboard", "dashboard"),
		Overview:   overview,
		Series:     fillSeries(overview.Series, from, to),
		Recent:     recent.Items,
		TotalLinks: recent.Total,
	})
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
	// GeoUnavailable is the sentence the ranked list shows with no GeoIP
	// database configured. It comes from the same constant the map uses, so the
	// two views of this data cannot disagree about whether the instance can
	// resolve a country at all.
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
	// list is given nothing to rank when the instance cannot resolve a country,
	// and its empty state finally means what it says.
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

	// The link's QR code (M41). QRSVG is the drawing, inlined rather than
	// fetched: it is bytes this process just generated, so an <img> pointing at
	// the API endpoint would be a second authenticated request for something
	// already in hand.
	//
	// Safe as template.HTML because internal/qr builds the document out of
	// integers and colours it has parsed itself; nothing a workspace controls
	// reaches the markup. See the package comment there.
	QRSVG     template.HTML
	QRContent string
	// QRSourceLabel is the value a scan appears under in the referrers
	// breakdown, passed in rather than written into the template so the word the
	// page promises and the word the redirect path writes cannot drift apart.
	QRSourceLabel string
	QRStyle       qr.Style
	QRLevels      []qr.Level
	QRStored      bool
	QRDownload    string
	// QRError is what went wrong drawing the code, if anything. The page keeps
	// its analytics and says so, rather than failing over a picture.
	QRError string
}

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

	data = linkDetailPageData{
		shell:        h.shell(r, "/"+l.Alias, "links"),
		Link:         l,
		Stats:        stats,
		Series:       fillSeries(stats.Series, from, to),
		RecentClicks: recent,
		Days:         days,
		Windows:      statsWindows,
		Form:         form,
		FieldErrors:  map[string]string{},
	}

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
	if bots, serr := h.Links.LinkDomainBots(r.Context(), actor, id); serr == nil {
		policy := domain.DomainBots(bots.BlockBots, bots.BlockBotsEnforced)
		data.BotsEnforced = domain.BotPolicyLocked(policy)
		data.BotDomainOn = bots.BlockBots
		data.BotHost = bots.Hostname
	}

	// The link's routing rules. A read failure leaves the section empty rather
	// than replacing the page: the rest of this page is analytics somebody asked
	// for, and losing it over a rule list would be the wrong trade.
	if rules, rerr := h.Links.ListRules(r.Context(), actor, id); rerr == nil {
		data.Rules = ruleViews(rules)
	}
	// The link's split test. Read and failed the same way the rules are, for the
	// same reason: this page is analytics somebody asked for, and losing it over
	// a list of arms would be the wrong trade.
	if split, serr := h.Links.GetSplit(r.Context(), actor, id); serr == nil {
		data.Split = split
	}
	// The folders this link could be filed in. Read and failed the same way the
	// rules and the split are: this page is analytics somebody asked for, and
	// losing it over a select would be the wrong trade.
	if tree, ferr := h.Links.Folders(r.Context(), actor); ferr == nil {
		data.FolderOptions = folderOptions(tree, 0)
		for i := range data.FolderOptions {
			data.FolderOptions[i].Selected = data.FolderOptions[i].ID == form.FolderID
		}
	}
	// The campaigns this link could carry, and the link's own QR code. Both are
	// read and failed exactly as the rules, the split and the folder tree above
	// are: this page is analytics somebody asked for, and losing it over a
	// select or a picture would be the wrong trade.
	if campaigns, cerr := h.Links.Campaigns(r.Context(), actor); cerr == nil {
		data.CampaignOptions = campaignOptions(campaigns, nil)
		for i := range data.CampaignOptions {
			data.CampaignOptions[i].Selected = data.CampaignOptions[i].ID == form.CampaignID
		}
	}
	data.QRLevels = qr.Levels
	data.QRSourceLabel = domain.ClickSourceQR
	data.QRDownload = fmt.Sprintf("%s/links/%s/qr.svg", APIPrefix, l.ID)
	if code, qerr := h.Links.QRCode(r.Context(), actor, id); qerr != nil {
		data.QRError = "The QR code could not be read."
	} else {
		data.QRContent = code.Content
		data.QRStyle = code.Style
		data.QRStored = code.Stored
		if svg, rerr := qr.Render(code.Content, code.Style); rerr != nil {
			data.QRError = "The QR code could not be drawn."
		} else {
			//nolint:gosec // G203: internal/qr emits integers and parsed colours only
			data.QRSVG = template.HTML(svg)
		}
	}

	data.RuleWeekdays = domain.RuleWeekdays
	data.RuleHelp = ruleConditionHelp
	data.SplitKinds = domain.SplitKinds
	data.SplitHelp = splitHelp
	data.MaxWeight = domain.MaxDestinationWeight
	data.GeoAvailable = h.Config.Analytics.GeoIPPath != ""
	if !data.GeoAvailable {
		// Only when it is actually true. The ranked list's empty state used to be
		// this sentence unconditionally, which meant a link with a GeoIP database
		// and no country rows yet — a link nobody has clicked — told its owner the
		// instance had no database. Now the empty state falls back to the ordinary
		// "No data yet".
		data.GeoUnavailable = ui.GeoUnavailable
	}
	data.GeoBase = fmt.Sprintf("/links/%s?days=%d", l.ID, days)
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
		geoMetric, stats.Caveat, data.GeoAvailable)
	if data.GeoAvailable {
		data.Countries = stats.Dimensions["country"]
	}

	// The rings. Every dimension the page ranks gets one, including countries:
	// the map answers "where", the ring answers "how concentrated", and the list
	// answers "exactly how many".
	data.Donuts = make(map[string]ui.Donut, len(stats.Dimensions))
	for dim, values := range stats.Dimensions {
		data.Donuts[dim] = ui.DonutChart(dimensionSlices(values), stats.Totals.Clicks, 100)
	}

	data.ReturningAvailable = h.Config.Redis.URL != ""
	data.MinPasswordLength = auth.MinPasswordLength

	switch {
	case r.URL.Query().Get("rule") != "":
		data.Notice = ruleNotice(r.URL.Query().Get("rule"))
	case r.URL.Query().Get("split") != "":
		data.Notice = splitNotice(r.URL.Query().Get("split"))
	case r.URL.Query().Get("qr") != "":
		data.Notice = qrNotice(r.URL.Query().Get("qr"))
	case r.URL.Query().Get("created") == "1":
		data.Notice = "Link created: " + l.ShortURL
	case r.URL.Query().Get("saved") == "1":
		data.Notice = "Changes saved."
	case r.URL.Query().Get("archived") == "1":
		data.Notice = "Link archived. It no longer redirects; the alias stays reserved."
	case r.URL.Query().Get("restored") == "1":
		data.Notice = "Link restored."
	}
	return data, true
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
		ttl = time.Duration(hours) * time.Hour
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
		data.FieldErrors = fields
		data.Error = general
		h.render(w, r, http.StatusUnprocessableEntity, "link_detail", data)
		return
	}

	data, ok := h.loadLinkDetail(w, r)
	if !ok {
		return
	}
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
