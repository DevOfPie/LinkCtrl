package httpx

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// maxBodyBytes caps request bodies. Without it a client can stream an
// unbounded body and make the server buffer it.
const maxBodyBytes = 256 * 1024

// LinkAPI serves /api/v1/links and /api/v1/tags.
//
// Handlers stay thin on purpose: they parse, call the service, and map the
// result. Every authorization and validation decision lives in the service, so
// the dashboard handlers added in M11 get identical behaviour by calling the
// same methods rather than by remembering to repeat the checks.
type LinkAPI struct {
	Links *link.Service
}

type createLinkRequest struct {
	URL          string   `json:"url"`
	Alias        string   `json:"alias"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Tags         []string `json:"tags"`
	ExpiresAt    *string  `json:"expires_at"`
	ForwardQuery bool     `json:"forward_query"`
	ForwardPath  bool     `json:"forward_path"`
	// FolderID files the new link (M38). Absent or null leaves it unfiled.
	FolderID *uuid.UUID `json:"folder_id"`
	// CampaignID labels the new link (M41). Absent or null leaves it unlabelled.
	// A folder and a campaign are different questions — where the link lives and
	// what it is for — so a link may carry both, one or neither.
	CampaignID *uuid.UUID `json:"campaign_id"`
	// DomainID names the hostname to serve the link on (M40). Absent or null
	// takes the workspace's own default. It must be verified: a link on an
	// unverified hostname would be a short URL that resolves nowhere.
	DomainID *uuid.UUID `json:"domain_id"`

	// The gates (M35). Write-only in the case of Password: it is hashed on
	// arrival and no response anywhere returns it or its hash.
	Password         string `json:"password"`
	MaxClicks        *int64 `json:"max_clicks"`
	OneTime          bool   `json:"one_time"`
	RequireSignature bool   `json:"require_signature"`
}

func (a *LinkAPI) Create(w http.ResponseWriter, r *http.Request) {
	var req createLinkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	in := link.CreateInput{
		URL: req.URL, Alias: req.Alias, Title: req.Title,
		Description: req.Description, Tags: req.Tags,
		ForwardQuery: req.ForwardQuery, ForwardPath: req.ForwardPath,
		Password: req.Password, MaxClicks: req.MaxClicks, OneTime: req.OneTime,
		RequireSignature: req.RequireSignature, FolderID: req.FolderID,
		CampaignID: req.CampaignID,
		DomainID:   req.DomainID,
	}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		at, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			WriteError(w, r, domain.ValidationErrors{{
				Field: "expires_at", Code: "invalid",
				Message: "expiry must be an RFC 3339 timestamp",
			}})
			return
		}
		in.ExpiresAt = &at
	}

	created, err := a.Links.Create(r.Context(), IdentityFrom(r.Context()), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w.Header().Set("Location", created.ShortURL)
	WriteJSON(w, http.StatusCreated, created)
}

func (a *LinkAPI) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	f := domain.LinkFilter{
		Search:       strings.TrimSpace(q.Get("search")),
		Cursor:       q.Get("cursor"),
		Sort:         domain.LinkSort(q.Get("sort")),
		IncludeTotal: q.Get("include_total") == "true",
	}
	if s := q.Get("status"); s != "" {
		f.Status = domain.LinkStatus(s)
	}
	if l := q.Get("limit"); l != "" {
		// Range-checked before narrowing. ?limit=2147483648 would otherwise
		// wrap to a negative int32 and reach the query as a negative LIMIT;
		// out-of-range values are dropped so the service default applies.
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= math.MaxInt32 {
			f.Limit = int32(n) //nolint:gosec // G109: range-checked on the line above
		}
	}
	for _, raw := range q["tag"] {
		if id, err := uuid.Parse(raw); err == nil {
			f.TagIDs = append(f.TagIDs, id)
		}
	}
	// The folder filter (M38). `?folder=none` is the links that are in no
	// folder, which is a question a uuid cannot ask; anything unparseable is
	// dropped so a stale bookmark shows the whole list rather than nothing.
	if raw := q.Get("folder"); raw == folderFilterNone {
		f.Unfiled = true
	} else if id, err := uuid.Parse(raw); err == nil {
		f.FolderID = &id
	}
	// The campaign filter (M41), spelled exactly as the folder filter above is
	// and dropping just as quietly when its id names nothing.
	if raw := q.Get("campaign"); raw == campaignFilterNone {
		f.Uncampaigned = true
	} else if id, err := uuid.Parse(raw); err == nil {
		f.CampaignID = &id
	}
	// The domain filter (M40), which the links list gained alongside custom
	// domains: once a workspace serves links on more than one hostname, "which
	// links are on go.acme.com" is a question the list has to be able to answer.
	// An unparseable id drops the filter for the reason the folder one does.
	if id, err := uuid.Parse(q.Get("domain")); err == nil {
		f.DomainID = &id
	}

	// A "tag:foo" prefix in the search box is a convenience that has to be
	// stripped: passing it through to full-text search matches nothing and
	// looks like the filter silently failed.
	f.Search = stripTagPrefix(f.Search)

	page, err := a.Links.List(r.Context(), IdentityFrom(r.Context()), f)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, page)
}

func (a *LinkAPI) Get(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.Links.Get(r.Context(), IdentityFrom(r.Context()), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, l)
}

type updateLinkRequest struct {
	URL          *string   `json:"url"`
	Alias        *string   `json:"alias"`
	Title        *string   `json:"title"`
	Description  *string   `json:"description"`
	ExpiresAt    *string   `json:"expires_at"`
	Tags         *[]string `json:"tags"`
	ForwardQuery *bool     `json:"forward_query"`
	ForwardPath  *bool     `json:"forward_path"`
	// BotBlocking is "inherit", "on" or "off". A pointer, like every other
	// field here, so omitting it means "leave it alone" rather than "reset it to
	// the default" — which for this setting would silently hand the decision
	// back to the domain.
	BotBlocking *string `json:"bot_blocking"`
	// FolderID is where the link is filed (M38), and it spells its third state
	// the way `expires_at` below does rather than inventing a second idiom on
	// one type: absent leaves the link where it is, an empty string takes it out
	// of every folder, and an id files it there. The empty string is
	// unambiguous — no folder has an empty id — and without it a filed link
	// could never be unfiled.
	FolderID *string `json:"folder_id"`
	// CampaignID is which campaign the link carries (M41), spelling its three
	// states exactly as FolderID above does.
	CampaignID *string `json:"campaign_id"`

	// The gates (M35). Two of them need three states, and both spell the third
	// the way `expires_at` already does on this same type: an absent key leaves
	// the gate alone, a sentinel removes it, a value sets it. An empty string
	// removes a password; a zero removes a click ceiling, which is unambiguous
	// because a ceiling of zero is refused everywhere else.
	//
	// The third state is not optional. A form that posts every field cannot
	// re-send a password nobody can read, so an empty box has to mean "no
	// change" — and without an explicit removal there would then be no way to
	// take a password off at all.
	//
	// There is deliberately no way to *read* a password back. Setting one is
	// write-only in every direction: a management API that returned the value,
	// or its hash, would make every reader of a link a holder of its secret.
	Password         *string `json:"password"`
	MaxClicks        *int64  `json:"max_clicks"`
	OneTime          *bool   `json:"one_time"`
	RequireSignature *bool   `json:"require_signature"`
}

func (a *LinkAPI) Update(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var req updateLinkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	in := link.UpdateInput{
		URL: req.URL, Alias: req.Alias, Title: req.Title,
		Description: req.Description, Tags: req.Tags,
		ForwardQuery: req.ForwardQuery, ForwardPath: req.ForwardPath,
		OneTime: req.OneTime, RequireSignature: req.RequireSignature,
	}
	if req.Password != nil && *req.Password == "" {
		in.ClearPassword = true
	} else {
		in.Password = req.Password
	}
	if req.MaxClicks != nil && *req.MaxClicks == 0 {
		in.ClearMaxClicks = true
	} else {
		in.MaxClicks = req.MaxClicks
	}
	if req.FolderID != nil {
		if *req.FolderID == "" {
			in.ClearFolder = true
		} else {
			folderID, perr := uuid.Parse(*req.FolderID)
			if perr != nil {
				WriteError(w, r, domain.ValidationErrors{{
					Field: "folder_id", Code: "invalid",
					Message: "folder_id must be a folder's id, or empty to take the " +
						"link out of every folder",
				}})
				return
			}
			in.FolderID = &folderID
		}
	}
	if req.CampaignID != nil {
		if *req.CampaignID == "" {
			in.ClearCampaign = true
		} else {
			campaignID, perr := uuid.Parse(*req.CampaignID)
			if perr != nil {
				WriteError(w, r, domain.ValidationErrors{{
					Field: "campaign_id", Code: "invalid",
					Message: "campaign_id must be a campaign's id, or empty to take the " +
						"link out of its campaign",
				}})
				return
			}
			in.CampaignID = &campaignID
		}
	}
	if req.BotBlocking != nil {
		policy, ok := domain.ParseBotPolicy(*req.BotBlocking)
		if !ok {
			WriteError(w, r, domain.ValidationErrors{{
				Field: "bot_blocking", Code: "invalid",
				Message: `bot_blocking must be "inherit", "on" or "off"`,
			}})
			return
		}
		in.BotBlocking = &policy
	}
	if req.ExpiresAt != nil {
		// An explicit null clears the expiry; an absent field leaves it alone.
		// Without the distinction there is no way to remove an expiry.
		if *req.ExpiresAt == "" {
			in.ClearExpiry = true
		} else {
			at, err := time.Parse(time.RFC3339, *req.ExpiresAt)
			if err != nil {
				WriteError(w, r, domain.ValidationErrors{{
					Field: "expires_at", Code: "invalid",
					Message: "expiry must be an RFC 3339 timestamp, or empty to clear it",
				}})
				return
			}
			in.ExpiresAt = &at
		}
	}

	updated, err := a.Links.Update(r.Context(), IdentityFrom(r.Context()), id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, updated)
}

// Sign mints a signed URL for a link (M35).
//
// A POST rather than a GET, and the method is the honest one: this call can
// bring the workspace's signing secret into existence, and it hands back a
// bearer capability that should not be sitting in anybody's browser history or
// proxy log as a URL that was fetched.
//
// The format the URL carries is documented in internal/gate/signature.go and in
// docs/SECURITY.md, so a client can verify one without this endpoint.
func (a *LinkAPI) Sign(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ttl, err := parseSignTTL(w, r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	signed, err := a.Links.Sign(r.Context(), IdentityFrom(r.Context()), id, ttl)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, signed)
}

func (a *LinkAPI) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.Delete(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *LinkAPI) Archive(w http.ResponseWriter, r *http.Request) {
	a.archiveOrRestore(w, r, true)
}

func (a *LinkAPI) Restore(w http.ResponseWriter, r *http.Request) {
	a.archiveOrRestore(w, r, false)
}

func (a *LinkAPI) archiveOrRestore(w http.ResponseWriter, r *http.Request, archive bool) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var l *domain.Link
	if archive {
		l, err = a.Links.Archive(r.Context(), IdentityFrom(r.Context()), id)
	} else {
		l, err = a.Links.Restore(r.Context(), IdentityFrom(r.Context()), id)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, l)
}

func (a *LinkAPI) ListTags(w http.ResponseWriter, r *http.Request) {
	tags, err := a.Links.ListTags(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": tags})
}

func (a *LinkAPI) DeleteTag(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.DeleteTag(r.Context(), IdentityFrom(r.Context()), id); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Me returns the current identity, including its permissions so a client can
// render only the actions the user can actually perform.
func (a *LinkAPI) Me(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	WriteJSON(w, http.StatusOK, map[string]any{
		"user_id":      id.UserID,
		"email":        id.Email,
		"name":         id.Name,
		"workspace_id": id.WorkspaceID,
		"role":         id.Role,
		"permissions":  id.Permissions(),
	})
}

// --- helpers ----------------------------------------------------------------

// parseSignTTL reads the optional ttl_seconds a signing request may carry.
//
// Zero with a nil error means "the caller named no lifetime": the service
// substitutes DefaultSignatureTTL, and that substitution belongs there, next to
// the ceiling it pairs with.
func parseSignTTL(w http.ResponseWriter, r *http.Request) (time.Duration, error) {
	var req struct {
		// TTLSeconds is how long the signature stays valid. Absent takes the
		// default; anything above the ceiling is refused rather than clamped, so
		// a caller asking for a year is told they did not get one.
		TTLSeconds *int64 `json:"ttl_seconds"`
	}
	// An empty body is the ordinary case here — "sign this, I do not care for
	// how long" — so it is not the validation error decodeJSON makes of it
	// everywhere else. The gate is `!= 0` and not `> 0` because a chunked
	// request carries ContentLength == -1 while still carrying a body: `> 0`
	// would throw that body away and sign with the default, telling the caller
	// their ttl was honoured when it was never read. The cost is that a chunked
	// request with a genuinely empty body now draws decodeJSON's empty-body
	// refusal instead of the default — the same trade Rotate already makes.
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			return 0, err
		}
	}
	if req.TTLSeconds == nil {
		return 0, nil
	}
	if *req.TTLSeconds <= 0 {
		return 0, domain.ValidationErrors{{
			Field: "ttl_seconds", Code: "out_of_range",
			Message: "ttl_seconds must be positive",
		}}
	}
	return saturateDuration(*req.TTLSeconds, time.Second, link.MaxSignatureTTL), nil
}

// saturateDuration converts a caller-supplied count of units into a
// time.Duration without letting the multiplication wrap.
//
// A time.Duration is an int64 of nanoseconds, so `count * unit` wraps mod 2^64
// once the product passes ~292 years, and a wrapped product can land anywhere:
// a few hundred milliseconds, a negative number, exactly zero. Every one of
// those slips past a range check that runs after the conversion — a negative or
// zero product reads as "no value named" and silently takes the default, and a
// small positive one can sit inside the accepted band, so a caller asking for
// 2^55 seconds would be granted something without ever being told no.
//
// The guard therefore runs on the count, before any multiplication. A count
// whose product would land past the ceiling — including every count that would
// wrap — is pinned to one unit beyond the ceiling, on whichever side of zero
// the count pointed. Deliberately not an error: the service that owns the range
// owns its message too, and a pinned value draws that service's canonical
// out_of_range refusal rather than a second copy of it maintained here.
func saturateDuration(count int64, unit, ceiling time.Duration) time.Duration {
	limit := int64(ceiling / unit)
	switch {
	case count > limit:
		return ceiling + unit
	case count < -limit:
		return -ceiling - unit
	}
	return time.Duration(count) * unit
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	// Unknown fields are an error rather than ignored: silently dropping a
	// misspelled field means the caller believes they set something they did
	// not, which is worse than a clear rejection.
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case err == io.EOF:
			return domain.ValidationErrors{{
				Field: "body", Code: "empty", Message: "a JSON body is required",
			}}
		case AsError(err, &maxErr):
			return domain.ValidationErrors{{
				Field: "body", Code: "too_large", Message: "request body is too large",
			}}
		default:
			return domain.ValidationErrors{{
				Field: "body", Code: "invalid", Message: "body is not valid JSON: " + err.Error(),
			}}
		}
	}
	return nil
}

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, domain.ErrNotFound
	}
	return id, nil
}

// stripTagPrefix removes a leading "tag:foo" token from a search string.
func stripTagPrefix(s string) string {
	fields := strings.Fields(s)
	kept := fields[:0]
	for _, f := range fields {
		if strings.HasPrefix(strings.ToLower(f), "tag:") {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
}

// GetDomain reports the link domain's settings.
func (a *LinkAPI) GetDomain(w http.ResponseWriter, r *http.Request) {
	settings, err := a.Links.DomainSettings(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, settings)
}

// UpdateDomain changes the link domain's settings.
//
// PATCH of whichever fields are present rather than PUT of the whole object.
// Every field is a pointer for the same reason: an absent key means "change
// nothing", and treating it as a value would let a client clear the root
// redirect or hand every link's bot decision back to itself by sending `{}`.
//
// The two settings go through separate service calls because they are guarded
// by the same permission and by nothing else in common — the root redirect is
// refused outright on a single-host deployment, bot blocking never is. Applying
// them in one call would mean failing a valid bot change because of the
// deployment shape.
//
// A body carrying both therefore applies **in order, stopping at the first
// refusal**, and the order is root redirect then bot blocking. That is stated
// rather than hidden because it is observable: a request whose URL is rejected
// changes nothing, while one whose URL is accepted and whose bot pair is not
// leaves the URL changed. The alternative is a transaction across two service
// methods with different guards, for a combination neither dashboard form ever
// sends — both post one setting at a time.
func (a *LinkAPI) UpdateDomain(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RootRedirectURL   *string `json:"root_redirect_url"`
		BlockBots         *bool   `json:"block_bots"`
		BlockBotsEnforced *bool   `json:"block_bots_enforced"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.RootRedirectURL == nil && req.BlockBots == nil && req.BlockBotsEnforced == nil {
		WriteError(w, r, domain.ValidationErrors{{
			Field: "root_redirect_url", Code: "required",
			Message: "send at least one setting: root_redirect_url, block_bots or block_bots_enforced",
		}})
		return
	}

	actor := IdentityFrom(r.Context())
	var settings *link.DomainSettings

	if req.RootRedirectURL != nil {
		updated, err := a.Links.SetRootRedirect(r.Context(), actor, *req.RootRedirectURL)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		settings = updated
	}

	if req.BlockBots != nil || req.BlockBotsEnforced != nil {
		// Read the current pair first, because either flag may be sent alone and
		// the two are written together — 01800's CHECK refuses the combination a
		// two-step write would pass through on the way.
		current, err := a.Links.DomainSettings(r.Context(), actor)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		block, enforced := current.BlockBots, current.BlockBotsEnforced
		if req.BlockBots != nil {
			block = *req.BlockBots
		}
		if req.BlockBotsEnforced != nil {
			enforced = *req.BlockBotsEnforced
		}
		if settings, err = a.Links.SetBotBlocking(r.Context(), actor, block, enforced); err != nil {
			WriteError(w, r, err)
			return
		}
	}

	WriteJSON(w, http.StatusOK, settings)
}
