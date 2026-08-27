package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/addon"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// This file is M67's HTTP surface: install an add-on, remove an add-on, and
// nothing else.
//
// # Two operations, and the rest of the surface is next door
//
// The list, the detail read, the settings write and the purge are M68's manager
// and live in api_addon_manager.go, under this same scope. The split is the
// plan's: an upload surface and the page that drives it are two reviewable units,
// and this is the one whose risk is that a request body becomes code the server
// runs. A reviewer reading this file should be able to hold all of it at once.
//
// [AddonManager] is the interface the router actually holds, and it embeds the
// one below — one object implements both, and the two names say which half a
// handler is using.
//
// # Multipart, because one of the parts is a binary
//
// The same shape M50.5 taught the contract test, with two file parts instead of
// one: `manifest` is `addon.json` verbatim and `module` is the `.wasm`. A JSON
// body with base64 in it would have worked and would have cost a third more bytes
// on the wire for a body that is already megabytes, and — the reason that matters
// — it would have made the manifest a string inside a document rather than the
// file a publisher signed off, which is exactly the substitution
// internal/addon's staging deliberately avoids.
//
// **Since M68.6 there is a second shape, and it is two ordinary fields rather
// than two files**: `url` and `sha256`. The two shapes are mutually exclusive and
// the OpenAPI document expresses that as a `oneOf` over two closed objects rather
// than as a sentence, so a body carrying three of the four fields is refused by
// the schema as well as by [readAddonInstall].
//
// M67's file said there never would be one, and argued server-side request
// forgery. The argument was real and the stance was nobody's decision (D365): what
// answers it is internal/addon's address policy — resolution-time refusal of
// everything outside globally-routable unicast space, on every address a name
// resolves to and every hop of a redirect — plus a digest the *operator* supplies,
// never the URL. Neither of those existed when M67 was written. internal/addon/
// install_url.go is where the whole argument lives; this file only reads fields.
//
// # Authorization is the service's, as everywhere else
//
// `addons.manage`, checked inside internal/addon, with nothing here about which
// credential the caller used. The scope is in auth.NonDelegableScopes, so no API
// key can hold it and these handlers never have to ask — the same arrangement
// InstanceAPI has and for a harder reason, which internal/auth/apikey.go argues.

// AddonLifecycle is what this package needs from the add-on host to install and
// remove one. An interface for the reason [AddonRouter] is: the tests that assert
// what a caller sees do not construct a wasm runtime.
type AddonLifecycle interface {
	Install(ctx context.Context, actor *auth.Identity, req addon.InstallRequest) (addon.Installed, error)
	InstallFromURL(ctx context.Context, actor *auth.Identity, req addon.URLInstallRequest) (addon.Installed, error)
	Remove(ctx context.Context, actor *auth.Identity, name string) (addon.Installed, error)
}

// AddonAPI is the lifecycle surface, plus the manager reads and writes M68 added.
// One struct because one object implements both halves and a second would mean
// two places to wire the same host.
type AddonAPI struct {
	Addons AddonManager
}

// Install accepts a module and its manifest, verifies them, and starts the
// add-on without restarting the instance.
//
// `201`, because it created something an operator can now address by name — and
// because the body is the same summary a removal answers with, a client can
// compare what it installed against what it later removed without a second call.
func (a *AddonAPI) Install(w http.ResponseWriter, r *http.Request) {
	req, err := readAddonInstall(w, r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := req.install(r.Context(), a.Addons, IdentityFrom(r.Context()))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, out)
}

// Remove unloads an add-on and takes its files out of the add-ons directory.
//
// `200` with a body rather than `204`, and the body is why: removal leaves the
// add-on's schema behind as an orphan (M63), and the one moment at which somebody
// can decide what to do about it is the moment they removed it. Answering "no
// content" to an operation whose consequence is data left on disk would put the
// orphan in a place they have to go and look for.
func (a *AddonAPI) Remove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		WriteError(w, r, domain.ErrNotFound)
		return
	}
	out, err := a.Addons.Remove(r.Context(), IdentityFrom(r.Context()), name)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

// addonInstallFields are the four field names, here so that the refusals below,
// the dashboard's form and the OpenAPI document cannot drift apart by a spelling.
const (
	addonManifestField = "manifest"
	addonModuleField   = "module"
	addonURLField      = "url"
	addonDigestField   = "sha256"
)

// addonInstall is one install request in whichever of the two shapes arrived.
//
// A struct rather than two readers, because the shapes are mutually exclusive and
// *which one arrived* is a decision that has to be made once, in one place, with
// both sets of fields in view. Two readers would each have to decide what to do
// about the other's fields being present, which is the same decision made twice.
type addonInstall struct {
	upload  addon.InstallRequest
	fetch   addon.URLInstallRequest
	fromURL bool
}

// install hands the request to whichever service operation it is.
//
// On this side of the boundary rather than in the two handlers, so the dashboard
// and the API cannot come to disagree about which shape means which call — the
// property m67.md called *no private side door*, extended to the second shape.
func (i addonInstall) install(
	ctx context.Context, svc AddonLifecycle, actor *auth.Identity,
) (addon.Installed, error) {
	if i.fromURL {
		return svc.InstallFromURL(ctx, actor, i.fetch)
	}
	return svc.Install(ctx, actor, i.upload)
}

// readAddonInstall reads one install request out of a multipart body, in either
// shape.
//
// Its own reader rather than [readUploadedFile]'s, and the difference is the
// bound: that one is written around `qr.MaxLogoUploadBytes`, one part, and an
// image decoder behind it. This carries up to two file parts, one of them
// megabytes, and the bound it enforces is [addon.MaxUploadBytes] over the whole
// body — over the *body*, so a caller cannot send a 30 MiB manifest and a 30 MiB
// module and have each part pass its own check.
//
// **One multipart body for both shapes**, and the URL shape's two fields are
// ordinary form fields inside it rather than a second content type. That keeps one
// reader and one service call for the dashboard and the API alike, which is what
// makes the browser form and `curl` provably the same path rather than two paths
// somebody keeps in step.
//
// Nothing here reads a filename. The names of the files on disk are the manifest's
// business — `addon.json` is this host's constant and the module's name is a
// validated manifest field — so what a client called its local copy decides
// nothing, exactly as it decides nothing for a logo.
func readAddonInstall(w http.ResponseWriter, r *http.Request) (addonInstall, error) {
	var out addonInstall
	tooLarge := domain.ValidationErrors{{
		Field: addonModuleField, Code: "too_large",
		Message: "an add-on upload is at most " + byteSize(addon.MaxUploadBytes),
	}}
	notMultipart := domain.ValidationErrors{{
		Field: addonModuleField, Code: "invalid",
		Message: "this endpoint takes a multipart/form-data body carrying either a " +
			addonManifestField + " part and a " + addonModuleField + " part, or a " +
			addonURLField + " field and a " + addonDigestField + " field",
	}}

	r.Body = http.MaxBytesReader(w, r.Body, addon.MaxUploadBytes)
	parts, err := r.MultipartReader()
	if err != nil {
		return out, notMultipart
	}
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, addonReadErr(err, tooLarge, notMultipart)
		}
		name := part.FormName()
		body, err := io.ReadAll(io.LimitReader(part, addon.MaxUploadBytes))
		_ = part.Close()
		if err != nil {
			return out, addonReadErr(err, tooLarge, notMultipart)
		}
		switch name {
		case addonManifestField:
			out.upload.Manifest = body
		case addonModuleField:
			out.upload.Module = body
		case addonURLField:
			out.fetch.URL = strings.TrimSpace(string(body))
		case addonDigestField:
			out.fetch.SHA256 = strings.TrimSpace(string(body))
		default:
			// Refused rather than ignored. A part this endpoint does not know is a
			// caller who believes something will happen that will not — the same
			// reading the manifest parser applies to an unknown key, and the same
			// reason: an install is not a place to guess.
			return out, domain.ValidationErrors{{
				Field: name, Code: "unknown",
				Message: "an add-on install carries " + addonManifestField + " and " +
					addonModuleField + " parts, or " + addonURLField + " and " +
					addonDigestField + " fields, and nothing else",
			}}
		}
	}

	// **A field submitted empty is a field that was not filled in.** The dashboard
	// draws two forms and a browser sends only the one that was used, but an
	// unfilled file input still produces an empty part in some browsers, and a
	// caller building the body by hand may send four. Emptiness is what the reader
	// judges, so a blank field never makes a request ambiguous.
	uploaded := len(out.upload.Manifest) > 0 || len(out.upload.Module) > 0
	fetched := out.fetch.URL != "" || out.fetch.SHA256 != ""
	switch {
	case uploaded && fetched:
		return addonInstall{}, domain.ValidationErrors{{
			Field: addonURLField, Code: "exclusive",
			Message: "an install is an upload or a fetch and not both: send " +
				addonManifestField + " and " + addonModuleField + ", or " + addonURLField +
				" and " + addonDigestField,
		}}
	case fetched:
		out.fromURL = true
		out.upload = addon.InstallRequest{}
		if out.fetch.URL == "" {
			return addonInstall{}, domain.ValidationErrors{{
				Field: addonURLField, Code: "required",
				Message: "a digest was sent with no URL to fetch",
			}}
		}
	}
	// The remaining "required" refusals are the service's, not this reader's:
	// internal/addon answers them so that a caller gets the same message whichever
	// surface it came through, and so that this function has one job.
	return out, nil
}

// addonReadErr tells a body past the bound from a body that is not multipart.
func addonReadErr(err error, tooLarge, notMultipart error) error {
	var maxErr *http.MaxBytesError
	if AsError(err, &maxErr) {
		return tooLarge
	}
	return notMultipart
}

// byteSize renders a bound the way the documentation writes it, so the refusal a
// caller reads and the row in docs/configuration.md say the same number.
func byteSize(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return strconv.FormatInt(n/(1<<20), 10) + " MiB"
	case n >= 1<<10 && n%(1<<10) == 0:
		return strconv.FormatInt(n/(1<<10), 10) + " KiB"
	default:
		return strconv.FormatInt(n, 10) + " bytes"
	}
}
