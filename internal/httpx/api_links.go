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

	// Phase 2. Present so the service can reject them by name.
	Password  string `json:"password"`
	MaxClicks *int64 `json:"max_clicks"`
	OneTime   bool   `json:"one_time"`
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
		ForwardQuery: req.ForwardQuery,
		Password:     req.Password, MaxClicks: req.MaxClicks, OneTime: req.OneTime,
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
		ForwardQuery: req.ForwardQuery,
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
