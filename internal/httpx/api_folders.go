package httpx

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

// Folders over the API (M38).
//
// Thin, like every other handler here: authorization, the cycle rule, the depth
// cap and sibling-name uniqueness are all in internal/link, so the dashboard
// forms get identical behaviour by calling the same methods.
//
// **The tree comes back as one flat, ordered list rather than as nested
// objects.** Depth-first, parents before their descendants, with `depth` on each
// entry — which is what both consumers actually want: the dashboard renders one
// indented row per entry, and a client rebuilding the nesting has `parent_id`.
// Nested JSON would make the common case (draw the tree) require a recursive
// walk to do what a `for` loop does here, and would make the response shape
// depend on how deep somebody's folders happen to be.
//
// **Moving is its own operation, not a field on the update.** A move is the one
// folder operation that can make the tree stop being a tree, and `parent_id` is
// the one field whose null is a value — "move to the top level" — rather than
// "leave it alone". Both facts point the same way: POST /folders/{id}/move takes
// a required, explicitly nullable `parent_id`, and PATCH renames.

// folderFilterNone is what `?folder=` carries to mean "the links that are in no
// folder". A word rather than an empty value, because an empty query parameter
// is indistinguishable from a control that was never touched — and the two mean
// opposite things here.
//
// Shared by the API and the dashboard so the two surfaces spell the same filter
// the same way, which is what lets a link from one be pasted into the other.
const folderFilterNone = "none"

type createFolderRequest struct {
	Name string `json:"name"`
	// ParentID puts the new folder inside another. Absent or null is the top
	// level.
	ParentID *uuid.UUID `json:"parent_id"`
}

type updateFolderRequest struct {
	Name string `json:"name"`
}

type moveFolderRequest struct {
	// ParentID is where the folder goes. Explicitly nullable: null is the top
	// level, and is the only way to get a folder back out of another one.
	ParentID *uuid.UUID `json:"parent_id"`
}

func (a *LinkAPI) ListFolders(w http.ResponseWriter, r *http.Request) {
	tree, err := a.Links.Folders(r.Context(), IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The cap travels with the answer, the way GetSplit advertises the kinds it
	// accepts: a client building a folder picker learns how deep it may offer to
	// go from the response rather than from this file.
	WriteJSON(w, http.StatusOK, map[string]any{
		"folders":   tree.Flat(),
		"max_depth": domain.MaxFolderDepth,
	})
}

func (a *LinkAPI) CreateFolder(w http.ResponseWriter, r *http.Request) {
	var req createFolderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	folder, err := a.Links.CreateFolder(r.Context(), IdentityFrom(r.Context()),
		link.CreateFolderInput{Name: req.Name, ParentID: req.ParentID})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, folder)
}

func (a *LinkAPI) UpdateFolder(w http.ResponseWriter, r *http.Request) {
	folderID, err := pathUUID(r, "folderID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateFolderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	folder, err := a.Links.RenameFolder(r.Context(), IdentityFrom(r.Context()), folderID, req.Name)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, folder)
}

func (a *LinkAPI) MoveFolder(w http.ResponseWriter, r *http.Request) {
	folderID, err := pathUUID(r, "folderID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req moveFolderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	folder, err := a.Links.MoveFolder(r.Context(), IdentityFrom(r.Context()), folderID, req.ParentID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, folder)
}

func (a *LinkAPI) DeleteFolder(w http.ResponseWriter, r *http.Request) {
	folderID, err := pathUUID(r, "folderID")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Links.DeleteFolder(r.Context(), IdentityFrom(r.Context()), folderID); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
