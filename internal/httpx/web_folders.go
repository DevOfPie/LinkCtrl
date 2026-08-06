package httpx

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// The folder tree page (M38).
//
// **Click-to-move, and no drag-and-drop.** Moving a folder is two clicks: "Move"
// on the folder, which puts the page into move mode, then "Move here" on the
// destination. Both are ordinary hypertext — a link and a form — so the whole
// feature works with scripting switched off, is reachable from a keyboard, and
// is announced by a screen reader as a button that says where it goes. A drag
// target is none of those things, and building one would need custom JavaScript
// that `ui` does not have and the CSP does not allow.
//
// htmx is the enhancement rather than the mechanism. Entering and leaving move
// mode is a GET whose `hx-get` swaps the panel in place, and every write posts
// to the same URL its form action names; with htmx present the handler answers
// the panel, without it the browser follows a 303 and re-renders the page. The
// difference between the two is a full page paint, which is exactly what
// progressive enhancement is supposed to be worth.
//
// **Every destination offered is one the service would accept.** The page asks
// domain.FolderTree.MoveRefusal — the same call MoveFolder makes — rather than
// re-deriving the rules, so a row that offers "Move here" cannot be a row that
// then refuses. Rendering hint, never authorization: the service re-judges the
// move on arrival, exactly as it would for a hand-made POST.

// folderNode is one row of the rendered tree, with its children nested so the
// template can recurse and the indentation can be a nested list rather than
// eight hand-written padding classes.
type folderNode struct {
	ID        string
	Name      string
	LinkCount int64
	Children  []folderNode

	// Moving marks the folder the page is currently moving, so the row can say
	// so instead of offering to move it into itself.
	Moving bool
	// CanReceive is whether this row offers "Move here". False both for
	// destinations the service would refuse and for the parent the folder is
	// already in, where the button would change nothing.
	CanReceive bool
}

// folderOption is one entry of a folder `<select>`.
type folderOption struct {
	ID    string
	Label string
	// Selected is set by the caller that knows what the current value is.
	Selected bool
}

type foldersPageData struct {
	shell
	Nodes []folderNode
	Count int
	// UnfiledURL is the links list filtered to the links in no folder. On this
	// page rather than only on the list, because "where did the rest go" is the
	// question a tree with a small link count raises.
	UnfiledURL string
	MaxDepth   int
	MaxFolders int

	// ParentOptions is what the create form offers. Folders already at the depth
	// cap are absent, because a child of one cannot exist.
	ParentOptions []folderOption
	FormName      string
	FormParent    string
	FieldErrors   map[string]string

	// Move mode. MovingID is empty when the page is not moving anything.
	MovingID      string
	MovingName    string
	CanMoveToRoot bool

	CanCreate bool
	CanUpdate bool
	CanDelete bool

	Notice string
	Error  string
}

func (h *Web) loadFoldersPage(w http.ResponseWriter, r *http.Request) (foldersPageData, bool) {
	actor := IdentityFrom(r.Context())

	tree, err := h.Links.Folders(r.Context(), actor)
	if err != nil {
		h.webError(w, r, err)
		return foldersPageData{}, false
	}

	data := foldersPageData{
		shell:       h.shell(r, "Folders", "links"),
		Count:       tree.Len(),
		UnfiledURL:  "/links?folder=" + folderFilterNone,
		MaxDepth:    domain.MaxFolderDepth,
		MaxFolders:  domain.MaxFoldersPerWorkspace,
		FormName:    r.URL.Query().Get("name"),
		FormParent:  r.URL.Query().Get("parent_id"),
		FieldErrors: map[string]string{},
		CanCreate:   actor.Can(link.PermCreate),
		CanUpdate:   actor.Can(link.PermUpdate),
		CanDelete:   actor.Can(link.PermDelete),
	}
	data.Notice = folderNotice(r.URL.Query().Get("folder"))

	// Move mode. An id that names nothing — a stale bookmark, a folder somebody
	// else deleted — drops the page back to its ordinary state rather than
	// erroring: there is nothing wrong with the request, the thing it was about
	// is simply gone.
	var moving *domain.Folder
	if raw := r.URL.Query().Get("move"); raw != "" && data.CanUpdate {
		if id, perr := uuid.Parse(raw); perr == nil {
			if f, ok := tree.Get(id); ok {
				moving = &f
				data.MovingID, data.MovingName = f.ID.String(), f.Name
				data.CanMoveToRoot = f.ParentID != nil &&
					tree.MoveRefusal(f.ID, nil) == nil
			}
		}
	}

	data.Nodes = folderNodes(tree, nil, moving)
	data.ParentOptions = folderOptions(tree, domain.MaxFolderDepth-1)
	for i := range data.ParentOptions {
		data.ParentOptions[i].Selected = data.ParentOptions[i].ID == data.FormParent
	}
	return data, true
}

// folderNodes turns the assembled tree into nested rows, deciding per row what
// the move controls should say.
func folderNodes(tree domain.FolderTree, parent *uuid.UUID, moving *domain.Folder) []folderNode {
	var out []folderNode
	for _, f := range tree.Flat() {
		if !domain.SameParent(f.ParentID, parent) {
			continue
		}
		node := folderNode{
			ID: f.ID.String(), Name: f.Name, LinkCount: f.LinkCount,
			Children: folderNodes(tree, &f.ID, moving),
		}
		if moving != nil {
			id := f.ID
			node.Moving = f.ID == moving.ID
			node.CanReceive = !node.Moving &&
				!domain.SameParent(moving.ParentID, &id) &&
				tree.MoveRefusal(moving.ID, &id) == nil
		}
		out = append(out, node)
	}
	return out
}

// folderOptions renders the tree as `<select>` options, deepest permitted level
// first excluded.
//
// The depth is spelled into the label with a run of figure dashes rather than
// with indentation, because an `<option>` cannot be styled and leading spaces
// are collapsed. maxDepth is the deepest folder that may appear: the create
// form passes MaxFolderDepth-1, since a child of a folder at the cap cannot
// exist and offering it would be offering a refusal.
func folderOptions(tree domain.FolderTree, maxDepth int) []folderOption {
	flat := tree.Flat()
	out := make([]folderOption, 0, len(flat))
	for _, f := range flat {
		if maxDepth > 0 && f.Depth > maxDepth {
			continue
		}
		out = append(out, folderOption{
			ID:    f.ID.String(),
			Label: strings.Repeat("‒ ", f.Depth-1) + f.Name,
		})
	}
	return out
}

func (h *Web) FoldersPage(w http.ResponseWriter, r *http.Request) {
	data, ok := h.loadFoldersPage(w, r)
	if !ok {
		return
	}
	h.renderFolders(w, r, http.StatusOK, data)
}

// renderFolders answers the whole page, or just the panel when htmx asked.
func (h *Web) renderFolders(w http.ResponseWriter, r *http.Request, status int, data foldersPageData) {
	if isHTMX(r) {
		h.renderPartial(w, r, "folders", "folder_panel", data)
		return
	}
	h.render(w, r, status, "folders", data)
}

func (h *Web) FolderCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	in := link.CreateFolderInput{Name: r.PostFormValue("name")}
	if raw := strings.TrimSpace(r.PostFormValue("parent_id")); raw != "" {
		id, perr := uuid.Parse(raw)
		if perr != nil {
			h.errorPage(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
			return
		}
		in.ParentID = &id
	}
	_, err := h.Links.CreateFolder(r.Context(), IdentityFrom(r.Context()), in)
	h.finishFolderAction(w, r, "created", err)
}

func (h *Web) FolderRename(w http.ResponseWriter, r *http.Request) {
	h.folderAction(w, r, "renamed", func(ctx context.Context, id uuid.UUID) error {
		if err := parseForm(w, r); err != nil {
			return err
		}
		_, err := h.Links.RenameFolder(ctx, IdentityFrom(ctx), id, r.PostFormValue("name"))
		return err
	})
}

// FolderMove is the second click of click-to-move. An empty `parent_id` is the
// top level, which is the one destination that is not a row on the page.
func (h *Web) FolderMove(w http.ResponseWriter, r *http.Request) {
	h.folderAction(w, r, "moved", func(ctx context.Context, id uuid.UUID) error {
		if err := parseForm(w, r); err != nil {
			return err
		}
		var parent *uuid.UUID
		if raw := strings.TrimSpace(r.PostFormValue("parent_id")); raw != "" {
			pid, perr := uuid.Parse(raw)
			if perr != nil {
				return domain.ValidationErrors{{
					Field: "parent_id", Code: "invalid",
					Message: "that is not a folder id",
				}}
			}
			parent = &pid
		}
		_, err := h.Links.MoveFolder(ctx, IdentityFrom(ctx), id, parent)
		return err
	})
}

func (h *Web) FolderDelete(w http.ResponseWriter, r *http.Request) {
	h.folderAction(w, r, "deleted", func(ctx context.Context, id uuid.UUID) error {
		return h.Links.DeleteFolder(ctx, IdentityFrom(ctx), id)
	})
}

func (h *Web) folderAction(
	w http.ResponseWriter, r *http.Request, marker string,
	do func(ctx context.Context, id uuid.UUID) error,
) {
	id, err := pathUUID(r, "folderID")
	if err != nil {
		h.webError(w, r, err)
		return
	}
	h.finishFolderAction(w, r, marker, do(r.Context(), id))
}

// finishFolderAction is the one place a folder write turns into a response.
//
// A refusal comes back on the page it was made from, with the reason above the
// tree — not on an error page, because every one of them is something the reader
// can fix in the form they are looking at. Anything that is not a validation
// error is a genuine failure and goes to webError.
func (h *Web) finishFolderAction(w http.ResponseWriter, r *http.Request, marker string, err error) {
	if err != nil {
		var ve domain.ValidationErrors
		if !errors.As(err, &ve) {
			h.webError(w, r, err)
			return
		}
		data, ok := h.loadFoldersPage(w, r)
		if !ok {
			return
		}
		data.Error = ve[0].Message
		data.Notice = ""
		data.FormName = r.PostFormValue("name")
		h.renderFolders(w, r, http.StatusUnprocessableEntity, data)
		return
	}
	// Leaving move mode is part of the success: the folder has moved, so the
	// page it moved from no longer describes anything.
	if isHTMX(r) {
		data, ok := h.loadFoldersPageAfter(w, r, marker)
		if !ok {
			return
		}
		h.renderPartial(w, r, "folders", "folder_panel", data)
		return
	}
	seeOther(w, r, "/folders?folder="+marker)
}

// loadFoldersPageAfter re-reads the page for an htmx response, with the notice
// the redirect would have carried in its query string.
func (h *Web) loadFoldersPageAfter(w http.ResponseWriter, r *http.Request, marker string) (foldersPageData, bool) {
	// The request is a POST to an action URL, so its own query string has
	// neither the move mode nor the marker. Reading the notice from `marker`
	// rather than from the URL is what keeps the two paths saying the same
	// sentence.
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = ""
	data, ok := h.loadFoldersPage(w, r2)
	if !ok {
		return data, false
	}
	data.Notice = folderNotice(marker)
	return data, true
}

// folderNotice turns the ?folder= marker into a sentence.
func folderNotice(marker string) string {
	switch marker {
	case "created":
		return "Folder created."
	case "renamed":
		return "Folder renamed. The links in it did not move."
	case "moved":
		return "Folder moved, with everything inside it."
	case "deleted":
		return "Folder deleted, along with the folders inside it. Every link that " +
			"was in any of them is still here — it is now filed in no folder."
	default:
		return ""
	}
}
