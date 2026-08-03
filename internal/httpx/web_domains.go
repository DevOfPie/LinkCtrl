package httpx

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// The domains page (M39).
//
// **It registers a hostname and it says, on the row, that nothing is served on
// it.** That sentence is the page's whole job at this milestone. A dashboard
// that let somebody add `go.example.com` and then looked exactly like a
// dashboard where the hostname works would produce one support question per
// registration; the row carries "Not verified — links are not served here yet"
// until M40 gives it something better to say.
//
// Ordinary forms, no JavaScript. Registering, renaming and removing are each a
// POST to the URL its form names, htmx swaps the panel when it is available, and
// the browser follows a 303 when it is not — the same shape as the folders page,
// for the same reason.
//
// Every control the page draws is one the service would accept: the row offers
// Rename and Remove only where `Manageable` is true, which is the service's own
// answer rather than a re-derivation of it. Rendering hint, never
// authorization — a hand-made POST is re-judged on arrival.

type domainRow struct {
	ID       string
	Hostname string
	// ScopeLabel is who owns it, in a word a reader recognizes.
	ScopeLabel string
	IsDefault  bool
	Verified   bool
	LinkCount  int64
	Manageable bool
}

type domainsPageData struct {
	shell
	Rows []domainRow
	// CanRegister gates the form. domains.write, which is the owner and admin
	// roles; an editor reads the list and is offered nothing.
	CanRegister  bool
	FormHostname string

	Notice string
	Error  string
}

func (h *Web) loadDomainsPage(w http.ResponseWriter, r *http.Request) (domainsPageData, bool) {
	actor := IdentityFrom(r.Context())

	domains, err := h.Links.Domains(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return domainsPageData{}, false
	}

	data := domainsPageData{
		shell:        h.shell(r, "Domains", "domains"),
		CanRegister:  actor.Can(link.PermDomainsWrite),
		FormHostname: r.URL.Query().Get("hostname"),
	}
	data.Notice = domainNotice(r.URL.Query().Get("domain"))

	data.Rows = make([]domainRow, 0, len(domains))
	for _, d := range domains {
		data.Rows = append(data.Rows, domainRow{
			ID: d.ID.String(), Hostname: d.Hostname,
			ScopeLabel: domainScopeLabel(d.Scope),
			IsDefault:  d.IsDefault,
			Verified:   d.Verified,
			LinkCount:  d.LinkCount,
			Manageable: d.Manageable,
		})
	}
	return data, true
}

// domainScopeLabel turns D68's three ownership states into words a reader of the
// page recognizes, rather than printing the column that holds them.
func domainScopeLabel(scope link.DomainScope) string {
	switch scope {
	case link.ScopeWorkspace:
		return "This workspace"
	case link.ScopeOrganization:
		return "This organization"
	default:
		return "This instance"
	}
}

func (h *Web) DomainsPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadDomainsPage(w, r)
	if !ok {
		return
	}
	h.renderDomains(w, r, http.StatusOK, data)
}

func (h *Web) renderDomains(w http.ResponseWriter, r *http.Request, status int, data domainsPageData) {
	if isHTMX(r) {
		h.renderPartial(w, r, "domains", "domain_panel", data)
		return
	}
	h.render(w, r, status, "domains", data)
}

func (h *Web) DomainCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	_, err := h.Links.RegisterDomain(r.Context(), IdentityFrom(r.Context()),
		r.PostFormValue("hostname"))
	h.finishDomainAction(w, r, "registered", err)
}

func (h *Web) DomainRename(w http.ResponseWriter, r *http.Request) {
	h.domainAction(w, r, "renamed", func(ctx context.Context, id uuid.UUID) error {
		if err := parseForm(w, r); err != nil {
			return err
		}
		_, err := h.Links.RenameDomain(ctx, IdentityFrom(ctx), id, r.PostFormValue("hostname"))
		return err
	})
}

func (h *Web) DomainDelete(w http.ResponseWriter, r *http.Request) {
	h.domainAction(w, r, "removed", func(ctx context.Context, id uuid.UUID) error {
		return h.Links.DeleteDomain(ctx, IdentityFrom(ctx), id)
	})
}

func (h *Web) domainAction(
	w http.ResponseWriter, r *http.Request, marker string,
	do func(ctx context.Context, id uuid.UUID) error,
) {
	id, err := pathUUID(r, "domainID")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	h.finishDomainAction(w, r, marker, do(r.Context(), id))
}

// finishDomainAction is the one place a domain write turns into a response.
//
// A validation refusal comes back on the page it was made from — every one of
// them is something the reader can fix in the box they are looking at. A 403 is
// not: being told a hostname belongs to another workspace is not a form error,
// and webError renders it as the refusal it is.
func (h *Web) finishDomainAction(w http.ResponseWriter, r *http.Request, marker string, err error) {
	if err != nil {
		var ve domain.ValidationErrors
		if !errors.As(err, &ve) {
			h.webError(w, r, err)
			return
		}
		data, ok := h.loadDomainsPage(w, r)
		if !ok {
			return
		}
		data.Error = ve[0].Message
		data.Notice = ""
		data.FormHostname = r.PostFormValue("hostname")
		h.renderDomains(w, r, http.StatusUnprocessableEntity, data)
		return
	}
	if isHTMX(r) {
		data, ok := h.loadDomainsPageAfter(w, r, marker)
		if !ok {
			return
		}
		h.renderPartial(w, r, "domains", "domain_panel", data)
		return
	}
	seeOther(w, r, "/domains?domain="+marker)
}

// loadDomainsPageAfter re-reads the page for an htmx response, with the notice
// the redirect would have carried in its query string.
func (h *Web) loadDomainsPageAfter(w http.ResponseWriter, r *http.Request, marker string) (domainsPageData, bool) {
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = ""
	data, ok := h.loadDomainsPage(w, r2)
	if !ok {
		return data, false
	}
	data.Notice = domainNotice(marker)
	return data, true
}

// domainNotice turns the ?domain= marker into a sentence.
//
// Each one says what did *not* happen, because that is the surprise. Registering
// a hostname is the moment somebody expects their links to move to it, and this
// is where they are told they have not.
func domainNotice(marker string) string {
	switch marker {
	case "registered":
		return "Hostname registered to this workspace. Nothing is served on it yet — " +
			"verifying a domain and serving links on it arrives in a later release."
	case "renamed":
		return "Hostname changed. Nothing was served on the old name, so no link changed."
	case "removed":
		return "Hostname removed from this workspace. It is free for anybody on this " +
			"instance to register again."
	default:
		return ""
	}
}
