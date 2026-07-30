package httpx

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
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
}

type linksPageData struct {
	shell
	Links       []domain.Link
	HasMore     bool
	NextURL     string
	Total       *int64
	Search      string
	Status      string
	Sort        string
	Filtered    bool
	Form        linkFormData
	FieldErrors map[string]string
	Notice      string
	Error       string
}

func (h *Web) loadLinksPage(w http.ResponseWriter, r *http.Request) (linksPageData, bool) {
	actor := IdentityFrom(r.Context())
	q := r.URL.Query()

	data := linksPageData{
		shell:       h.shell(r, "Links", "links"),
		Search:      strings.TrimSpace(q.Get("search")),
		Status:      q.Get("status"),
		Sort:        q.Get("sort"),
		FieldErrors: map[string]string{},
	}
	if data.Sort == "" {
		data.Sort = "newest"
	}
	if q.Get("deleted") == "1" {
		data.Notice = "Link deleted. It stays restorable for 30 days."
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

	page, err := h.Links.List(r.Context(), actor, f)
	if err != nil {
		h.webError(w, r, err)
		return data, false
	}

	data.Links = page.Items
	data.HasMore = page.HasMore
	data.Total = page.Total
	data.Filtered = data.Search != "" || data.Status != "" || f.Cursor != ""
	if page.HasMore {
		next := url.Values{}
		if data.Search != "" {
			next.Set("search", data.Search)
		}
		if data.Status != "" {
			next.Set("status", data.Status)
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
		URL:          l.URL,
		Alias:        l.Alias,
		Title:        l.Title,
		Description:  l.Description,
		ForwardQuery: l.ForwardQuery,
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
	switch {
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

	in := link.UpdateInput{
		URL: &urlv, Alias: &alias, Title: &title, Description: &desc, Tags: &tags,
		ForwardQuery: &forward,
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
