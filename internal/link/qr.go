package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
	"github.com/DevOfPie/LinkCtrl/internal/store/pgerr"
)

// QR codes (M41).
//
// **A QR code is a picture of the link's own short URL**, so seeing one is
// seeing the link: `links.read` renders it, `links.update` styles it. No new
// permission, for the reason campaign.go gives in full and D75 records.
//
// **The code encodes `?src=qr`.** A camera sends no `Referer`, so without it
// every scan arrives indistinguishable from somebody typing the URL, and the
// milestone's "scans are attributed as ordinary clicks" would attribute nothing.
// The parameter travels in the picture, is resolved against a closed vocabulary
// on the redirect path, and lands in the existing referrer dimension — see
// domain/attribution.go.
//
// **The style is a preference, not content.** It is stored as jsonb in
// `qr_codes.style`, and a link with no row renders at the default style rather
// than not at all: the endpoint answers for every link in the workspace, which
// is what "a QR endpoint returns a code for any link" means. That is also why
// the row is created only when somebody styles the code — twenty links would
// otherwise carry twenty rows saying nothing.

// More than one code per link (M50).
//
// **A link has codes, and one of them is the default.** The default is what an
// *untagged* scan resolves through — a payload carrying `?src=qr` and no `qrc`,
// which is what every picture this product drew before M50 carries. The rest is
// per code: a generated slug that travels in the payload and a label that never
// leaves the dashboard.
//
// **The single-code operations stayed as they were and now mean the default
// code.** m50.md required this choice be made and recorded, and the alternative —
// growing `GET /links/{id}/qr` an identifier — would have changed what a shipped
// endpoint answers for every client already calling it, which is exactly what
// the contract test exists to catch. Recorded in decisions.md under M50.

// The default became a flag (M50's reopening, D183).
//
// **It was the row with the empty slug, and that identity is what made it
// unremovable** — the owner's report, F222: *"As long as there are multiple QR
// codes any of them should be able to be removed, currently the first one cannot
// be removed."* Deleting the code every already-printed picture resolved to
// would have left those pictures resolving to nothing, so the refusal was right
// and the identity was wrong. `qr_codes.is_default` carries it now, any code may
// hold it, and removing the holder promotes another rather than being refused.
//
// **Nothing already printed changes what it means, and here is the whole of
// why.** An untagged scan records the bare `qr` it has recorded since M41 —
// `clickSource` and `Snapshot.CodeSlug` are untouched by this reopening — and
// the breakdown attributes that bucket to whichever code holds the flag when
// somebody reads it. So a picture printed before M50 existed, one printed
// between M50 and this reopening, and one printed tomorrow off a code that holds
// the flag all land on the same row, and no scan already recorded was rewritten
// to make it so. The alternative was resolving the flag on the redirect path and
// storing `qr:<slug>` for an untagged scan, which splits every link's existing
// history at the migration — the split D130 spent a milestone avoiding.
//
// **A code gains a slug when it stops being alone**, which happens at exactly
// two moments: 04400, for the links already carrying more than one code, and
// CreateQRCode, which names the default before it adds the second code beside
// it. From there every code of that link has one and every one of them is
// removable, addressable and tellable apart, which is the whole of what the
// reopening asked for.
//
// **A link's only code keeps the payload it has**, and that is not a shortcut.
// M41's claim is that *restyling a code never changes what it says*, which a
// style write that also handed out an identity would falsify — a preference
// about colours silently rewriting a printed payload is the shape this
// reopening exists to remove, not one to add. A lone code also has nothing for a
// tag to distinguish it from: it is the link's default by arithmetic, an
// untagged scan is counted against it either way, and it cannot be removed while
// it is the only one. Nothing about it is decided by a slug it does not have.
//
// So the empty slug survives, and what it means has changed completely. It was
// the *identity* of the default code, load-bearing in three places and
// unremovable because of it. It is now the absence of a name on a code that has
// nobody to be told apart from, and it carries no meaning at all.

// QRCode is one of a link's codes: the style it is drawn with and the URL it
// encodes.
type QRCode struct {
	// ID is the stored row, and the zero uuid means there is no row — a default
	// code nobody has styled or named yet. It is what the per-code API paths
	// address, and it is absent from the JSON for an unstored code rather than
	// answering with a uuid nothing can be done with.
	ID     uuid.UUID `json:"id,omitempty"`
	LinkID uuid.UUID `json:"link_id"`
	// Slug is the identity that travels in the payload. It is generated, never
	// chosen: it is printed, so a workspace-supplied one would be a name somebody
	// has to keep unique across a link's codes and correct across every copy
	// already in the world.
	//
	// **Empty only for a link's single code** (D183). It used to be the default
	// code's identity, and that is what made the default undeletable; the
	// identity is Default below. A code gains a slug when a second one appears
	// beside it, because that is when there is something to tell it apart from —
	// before then it is the link's default by arithmetic and its payload is the
	// one every already-printed picture carries.
	Slug string `json:"slug"`
	// Default says whether an untagged scan resolves through this code (D183).
	//
	// True for exactly one of a link's codes, which
	// `qr_codes_link_default_key` (04400) is what makes true. Also true for the
	// synthesised code above: a link's default exists whether or not a row holds
	// it, and reporting false for the only code a link has would be reporting
	// that the link has no default at all.
	Default bool `json:"default"`
	// Label is what a person reads in the list. Free text, never in a URL, never
	// in the picture, and never seen by the redirect path.
	Label string `json:"label"`
	// Content is exactly what the picture encodes, including the source
	// parameter. Returned so a client can see what a scanner will read rather
	// than having to reconstruct it.
	Content string   `json:"content"`
	Style   qr.Style `json:"style"`
	// Stored is false for a link whose code has never been styled, which renders
	// at the default rather than not at all.
	Stored bool `json:"stored"`
	// Size is the output size in pixels this style draws this content at (M49).
	//
	// **Derived, never stored.** `qr_codes.style` holds a quiet zone in modules
	// and a scale in pixels per module, and how many pixels those come to
	// depends on how many modules the content encodes to — a longer alias is a
	// bigger matrix at the same style. So the number is computed on every read,
	// which is also what makes a style written before M49 read forward: the size
	// it means is the one its margin and scale already produce.
	//
	// It is in the API's answer as well as on the dashboard because the size is
	// now the vocabulary the surface asks in, and a script that could not see the
	// number the form shows would be a second answer to the same question.
	Size int `json:"size"`
	// HasLogo says whether an image has been uploaded against this code (M50.5).
	//
	// **A boolean rather than the image, and there is no endpoint that returns
	// the bytes.** The two operations this milestone adds are set and clear; what
	// a stored logo is *for* is M50.6, which composites it into the picture the
	// existing `.svg` and `.png` paths already serve. Until then the only thing a
	// client needs to know is whether its upload landed and whether a clear took
	// effect, and that is one bit — which is also all the reads fetch, because
	// the bytes are a megabyte a row and a list of twenty codes must not pull
	// them to print twenty names.
	HasLogo bool `json:"has_logo"`
}

// qrRow is one stored code as the service reads it: every column of `qr_codes`
// except the logo, plus whether there is one.
//
// **A type of this package's own rather than sqlc's**, because M50.5 gave the
// three read queries explicit column lists — a star projection would have
// fetched the logo bytes on every read — and sqlc answers a projection with a
// generated row struct per query. Three structurally identical types would
// otherwise mean three copies of qrCodeFrom.
type qrRow struct {
	ID        uuid.UUID
	Slug      string
	Label     string
	Style     []byte
	IsDefault bool
	HasLogo   bool
}

func qrRowFromGet(r dbgen.GetQRCodeRow) qrRow {
	return qrRow{ID: r.ID, Slug: r.Slug, Label: r.Label, Style: r.Style,
		IsDefault: r.IsDefault, HasLogo: r.HasLogo}
}

func qrRowFromDefault(r dbgen.GetDefaultQRCodeRow) qrRow {
	return qrRow{ID: r.ID, Slug: r.Slug, Label: r.Label, Style: r.Style,
		IsDefault: r.IsDefault, HasLogo: r.HasLogo}
}

func qrRowFromList(r dbgen.ListQRCodesRow) qrRow {
	return qrRow{ID: r.ID, Slug: r.Slug, Label: r.Label, Style: r.Style,
		IsDefault: r.IsDefault, HasLogo: r.HasLogo}
}

func qrRowFromUpsert(r dbgen.UpsertQRCodeRow) qrRow {
	return qrRow{ID: r.ID, Slug: r.Slug, Label: r.Label, Style: r.Style,
		IsDefault: r.IsDefault, HasLogo: r.HasLogo}
}

// QRCode returns a link's default code: its content and the style it is drawn
// with.
//
// The shorthand, unchanged in meaning since M41: it answers for the code whose
// payload carries no code parameter, which is the one on every picture this
// product has ever produced. QRCodeBySlug is how a named code is reached.
func (s *Service) QRCode(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID,
) (*QRCode, error) {
	return s.QRCodeBySlug(ctx, actor, linkID, "")
}

// QRCodeBySlug returns one of a link's codes.
//
// The empty slug asks for the default code and never 404s: a link that has never
// been styled or named still has one, drawn at the default style, which is what
// "a QR endpoint returns a code for any link" has meant since M41. Any other
// slug is a row that must exist, and its absence is a 404 rather than a default —
// a code somebody deleted must stop answering, or a printed identity would go on
// resolving after the workspace retired it.
//
// **A link's default code can be reached two ways now and they agree** (D183):
// by the empty string, which dispatches on the flag, and by the slug the flag's
// holder carries. The second is what the codes list links to and what the
// per-code API paths address, and it is why nothing in this function special-
// cases which of the two it was given.
func (s *Service) QRCodeBySlug(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string,
) (*QRCode, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return nil, err
	}
	row, found, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return nil, err
	}
	if !found && slug != "" {
		return nil, domain.ErrNotFound
	}
	code := qrCodeFrom(linkID, l.ShortURL, slug, row, found)
	return &code, nil
}

// ListQRCodes returns every code a link carries, default first.
//
// **The default is synthesised when no row holds it**, for the same reason
// QRCodeBySlug answers for it: the link has that code whether or not anybody has
// styled it, and a list that omitted it would show a link's second code as its
// only one. A link nobody has touched therefore lists exactly one code, which is
// the state every link is in until this milestone's create operation is used.
//
// **The test for synthesising is the flag, with the empty slug behind it**
// (D183). It read `rows[0].Slug != ""`, which was the same question while the
// default was the empty slug; it is now the flag, falling back to that slug for
// the reason GetDefaultQRCode falls back to it — a row can still arrive carrying
// the old spelling and not the new one. Either way the list is ordered so that
// the default leads, and a link with rows and none of them the default is the
// only case that still needs a code invented.
func (s *Service) ListQRCodes(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID,
) ([]QRCode, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListQRCodes(ctx, dbgen.ListQRCodesParams{
		LinkID: linkID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("list qr codes for %s: %w", linkID, err)
	}
	out := make([]QRCode, 0, len(rows)+1)
	if len(rows) == 0 || (!rows[0].IsDefault && rows[0].Slug != "") {
		out = append(out, qrCodeFrom(linkID, l.ShortURL, "", qrRow{}, false))
	}
	for _, row := range rows {
		out = append(out, qrCodeFrom(linkID, l.ShortURL, row.Slug, qrRowFromList(row), true))
	}
	return out, nil
}

// CreateQRCode adds a named code to a link.
//
// A new code starts at the link's *default* style rather than at the product
// default, because somebody adding a second poster wants the poster they already
// have. Nothing is copied that identifies the code it was copied from: the slug
// is new, the label is what the caller asked for, and the two codes are
// thereafter independent.
//
// **Both rows are re-fitted here, and the second return says when that cost the
// reader a number** (M49's third reopening, F225, F226, D185). This is the one
// operation that lengthens a payload: it gives the default a slug, and it copies
// a style fitted against the untagged picture onto a code whose picture carries a
// tag. Either half leaves a `size` the symbol has outgrown, which the drawing
// answers by falling back to margin-and-scale and measuring something else. See
// refitForPayload for what is kept and what is reported.
//
// **The link's default code gets its row here, and this is the moment it has to**
// (D183). Until this reopening a link could carry a named row and a default that
// was synthesised on every read, which is the state the owner reported: two
// codes in the list, and the first with nothing to remove. A code with no row
// has no slug, no flag and nothing to delete, so the second code is not added
// until the first one exists — 04400 did the same for the links already in that
// state, and this keeps new ones out of it.
func (s *Service) CreateQRCode(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, label string,
) (*QRCode, QRSizeRise, error) {
	if !actor.Can(PermUpdate) {
		return nil, QRSizeRise{}, fmt.Errorf(
			"%w: adding a QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return nil, QRSizeRise{}, err
	}
	label = strings.TrimSpace(label)
	if errs := domain.QRCodeLabelErrors(label); len(errs) > 0 {
		return nil, QRSizeRise{}, errs
	}

	count, err := s.q.CountQRCodes(ctx, dbgen.CountQRCodesParams{
		LinkID: linkID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		return nil, QRSizeRise{}, fmt.Errorf("count qr codes for %s: %w", linkID, err)
	}
	// The default code counts against the cap whether or not it has a row, so
	// the check is against the number of codes the link *has* rather than the
	// number of rows it holds. Otherwise a link would be allowed one code more
	// than the cap for as long as its default went unstyled.
	def, defaulted, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, "")
	if err != nil {
		return nil, QRSizeRise{}, err
	}
	held := count
	if !defaulted {
		held++
	}
	if held >= domain.MaxQRCodesPerLink {
		return nil, QRSizeRise{}, qrCapError()
	}

	// **The link's default code gets a row and a slug here, and this is the one
	// moment both are owed** (D183). It gets a row because a code with none has
	// nothing to remove, nothing to flag and nothing to address — which is
	// precisely the state the owner reported: two codes in the list and no way to
	// remove the first. It gets a slug because this is the moment it stops being
	// alone, and a tag is what tells one code from another.
	//
	// That is also the moment its picture changes, and the only one. A copy of it
	// already printed carries no tag, resolves through the flag, and is counted
	// against this same code — which is what D183 means by *nothing already
	// printed changes what it means*. What is downloaded from here on carries the
	// tag, and the two are the same code.
	if !defaulted {
		if def, err = s.materializeDefaultQRCode(ctx, actor.WorkspaceID, linkID); err != nil {
			return nil, QRSizeRise{}, err
		}
	}
	//
	// **The statement writes the flag as well as the slug**, because for one kind
	// of row it is the flag: `storedQRCode` falls back to the empty slug for a row
	// the flag never reached — written by the previous release during a rolling
	// deploy — and taking that slug away without putting the flag on would leave
	// the link with no default at all.
	if def.Slug == "" {
		def.Slug = domain.NewQRCodeSlug()
		if _, err := s.q.NameQRCode(ctx, dbgen.NameQRCodeParams{
			ID: def.ID, WorkspaceID: actor.WorkspaceID, Slug: def.Slug,
		}); err != nil {
			// The flag moved to another of this link's codes between the read
			// above and this write, so the row being named is no longer the
			// default and the partial index says so. Re-read the winner rather
			// than failing the create over a race about which code is the
			// default, the way tag creation re-reads one.
			if !pgerr.IsUniqueViolation(err) {
				return nil, QRSizeRise{}, fmt.Errorf(
					"name the default qr code on %s: %w", linkID, err)
			}
			if def, _, err = s.storedQRCode(ctx, actor.WorkspaceID, linkID, ""); err != nil {
				return nil, QRSizeRise{}, err
			}
		} else {
			def.IsDefault = true
		}
	}

	// **The default code's payload just grew, so its stored size is re-fitted
	// here** (F226, D185). The tag written above is eight characters and a
	// parameter name on top of what the picture said a moment ago, which is
	// enough to push the symbol into the next version — measured, 29 modules to
	// 33 — and a size fitted against the smaller one then no longer contains the
	// larger one and its quiet zone. Without this the row goes on saying 70px
	// while the drawing measures 82, which is the second reopening's claim made
	// false by the create rather than by anything the reader did.
	//
	// **Not conditioned on the naming, though the naming is what makes it
	// necessary.** A row whose payload has already outgrown its size — one
	// written by the release before this rule existed, or by an alias change,
	// which is F228 — is in exactly the state this repairs, and the guard below
	// is what keeps that from costing anything: a row that is already fitted
	// re-fits to itself and no statement runs. Skipping it on a link whose
	// default already had a slug would also make the sentence the reader is shown
	// untrue, because the code created below inherits this style and would come
	// out at a size the code it was copied from is not.
	def, rise, err := s.refitStoredQRCode(ctx, actor.WorkspaceID, linkID, l.ShortURL, def)
	if err != nil {
		return nil, QRSizeRise{}, err
	}

	// The style the link is already drawing at, so a second code looks like the
	// first one until somebody changes it — re-fitted against **this** code's own
	// payload, which is the one it will be drawn from rather than the one it was
	// copied from. The two are the same length once the default has a slug, so
	// this normally repeats the answer above; it is the direct answer to F225,
	// which is written about the copy rather than about the row copied from.
	slug := domain.NewQRCodeSlug()
	inherited := decodeQRStyle(def.Style)
	// **The `H` a logo forced is not copied onto a code that has no logo**
	// (M50.6's second reopening). Every row carrying a logo holds level H —
	// `refitForLogo` writes it on upload and `storeQRStyle` writes it again on
	// every style write, so that a `GET` reports what is drawn (D141) — and the
	// upsert below leaves the new row's `logo` NULL, because a code that has just
	// come into being has no image. Copying the level whole would therefore draw
	// the new code at H with nothing covering it, permanently and with no door
	// back: `refitFromLogo` fires only for a code that *had* a logo. That is
	// F223's own defect rebuilt inside the milestone that closes it, and it is
	// reachable in two clicks — upload a logo, then *Add another code*.
	//
	// **Cleared rather than recomputed here**, because an unset level is a floor
	// of none and the rule answers the rest at the moment the picture is drawn.
	// The re-fit below then measures against the symbol the rule produces rather
	// than against H's larger one, so the copy comes out at the size the code it
	// was copied from is drawn at.
	//
	// **Only when the source carries a logo.** On any instance an operator can
	// have, a level on a logo-less row came from an API caller naming one (D129),
	// which is a choice, and D187 makes it a floor the new code inherits like any
	// other field.
	//
	// The bound is the release rather than the code: a row whose logo was removed
	// by a build older than this one kept the `H` the upload wrote, and would
	// propagate here. QR logos have never shipped — the migration that stores
	// them landed after `v0.2.0` and 0.3.0 is untagged — so no upgrade path
	// produces such a row, and neither instance holds one (checked 2026-08-13,
	// both `qr_codes` tables, zero rows at `H` with a null logo).
	if def.HasLogo {
		inherited.Level = ""
	}
	style, copied := refitForPayload(QRContent(l.ShortURL, slug), inherited)
	if copied.Rose() && !rise.Rose() {
		rise = copied
	}
	blob, err := json.Marshal(style)
	if err != nil {
		return nil, QRSizeRise{}, fmt.Errorf("encode qr style: %w", err)
	}
	row, err := s.q.UpsertQRCode(ctx, dbgen.UpsertQRCodeParams{
		ID: uuid.Must(uuid.NewV7()), LinkID: linkID, WorkspaceID: actor.WorkspaceID,
		Slug: slug, Label: label, Style: blob,
	})
	if err != nil {
		return nil, QRSizeRise{}, fmt.Errorf("insert qr code: %w", err)
	}
	// The redirect snapshot carries this link's slugs, so a new one has to reach
	// every replica before a scan of it can be attributed. Until it does, a scan
	// resolves as the default code — the safe direction, and the same one a cold
	// cache falls in — but the window is a printed code that is not counted as
	// itself, so it is closed here rather than left to REDIRECT_TTL.
	s.invalidateQRLink(ctx, actor, linkID)
	code := qrCodeFrom(linkID, l.ShortURL, row.Slug, qrRowFromUpsert(row), true)
	return &code, rise, nil
}

// SetQRCodeLabel renames one of a link's codes.
//
// The slug is not touched and cannot be: it is printed. A rename is a change to
// what the dashboard calls a code, never to what the code says.
func (s *Service) SetQRCodeLabel(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug, label string,
) (*QRCode, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf(
			"%w: renaming a QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return nil, err
	}
	label = strings.TrimSpace(label)
	if errs := domain.QRCodeLabelErrors(label); len(errs) > 0 {
		return nil, errs
	}

	// Naming the default code is the first thing that makes a row for it, which
	// is the same trade styling one makes: a row appears when somebody expresses
	// a preference and not before. Since D183 the row that appears carries a
	// generated slug and the flag, so the rename below is a rename of a code with
	// an identity rather than of the absence of one.
	row, err := s.qrTargetRow(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return nil, err
	}

	if _, err := s.q.UpdateQRCodeLabel(ctx, dbgen.UpdateQRCodeLabelParams{
		ID: row.ID, WorkspaceID: actor.WorkspaceID, Label: label,
	}); err != nil {
		return nil, fmt.Errorf("rename qr code %s: %w", row.ID, err)
	}
	row.Label = label
	code := qrCodeFrom(linkID, l.ShortURL, slug, row, true)
	return &code, nil
}

// DeleteQRCode removes one of a link's codes, and reports which code was
// promoted if the one removed held the default flag.
//
// **Any code can go, and the last one cannot** (D183). It used to be the default
// code that could not go, because the default *was* the row with no slug and
// deleting it would have left every already-printed picture resolving to
// nothing. The owner rejected that: *"As long as there are multiple QR codes any
// of them should be able to be removed"* (F222). What replaces it is arithmetic
// rather than identity — a link always has a code, so the refusal falls on
// whichever one is the last, and what the caller almost certainly means by
// removing it is ResetQRStyleBySlug.
//
// **The arithmetic counts codes rather than rows**, which is the same count the
// list on the page shows and the same one CreateQRCode checks the cap against.
// The two differ on exactly one shape — a link holding a named row whose default
// has no row of its own — and counting rows there would put a Remove control on
// two codes and refuse both.
//
// **Removing the flag-holder promotes the oldest code that is left**, and the
// promotion is returned rather than performed silently, because it moves where
// every untagged picture of this link lands. Which code to promote is a decision
// and not a detail — oldest, first-in-list and the one the reader was looking at
// were all defensible. Oldest wins because it is the only one that is a property
// of the *data*: "first in list" is the same rule wearing a presentation's name,
// since the list orders by `created_at, id` once the flag-holder is out of it,
// and "the one the reader was looking at" cannot be expressed by a caller that
// is not a browser, so the API and the dashboard would promote different codes
// from the same delete. It is also the most conservative reading of what the
// flag is for: the longest-lived code is the one most likely to have pictures of
// it in the world, and the flag is what those pictures resolve through.
//
// A deleted code's scans stop accumulating; they are not reassigned. A payload
// naming a slug that no longer exists is recorded as no code at all, which the
// analytics attribute to whichever code holds the flag, and the rows the deleted
// code already earned stay exactly where they are under its slug.
//
// The whole of it is one transaction, because a delete that promoted nothing
// would leave a link with codes and no default — a state the read path answers
// by inventing a code the link does not have.
func (s *Service) DeleteQRCode(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string,
) (promoted *QRCode, err error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf(
			"%w: removing a QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	if _, err := s.Get(ctx, actor, linkID); err != nil {
		return nil, err
	}
	// The link's default is read first whatever was asked for, because the
	// refusal below is about how many codes the link *has* and that is the one
	// question a row count cannot answer.
	def, defaulted, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, "")
	if err != nil {
		return nil, err
	}
	row, found := def, defaulted
	if slug != "" {
		if row, found, err = s.storedQRCode(ctx, actor.WorkspaceID, linkID, slug); err != nil {
			return nil, err
		}
		if !found {
			return nil, domain.ErrNotFound
		}
	}
	count, err := s.q.CountQRCodes(ctx, dbgen.CountQRCodesParams{
		LinkID: linkID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("count qr codes for %s: %w", linkID, err)
	}
	// **Codes, not rows, and it is the same arithmetic CreateQRCode's cap check
	// does.** A link can carry one named row and a default that no row holds —
	// the shape the previous release wrote, and the shape 04400 fixes only for
	// the links that existed when it ran. Counting rows there would refuse to
	// remove either of the two codes the reader is looking at, and refuse the
	// named one by telling them it is the link's only code. The list on the page
	// counts the same way, so the control and the refusal now agree about what
	// "one code" means.
	held := count
	if !defaulted {
		held++
	}
	if held <= 1 {
		return nil, qrLastCodeError()
	}
	if !found {
		// The default code with no row, on a link that has another code. There is
		// nothing to delete, and what removing it *means* is the whole of what the
		// flag means: the code every untagged picture of this link resolves
		// through stops being this one and becomes the oldest code that is
		// written down. That is the same promotion the delete below performs, so
		// it is performed by the same operation rather than by a second spelling
		// of it.
		next, err := s.q.OldestQRCode(ctx, dbgen.OldestQRCodeParams{
			LinkID: linkID, WorkspaceID: actor.WorkspaceID, ID: uuid.Nil,
		})
		if err != nil {
			return nil, fmt.Errorf("find a qr code to promote for %s: %w", linkID, err)
		}
		return s.SetDefaultQRCode(ctx, actor, linkID, next.Slug)
	}
	// **Whether this row is the one untagged scans resolve through**, which is
	// GetDefaultQRCode's question and therefore its predicate: a row carrying the
	// empty slug and not the flag is a default the previous release wrote, and
	// removing it has to promote for the same reason removing a flagged one does.
	wasDefault := row.IsDefault || row.Slug == ""

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if _, err := q.DeleteQRCodeByID(ctx, dbgen.DeleteQRCodeByIDParams{
		ID: row.ID, WorkspaceID: actor.WorkspaceID,
	}); err != nil {
		return nil, fmt.Errorf("delete qr code %s: %w", row.ID, err)
	}
	var next dbgen.OldestQRCodeRow
	if wasDefault {
		// Excluding the row just deleted as well as filtering on what is left,
		// because the statement is correct either way and one of the two is a
		// belt this transaction cannot afford to be without: promoting the code
		// that was removed would leave the link with no default at all.
		if next, err = q.OldestQRCode(ctx, dbgen.OldestQRCodeParams{
			LinkID: linkID, WorkspaceID: actor.WorkspaceID, ID: row.ID,
		}); err != nil {
			return nil, fmt.Errorf("find a qr code to promote for %s: %w", linkID, err)
		}
		if _, err := q.MarkDefaultQRCode(ctx, dbgen.MarkDefaultQRCodeParams{
			ID: next.ID, WorkspaceID: actor.WorkspaceID,
		}); err != nil {
			return nil, fmt.Errorf("promote qr code %s: %w", next.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// The other half of the create's reasoning, and the half that is visible in
	// the data: a replica still holding the deleted slug goes on attributing
	// that printed code to itself, so the row it stopped earning keeps growing
	// for up to REDIRECT_TTL after somebody removed it.
	s.invalidateQRLink(ctx, actor, linkID)
	if !wasDefault {
		return nil, nil
	}
	// Read back rather than assembled from what was written, so the code the
	// caller is told about is the one the next reader will see.
	return s.QRCodeBySlug(ctx, actor, linkID, next.Slug)
}

// SetDefaultQRCode makes one of a link's codes the one untagged scans resolve
// through (D183).
//
// **No clearing operation beside it, because a link always has a default.** The
// flag is not a preference that can be withdrawn — it answers "where does a
// picture with no tag on it land", and that question has an answer for every
// link whether anybody has chosen one or not. So this moves the flag and there
// is nothing that removes it.
//
// The code must already have a row, which for a named code it always does. The
// default's own row is written here if it has none, because moving the flag off
// a code that is not written down is moving it off nothing.
func (s *Service) SetDefaultQRCode(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string,
) (*QRCode, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf(
			"%w: setting a link's default QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	if _, err := s.Get(ctx, actor, linkID); err != nil {
		return nil, err
	}
	row, err := s.qrTargetRow(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return nil, err
	}
	if row.IsDefault {
		// Already the answer. Reported as success rather than as a no-op error,
		// on the reason ResetQRStyle gives for a link with no row: the caller
		// asked for a state and the state holds.
		return s.QRCodeBySlug(ctx, actor, linkID, row.Slug)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	if _, err := q.ClearDefaultQRCode(ctx, dbgen.ClearDefaultQRCodeParams{
		LinkID: linkID, WorkspaceID: actor.WorkspaceID,
	}); err != nil {
		return nil, fmt.Errorf("clear default qr code for %s: %w", linkID, err)
	}
	if _, err := q.MarkDefaultQRCode(ctx, dbgen.MarkDefaultQRCodeParams{
		ID: row.ID, WorkspaceID: actor.WorkspaceID,
	}); err != nil {
		return nil, fmt.Errorf("set default qr code %s: %w", row.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	// **No snapshot invalidation, and the absence is the point.** The redirect
	// path reads slugs and nothing else: the flag is not in the snapshot, is not
	// consulted by `clickSource`, and does not change what any request records.
	// Where an untagged scan is *shown* moves, and that is read from the database
	// every time the breakdown is drawn.
	return s.QRCodeBySlug(ctx, actor, linkID, row.Slug)
}

// qrLastCodeError is the refusal that replaced "the default cannot be removed".
func qrLastCodeError() error {
	return domain.ValidationErrors{{
		Field: "slug", Code: "invalid",
		Message: "this is the link's only QR code, and a link always has one — every " +
			"picture of it already printed resolves through this code, so removing it " +
			"would leave them resolving to nothing. Add another code first, or restore " +
			"this one's defaults instead",
	}}
}

// invalidateQRLink drops the link's cached snapshot after its code set changes.
//
// Only the code *set* needs this. A style, a label and a size are dashboard
// facts that never enter the snapshot, so writing one invalidates nothing; the
// slugs are the one part of a QR code the redirect path reads, and adding or
// removing one is the only thing that can make a cached entry disagree with the
// database about which scans belong to which code.
//
// The alias is read here rather than carried down from Get, because
// domain.Link does not expose the domain id and the invalidation key is the
// pair. A failure is swallowed for the reason invalidateLink swallows one: the
// write already happened, and the worst outcome is attribution being briefly
// wrong rather than a visitor going anywhere unexpected.
func (s *Service) invalidateQRLink(ctx context.Context, actor *auth.Identity, linkID uuid.UUID) {
	if s.cache == nil {
		return
	}
	row, err := s.q.GetLink(ctx, dbgen.GetLinkParams{ID: linkID, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		return
	}
	s.invalidateLink(ctx, row.DomainID, row.Alias)
}

// qrCapError is the refusal both create paths share.
func qrCapError() error {
	return domain.ValidationErrors{{
		Field: "codes", Code: "limit_reached",
		Message: fmt.Sprintf("a link carries at most %d QR codes, and this one already has "+
			"them; remove one before adding another", domain.MaxQRCodesPerLink),
	}}
}

// qrCodeFrom assembles the view of one code from its row, or from nothing.
//
// The `found` argument is what makes the second case expressible: a default code
// with no row is a real code drawn at the default style, and the zero row is how
// it arrives here.
//
// **The slug comes off the row rather than off the argument** (D183). Callers
// pass the empty string to mean "the default code", which is a request and no
// longer an identity: the row that answers it carries a slug of its own, and
// building the payload from what was asked for would draw the default code
// without the tag it has.
func qrCodeFrom(
	linkID uuid.UUID, shortURL, slug string, row qrRow, found bool,
) QRCode {
	isDefault := !found
	if found {
		slug, isDefault = row.Slug, row.IsDefault
	}
	style := decodeQRStyle(row.Style)
	// The level a code with a logo is *drawn* at, which is the one a caller has
	// to be told about (M50.6, D141). SetQRStyleBySlug writes H into the row
	// whenever there is a logo, so this normally changes nothing; it is what
	// keeps the answer honest for a row written before this milestone, or by
	// hand, or by a style write that raced an upload.
	if found && row.HasLogo {
		style = style.ForLogo()
	}
	content := QRContent(shortURL, slug)
	// **And the level the rule resolves, for every other code** (D184, D187). A
	// row holds a floor — usually none at all — and the picture carries the
	// stronger of that floor and the strongest free level, so the two are no
	// longer the same string. Reporting the row's would tell a caller `M` about a
	// picture drawn at `Q`, which is the drift the level's contract test already
	// refuses for logos, one field along. qr.Drawn answers this and the size off
	// one encode.
	style, size := qr.Drawn(content, style)
	out := QRCode{
		LinkID: linkID, Slug: slug, Default: isDefault, Content: content,
		Style: style, Stored: found, Size: size,
	}
	if found {
		out.ID = row.ID
		out.Label = row.Label
		out.HasLogo = row.HasLogo
	}
	return out
}

// QROutputSize is the pixel size a style draws a piece of content at, or 0 for
// content that cannot be encoded at all.
//
// Zero rather than an error, because every caller is answering "how big is this
// picture" about a picture it is already reporting a failure for by other means:
// the panel shows its own message and the API's JSON view is not the surface
// that draws anything. A size beside a code that does not exist is the one
// answer that would be actively wrong.
//
// Exported for the same reason QRContent is — two surfaces ask, and a second
// copy of the arithmetic is a second answer.
//
// Through qr.Drawn since D184, so the size is measured against the symbol the
// resolved level produces rather than against the floor the row names. The two
// come apart wherever the floor is above the free level — every logo'd code —
// and asking through the one function is what keeps that a property of the
// encoder rather than of each caller remembering it.
func QROutputSize(content string, style qr.Style) int {
	_, px := qr.Drawn(content, style)
	return px
}

// RenderQR draws a link's default code as SVG.
func (s *Service) RenderQR(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID,
) ([]byte, error) {
	return s.RenderQRBySlug(ctx, actor, linkID, "")
}

// RenderQRBySlug draws one of a link's codes as SVG.
func (s *Service) RenderQRBySlug(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string,
) ([]byte, error) {
	code, err := s.QRCodeBySlug(ctx, actor, linkID, slug)
	if err != nil {
		return nil, err
	}
	// `code.Slug` and not `slug`: the caller may have asked for the default code
	// by the empty string, and the logo is stored against the row that answered
	// (D183). Keying the read on what was asked for finds nothing on a link whose
	// default has a slug, and the picture comes back without its logo.
	logo, err := s.qrLogoFor(ctx, actor, linkID, code.Slug, code.HasLogo)
	if err != nil {
		return nil, err
	}
	// The empty class rather than qr.Render's default, because this is the file
	// somebody downloads and not a picture inlined into a page (F184). A class
	// list means nothing outside the dashboard's stylesheet, and this document
	// leaves the product: what it carries is the size it was asked for and
	// nothing that only resolves here.
	svg, err := qr.RenderClassWithLogo(code.Content, code.Style, "", logo)
	if err != nil {
		// A stored style is normalized before it is written, and the content is
		// a short URL, so reaching here means the row was edited outside the
		// product or the URL grew past qr.MaxContent. Neither is the caller's
		// mistake, so it is a 500 rather than a 422.
		return nil, fmt.Errorf("render qr for %s: %w", linkID, err)
	}
	return svg, nil
}

// RenderQRPNG rasterises a link's code (M49).
//
// **The one refusal that is the caller's fault is the size.** A style written
// before M49 carries whatever margin and scale it was given, up to 16 and 32,
// and a long URL at those settings describes a picture past qr.MaxSize. That is
// a 422 rather than a 500: the reader can make it smaller, and the alternative —
// rasterising it anyway — is the unbounded allocation D11 refused to allow in
// the first place. Everything else reaching the error path here is the product's
// own mistake, exactly as it is for RenderQR.
func (s *Service) RenderQRPNG(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID,
) ([]byte, error) {
	return s.RenderQRPNGBySlug(ctx, actor, linkID, "")
}

// RenderQRPNGBySlug rasterises one of a link's codes.
func (s *Service) RenderQRPNGBySlug(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string,
) ([]byte, error) {
	code, err := s.QRCodeBySlug(ctx, actor, linkID, slug)
	if err != nil {
		return nil, err
	}
	logo, err := s.qrLogoFor(ctx, actor, linkID, code.Slug, code.HasLogo)
	if err != nil {
		return nil, err
	}
	out, err := qr.RenderPNGWithLogo(code.Content, code.Style, logo)
	if err != nil {
		if errors.Is(err, qr.ErrTooLarge) {
			return nil, domain.ValidationErrors{{
				Field: "style.size", Code: "out_of_range",
				Message: fmt.Sprintf(
					"this code is drawn at %dpx and a downloadable image stops at %dpx; "+
						"set a smaller size and download it again", code.Size, qr.MaxSize),
			}}
		}
		return nil, fmt.Errorf("render qr png for %s: %w", linkID, err)
	}
	return out, nil
}

// QRSizeInput is the dashboard's write: the colours somebody chose and the one
// number they know, which is how big they want the picture (M49).
//
// **The error-correction level is deliberately absent**, and its absence is what
// makes SetQRSize a different operation from SetQRStyle rather than a wrapper
// with defaults. A save from a form that no longer asks about error correction
// must not silently answer the question; the level a link already has is carried
// forward, and a caller that wants to choose one uses the API.
//
// Since D184 what is carried forward is the **floor** — usually none at all, and
// then the level is the rule's. A form that wrote a level here would be pinning
// one for a reader who was never asked, which is the shape of the defect that
// reopened this milestone.
type QRSizeInput struct {
	Foreground string
	Background string
	// Size is the output size in pixels, and it is the size drawn: since D182
	// nothing snaps, because the fit puts the rounding remainder into the quiet
	// zone rather than into this number.
	Size int
}

// SetQRSize stores a style described by its output size.
//
// The size is resolved against *this link's* module count, because that is what
// decides how many pixels a scale comes to. The number is then written into the
// row — `qr.Style.Size`, which SetQRSizeBySlug sets below (D182) — so a link
// whose alias grows later goes on drawing at exactly it, until the larger symbol
// and its minimum quiet zone no longer fit inside that many pixels. Past that
// point the margin and scale stored alongside it take over and the picture draws
// slightly larger, which is the same behaviour every pre-M49 style has.
func (s *Service) SetQRSize(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, in QRSizeInput,
) (*QRCode, qr.SizeFit, error) {
	return s.SetQRSizeBySlug(ctx, actor, linkID, "", in)
}

// SetQRSizeBySlug stores one code's style, described by its output size.
//
// The size is resolved against *this code's* module count rather than the
// link's. Two codes for one link encode different payloads — one carries a slug
// and the other does not — so they are different matrices, and a size fitted
// against the wrong one snaps to a scale that draws the picture at some other
// number of pixels than the one asked for.
func (s *Service) SetQRSizeBySlug(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string, in QRSizeInput,
) (*QRCode, qr.SizeFit, error) {
	if !actor.Can(PermUpdate) {
		return nil, qr.SizeFit{}, fmt.Errorf(
			"%w: styling a QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return nil, qr.SizeFit{}, err
	}
	row, found, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return nil, qr.SizeFit{}, err
	}
	if !found && slug != "" {
		return nil, qr.SizeFit{}, domain.ErrNotFound
	}
	// The row's own slug, because the fit is arithmetic over this code's module
	// count and the module count is a property of the payload (D183). A caller
	// reaching the default code by the empty string gets the code, and the code's
	// picture carries whatever slug it has — fitting against the untagged payload
	// and drawing a tagged one is how the size the reader asked for stops being
	// the size the picture is.
	if found {
		slug = row.Slug
	}
	current := decodeQRStyle(row.Style)
	// **Fitted against the level the picture will be drawn at, not the one the
	// row happens to hold (F190).** A size is a pixel arithmetic over a module
	// count, and a logo forces the level to H, which is a different module count
	// — so fitting against a stored `M` and drawing at `H` snaps to a scale that
	// comes out at some other number of pixels than the reader typed. This is the
	// same defence `qrCodeFrom` applies on read, and its existence there is the
	// evidence the disagreement is reachable: a row written before M50.6, one
	// written by hand, or a style write that raced an upload. The size control is
	// the second site that needs it and was the one without it.
	if found && row.HasLogo {
		current = current.ForLogo()
	}

	code, err := qr.Encode(QRContent(l.ShortURL, slug), current.Level)
	if err != nil {
		return nil, qr.SizeFit{}, fmt.Errorf("encode qr for %s: %w", linkID, err)
	}
	fit, err := qr.FitSize(code.Size, in.Size)
	if err != nil {
		if errors.Is(err, qr.ErrSizeOutOfRange) {
			// **Two refusals wearing one code, and the second names a number the
			// form cannot know** (D182). Outside [MinSize, MaxSize] is the
			// control's own range; inside it but below what *this* symbol can
			// hold is a floor that depends on the payload, so the sentence
			// carries qr.MinSizeFor's answer rather than the constant.
			message := fmt.Sprintf("a code is %d to %d pixels across; %d is not a size "+
				"anything can be printed at", qr.MinSize, qr.MaxSize, in.Size)
			if floor := qr.MinSizeFor(code.Size); in.Size >= qr.MinSize && in.Size < floor {
				message = fmt.Sprintf("this code is %d modules across, so it needs at least "+
					"%d pixels to leave a quiet zone anything can read; %d is below that",
					code.Size, floor, in.Size)
			}
			return nil, qr.SizeFit{}, domain.ValidationErrors{{
				Field: "style.size", Code: "out_of_range", Message: message,
			}}
		}
		return nil, qr.SizeFit{}, fmt.Errorf("fit qr size for %s: %w", linkID, err)
	}

	// Margin as well as Size, and it is not redundant: the quiet zone in modules
	// is what the drawing falls back to if this link's alias ever grows the
	// symbol past what Size can hold, and a row that carried no fallback would
	// draw at a zero quiet zone on that day.
	stored, err := s.SetQRStyleBySlug(ctx, actor, linkID, slug, qr.Style{
		Foreground: in.Foreground, Background: in.Background,
		Level: current.Level, Margin: qr.DefaultMargin, Scale: fit.Scale, Size: fit.Size,
	})
	if err != nil {
		return nil, qr.SizeFit{}, err
	}
	return stored, fit, nil
}

// SetQRStyle stores how a link's default code is drawn.
func (s *Service) SetQRStyle(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, style qr.Style,
) (*QRCode, error) {
	return s.SetQRStyleBySlug(ctx, actor, linkID, "", style)
}

// SetQRStyleBySlug stores how one of a link's codes is drawn.
//
// A named code must already exist: styling is a change to a code, and the
// operation that brings one into being is CreateQRCode. The default code is the
// exception it has always been — its row appears the first time somebody
// expresses a preference about it.
func (s *Service) SetQRStyleBySlug(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string, style qr.Style,
) (*QRCode, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf("%w: styling a QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return nil, err
	}

	normalized, styleErrs := style.Normalize()
	if len(styleErrs) > 0 {
		errs := make(domain.ValidationErrors, 0, len(styleErrs))
		for _, e := range styleErrs {
			errs = append(errs, domain.FieldError{
				Field: "style." + e.Field, Code: e.Code, Message: e.Message,
			})
		}
		return nil, errs
	}

	out, err := s.storeQRStyle(ctx, actor.WorkspaceID, l, linkID, slug, normalized)
	// **A unique violation here is a race about which code is the default, and
	// the answer is to look again** (D183). The caller asked for the default by
	// passing the empty slug; between that read and the insert a concurrent
	// CreateQRCode can write the default's row down and name it, at which point
	// this insert no longer conflicts on `(link_id, '')` — it conflicts on
	// `qr_codes_link_default_key`, because both rows claim the flag. The second
	// attempt reads the winner, finds the default's row where the first attempt
	// found none, and writes the style onto it through the upsert's conflict
	// branch, which is what the caller asked for and what a request arriving a
	// moment later would have done. Once only: a second violation is not a race
	// this can reason about, and a 500 is the honest answer to it.
	//
	// Named codes cannot reach this. Their row exists, so the upsert takes its
	// conflict branch and never inserts, and the branch does not touch the flag.
	if err != nil && slug == "" && pgerr.IsUniqueViolation(err) {
		out, err = s.storeQRStyle(ctx, actor.WorkspaceID, l, linkID, slug, normalized)
	}
	return out, err
}

// storeQRStyle is one attempt at writing a style onto the code `slug` names, the
// empty slug being the link's default.
//
// Separate from its caller so that the attempt can be repeated: everything it
// does depends on a read that a concurrent write can invalidate, and repeating
// the read without repeating the size check below would check a floor against a
// payload the code no longer has.
func (s *Service) storeQRStyle(
	ctx context.Context, workspaceID uuid.UUID, l *domain.Link,
	linkID uuid.UUID, slug string, normalized qr.Style,
) (*QRCode, error) {
	// Stored normalized, so what is written is what will be drawn. A row holding
	// an unchecked colour would be a row whose only validation was the form that
	// happened to write it.
	blob, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("encode qr style: %w", err)
	}
	// The label the code already carries, so a style write does not rename it.
	// The upsert leaves `label` alone on conflict, and this is what supplies it
	// for the insert branch — a named code that reached here exists, so this is
	// its own label, and the default code's is empty until somebody sets one.
	//
	// **The slug written is the row's own** (D183). A caller reaching the default
	// code passes the empty string, which is a request rather than an identity
	// since the flag replaced it, and on a link whose default has a slug the
	// upsert has to key on that slug or it inserts a second row beside it. Where
	// there is no row at all the empty string is also what gets written, and it
	// is correct: a link's only code has no slug until a second one appears
	// beside it, and this write is a style rather than the appearance of one.
	existing, found, err := s.storedQRCode(ctx, workspaceID, linkID, slug)
	if err != nil {
		return nil, err
	}
	if !found && slug != "" {
		return nil, domain.ErrNotFound
	}
	if found {
		slug = existing.Slug
	}
	// **A code carrying a logo is stored at level H, whatever was asked for**
	// (M50.6, D141). Accept-and-override rather than refuse: this endpoint is a
	// PUT that replaces the style whole, so an omitted level names no floor at
	// all (D184), and refusing would make every colour change on a logo'd
	// code a 422 for a field the caller never mentioned. What the milestone
	// forbids is silence, and there is none — the row holds H, the response below
	// returns H, and a `GET` after this `PUT` reports what was applied.
	if existing.HasLogo {
		normalized = normalized.ForLogo()
		if blob, err = json.Marshal(normalized); err != nil {
			return nil, fmt.Errorf("encode qr style: %w", err)
		}
	}
	// **The size floor is the endpoint's to enforce, because a style does not
	// carry a module count** (D182). `Normalize` range-checks `size` against
	// [qr.MinSize] and [qr.MaxSize] and can do no more: the pixels a symbol needs
	// are a property of the symbol, and the picture is the one thing a style has
	// never known. Without this an API caller could store a size the symbol has
	// outgrown at its own scale, `qr.Code.geometry` would fall back to the
	// margin-and-scale arithmetic, and the drawing would measure something other
	// than what was asked for. M49's claim is *the requested size is the size
	// stored and drawn, exactly*, and it is false whichever door the style came
	// through — so the refusal `SetQRSizeBySlug` gives the form is given to the
	// API too.
	//
	// **[qr.MinSizeForStyle] rather than [qr.MinSizeFor], because this caller
	// also set the scale.** The form's floor is the one over every module width,
	// since the form picks the width; a style is `size` *and* `scale`, so what
	// binds here is the floor at the scale that was actually sent. Both numbers
	// are in the sentence, because lowering `scale` is the other way to satisfy
	// it and a message naming one number would not say so. The size control's own
	// writes arrive already fitted and never reach this.
	if normalized.Size != 0 {
		code, err := qr.Encode(QRContent(l.ShortURL, slug), normalized.Level)
		if err != nil {
			return nil, fmt.Errorf("encode qr for %s: %w", linkID, err)
		}
		if floor := qr.MinSizeForStyle(code.Size, normalized); normalized.Size < floor {
			return nil, domain.ValidationErrors{{
				Field: "style.size", Code: "out_of_range",
				Message: fmt.Sprintf(
					"this code is %d modules across, so at %d pixels a module it needs at "+
						"least %d pixels to leave a quiet zone anything can read; %d is "+
						"below that. Its floor at the narrowest module is %d",
					code.Size, normalized.Scale, floor, normalized.Size,
					qr.MinSizeFor(code.Size)),
			}}
		}
	}
	// **The insert branch is where a link's default code comes into being**, so
	// it is the branch that has to set the flag: a row written here for a code
	// that had none is the code an untagged scan resolves through, and a link
	// with codes and no default is a link the read path answers by inventing one.
	// `ON CONFLICT` leaves the column alone, which is what keeps a style write
	// from moving a flag it was never asked about.
	row, err := s.q.UpsertQRCode(ctx, dbgen.UpsertQRCodeParams{
		ID: uuid.Must(uuid.NewV7()), LinkID: linkID, WorkspaceID: workspaceID,
		Slug: slug, Label: existing.Label, Style: blob, IsDefault: !found || existing.IsDefault,
	})
	if err != nil {
		return nil, fmt.Errorf("upsert qr code: %w", err)
	}
	// **The view comes off the row and nothing overwrites it** (D184). It used to
	// end `out.Style = normalized`, which was the same struct by another route —
	// the upsert returns the blob it just wrote and qrCodeFrom decodes it — until
	// the level stopped being the string a row holds. What the row holds is the
	// floor; what the caller is owed is the level the picture carries, and
	// qrCodeFrom is the one place that resolves it.
	out := qrCodeFrom(linkID, l.ShortURL, slug, qrRowFromUpsert(row), true)
	return &out, nil
}

// ResetQRStyle returns a link's default code to the default style.
func (s *Service) ResetQRStyle(ctx context.Context, actor *auth.Identity, linkID uuid.UUID) error {
	return s.ResetQRStyleBySlug(ctx, actor, linkID, "")
}

// ResetQRStyleBySlug returns one of a link's codes to the default style.
//
// **It used to take no slug at all, and that was the second half of F222.**
// Pressing *Restore defaults* while a named code was selected cleared the
// *default* code's style — a control on a form about one code writing to
// another, and then dropping the reader onto the code it had written to. D183
// scopes it to the selection.
//
// **It writes the default style rather than deleting the row**, which is the
// other thing D183 changed here. Deleting was honest while a row held nothing
// but the preference being withdrawn; the row now holds the code's identity —
// its slug, which is printed, and the flag that says untagged scans resolve
// through it — and dropping those to clear two colours and a size would retire a
// printed identity to undo a styling. A named code has never been resettable by
// deletion for the same reason, and this is what it means for one.
//
// The logo stays. It is not part of the style: *Remove the logo* is its own
// control with its own sentence, and a button labelled *Restore defaults* that
// silently discarded an uploaded image would be doing something nobody could
// read off it.
//
// No error for a link whose code has no row. "Draw this at the default style" is
// already true, and reporting 404 for it would make the operation care whether a
// preference had ever been expressed — and materialising a row to write the
// style that row would have been read as anyway is a write for nothing.
func (s *Service) ResetQRStyleBySlug(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string,
) error {
	if !actor.Can(PermUpdate) {
		return fmt.Errorf("%w: styling a QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	// Through Get, so a link in another workspace is a 404 rather than a silent
	// no-op that reports success.
	if _, err := s.Get(ctx, actor, linkID); err != nil {
		return err
	}
	row, found, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return err
	}
	if !found {
		if slug != "" {
			return domain.ErrNotFound
		}
		return nil
	}
	blob, err := json.Marshal(decodeQRStyle(nil))
	if err != nil {
		return fmt.Errorf("encode qr style: %w", err)
	}
	if _, err := s.q.UpsertQRCode(ctx, dbgen.UpsertQRCodeParams{
		ID: row.ID, LinkID: linkID, WorkspaceID: actor.WorkspaceID,
		Slug: row.Slug, Label: row.Label, Style: blob, IsDefault: row.IsDefault,
	}); err != nil {
		return fmt.Errorf("reset qr code style %s: %w", row.ID, err)
	}
	return nil
}

// A logo on a code (M50.5).
//
// **The first file this product accepts, and the service layer's whole part in
// it is two writes and a translation.** The bytes arrive already bounded: the
// handler caps the request body with `http.MaxBytesReader`, and internal/qr's
// NormalizeLogo caps the declared image before decoding it and re-encodes what
// it decodes. What is left here is the authorization, the code lookup, and
// turning internal/qr's sentinels into the field errors a caller reads — the
// same division RenderQRPNG makes with qr.ErrTooLarge.
//
// **`links.update`, and no permission of its own.** D75's reasoning is
// unchanged by a logo: a QR code is a picture of the link's own short URL, so
// styling one is styling the link's own presentation, and a logo is style. There
// is therefore no new slug to classify and no D18 delegability question to
// answer.
//
// **Not requireSessionActor-gated, deliberately.** D87's limb refuses operations
// whose subject is *the person* — their password, their sessions, where their
// browser lands. The subject here is the link, so an API key that may restyle a
// code may put a logo on it, exactly as it may change its colours today.
//
// **Every code, and the default one is reached by the empty slug.** The owner
// overruled D136 on 2026-08-07: *one upload operation and one to clear* counts
// capabilities rather than routes, so the same two operations answer at the
// `/qr` shorthand D133 kept and at `/qr/codes/{slug}`, exactly as `GET
// …/qr.png` and `GET …/qr/codes/{slug}/image.png` are one capability.
//
// **The empty string is a request for the default code and not the name of one**
// (D183). It was the default's identity when this was written; the identity is
// `is_default` now, and every write and read below resolves the flag before it
// touches a row. Passing the request through instead inserts a second row
// against the empty slug on any link whose default has one — the picture the
// reader downloads then has no logo in it, and the link carries a code nobody
// made.
//
// **What that costs is a row that did not have to exist before.** A default
// code with no `qr_codes` row is a real code drawn at the default style, and a
// column cannot hold bytes for a row that is not there. So an upload against it
// writes the row first, at the style it was already being drawn at.

// LogoFit is what an upload became, and what it was.
//
// **The pair, not a boolean**, for the reason the size control reports both
// numbers rather than "snapped": somebody who uploaded artwork and got a stored
// image at a different size is owed the two figures, and a flag would leave the
// page saying that *something* happened. [qr.FitStoredLogo] is where the second
// pair comes from; this type is only how it reaches a handler without the bytes
// coming with it.
type LogoFit struct {
	// SourceWidth and SourceHeight are what was uploaded, in pixels.
	SourceWidth, SourceHeight int
	// Width and Height are what is stored.
	Width, Height int
}

// Resampled reports whether the upload had to be shrunk to be stored.
func (f LogoFit) Resampled() bool {
	return f.SourceWidth != f.Width || f.SourceHeight != f.Height
}

// SetQRCodeLogo stores an uploaded image against one of a link's codes. The
// empty slug asks for the link's default code, which since D183 is a flag on a
// row rather than the absence of a slug — see the preamble above.
//
// Replacing is the same call: the write is a single UPDATE, so the image being
// replaced is overwritten rather than deleted by a second statement, and there
// is no state in which a code has two logos or none.
//
// **The second return is what the F214 reopening added.** An image past the
// storage target is now shrunk to fit instead of refused, and a caller that was
// not told would report an unqualified success for a picture this product
// changed. It is a [LogoFit] rather than an error because nothing went wrong —
// the upload was accepted, and the sentence beside it is a warning.
func (s *Service) SetQRCodeLogo(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string, upload []byte,
) (*QRCode, LogoFit, error) {
	if !actor.Can(PermUpdate) {
		return nil, LogoFit{}, fmt.Errorf(
			"%w: uploading a QR code logo requires %s", domain.ErrForbidden, PermUpdate)
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return nil, LogoFit{}, err
	}
	row, found, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return nil, LogoFit{}, err
	}
	if !found {
		// A named code must already exist — CreateQRCode is what brings one into
		// being, and a slug the link never issued must not be creatable by
		// uploading to it. The default code is the exception it has always been.
		if slug != "" {
			return nil, LogoFit{}, domain.ErrNotFound
		}
		if row, err = s.materializeDefaultQRCode(ctx, actor.WorkspaceID, linkID); err != nil {
			return nil, LogoFit{}, err
		}
	}
	// **The row's own slug, not the one asked for** (D183). The upsert below keys
	// on `(link_id, slug)`, and a caller reaching the default code passes the
	// empty string — which stopped being an identity when the flag replaced it.
	// Writing it through inserts a *second* row on any link whose default has a
	// slug, so the logo would land on the code that was addressed and the level
	// would be raised on a phantom code beside it. It is also the payload
	// `refitForLogo` measures, and measuring the untagged one would fit the
	// occlusion cap against a symbol this code does not draw.
	slug = row.Slug

	logo, err := qr.NormalizeLogo(upload)
	if err != nil {
		return nil, LogoFit{}, qrLogoError(err)
	}
	fit := LogoFit{
		SourceWidth: logo.SourceWidth, SourceHeight: logo.SourceHeight,
		Width: logo.Width, Height: logo.Height,
	}
	if _, err := s.q.SetQRCodeLogo(ctx, dbgen.SetQRCodeLogoParams{
		ID: row.ID, WorkspaceID: actor.WorkspaceID, Logo: logo.PNG,
	}); err != nil {
		return nil, fit, fmt.Errorf("store qr code logo %s: %w", row.ID, err)
	}
	row.HasLogo = true

	// **And the level goes to H, in the row** (M50.6, D141). A logo occludes
	// modules and H's correction budget is what the occlusion cap is measured
	// against, so a code that has one is drawn at H — and the row says so, rather
	// than the renderer quietly disagreeing with what a `GET` reports.
	//
	// After the bytes rather than before them, so the failure that can happen
	// leaves the safe state: a logo stored at a level the row still calls `M`
	// draws at H anyway, because the renderer forces it too. The other order
	// would leave a code with no logo restyled for nothing.
	styled := refitForLogo(QRContent(l.ShortURL, slug), decodeQRStyle(row.Style))
	blob, err := json.Marshal(styled)
	if err != nil {
		return nil, fit, fmt.Errorf("encode qr style: %w", err)
	}
	stored, err := s.q.UpsertQRCode(ctx, dbgen.UpsertQRCodeParams{
		ID: uuid.Must(uuid.NewV7()), LinkID: linkID, WorkspaceID: actor.WorkspaceID,
		Slug: slug, Label: row.Label, Style: blob, IsDefault: row.IsDefault,
	})
	if err != nil {
		return nil, fit, fmt.Errorf("raise qr code %s to level H: %w", row.ID, err)
	}
	row = qrRowFromUpsert(stored)
	row.HasLogo = true
	// No cache invalidation. A logo is a dashboard fact like a style and a label:
	// the redirect snapshot carries a link's slugs and nothing else about its
	// codes, so nothing cached can disagree with this write.
	code := qrCodeFrom(linkID, l.ShortURL, slug, row, true)
	return &code, fit, nil
}

// refitStoredQRCode re-fits one stored row against the payload it now encodes,
// writing it back only if the arithmetic moved (M49's third reopening, D185).
//
// **The guard is what makes this callable unconditionally**, which is how
// CreateQRCode calls it: a row already fitted to its own payload re-fits to
// itself, the comparison is false, and no statement runs. So the cost of asking
// is an encode, and the benefit is that a row left stale by an earlier release —
// or by any path this milestone did not close — is repaired the next time
// something touches the link's codes rather than waiting for somebody to notice
// the picture is the wrong size.
//
// The write is the upsert's conflict branch, which is the one statement in this
// package that writes a style. `label` and `is_default` are outside its SET
// list, so a re-fit cannot rename a code or move the flag; the row is read back
// out of the statement rather than assembled here, on QRCode's own reasoning
// that what a caller is told about is what the next reader will see.
func (s *Service) refitStoredQRCode(
	ctx context.Context, workspaceID, linkID uuid.UUID, shortURL string, row qrRow,
) (qrRow, QRSizeRise, error) {
	style := decodeQRStyle(row.Style)
	// Fitted against the level the picture is *drawn* at rather than the one the
	// row happens to hold — the same defence qrCodeFrom and SetQRSizeBySlug make
	// (F190). A logo forces H, H is a larger symbol, and a symbol's module count
	// is what every number below is derived from.
	if row.HasLogo {
		style = style.ForLogo()
	}
	refitted, rise := refitForPayload(QRContent(shortURL, row.Slug), style)
	if refitted == style {
		return row, QRSizeRise{}, nil
	}
	blob, err := json.Marshal(refitted)
	if err != nil {
		return row, QRSizeRise{}, fmt.Errorf("encode qr style: %w", err)
	}
	stored, err := s.q.UpsertQRCode(ctx, dbgen.UpsertQRCodeParams{
		ID: uuid.Must(uuid.NewV7()), LinkID: linkID, WorkspaceID: workspaceID,
		Slug: row.Slug, Label: row.Label, Style: blob, IsDefault: row.IsDefault,
	})
	if err != nil {
		return row, QRSizeRise{}, fmt.Errorf("re-fit qr code %s: %w", row.ID, err)
	}
	return qrRowFromUpsert(stored), rise, nil
}

// QRSizeRise is a re-fit that had to push a stored size **up**, and it is the
// only re-fit anybody is told about (M49's third reopening, D185).
//
// Owner-set, in the answer that reopened the milestone: *"The user doesn't need
// to be notified unless we need to raise the currently selected size."* A re-fit
// that keeps the number the reader chose is a scale change they cannot see and a
// picture that measures what it always did, so a sentence about it is a sentence
// that teaches readers to stop reading them. A re-fit that cannot keep it has
// changed a number somebody typed, and that is not the product's to do quietly.
//
// The zero value is *nothing happened*, which is the common case: [QRSizeRise.Rose]
// is what every caller branches on.
type QRSizeRise struct {
	// From is the size the row carried and To is the size it now carries.
	From, To int
}

// Rose reports whether the size the reader chose had to be raised.
func (r QRSizeRise) Rose() bool { return r.To > r.From }

// refitForPayload re-fits a stored size onto a payload that has changed under it
// (M49's third reopening, F225, F226, D185).
//
// **A stored size is fitted against a payload, and a payload can change.** The
// size control resolves a requested number of pixels against *this code's*
// module count, which is a property of what the picture encodes; naming a code
// appends `&qrc=<slug>` and can push the symbol into the next version, at which
// point the pixels the row holds no longer contain the symbol and its quiet zone.
// [qr.Code.geometry] then falls back to the margin-and-scale arithmetic and the
// picture measures something else — 70px stored, 82px drawn on the measurement
// in F226 — which is exactly what the second reopening exists to have made
// impossible.
//
// So the row is re-fitted where the payload changes, rather than the drawing
// being left to discover the disagreement. The size the reader chose is kept
// wherever the larger symbol still admits it, which is nearly everywhere: only
// the scale moves, and the scale is not a number anybody set. Where it does not,
// the size rises to [qr.MinSizeFor] — this code's own floor — and the second
// return says so. **Raising the floor is acceptable and is not a prompt**, owner-set
// in the same answer: *"Raising the lower limit would have next to no affect on
// almost any use case unless it starts to go above a 128px minimum."*
//
// **A style carrying no size is left exactly as it is**, and that is not an
// omission. Such a row is the pre-M49 form — a quiet zone in modules and a scale
// in pixels — and the size it means has always been whatever those two multiply
// out to against the payload of the day. It grows with the payload by
// construction, so there is no number to keep and nothing to report; re-fitting
// it would rewrite a row nobody asked to have rewritten into the newer form,
// which is the trade refitForLogo declines below for the same reason.
func refitForPayload(content string, style qr.Style) (qr.Style, QRSizeRise) {
	if style.Size == 0 {
		return style, QRSizeRise{}
	}
	code, err := qr.Encode(content, style.Level)
	if err != nil {
		// The payload cannot be drawn at all, which the renderers already report
		// by their own means. Rewriting the row over it would be this function
		// deciding a picture's size from a picture that does not exist.
		return style, QRSizeRise{}
	}
	want := max(style.Size, qr.MinSizeFor(code.Size))
	out, ok := fitStyleTo(code.Size, style, want)
	if !ok {
		return style, QRSizeRise{}
	}
	if out.Size == style.Size {
		return out, QRSizeRise{}
	}
	return out, QRSizeRise{From: style.Size, To: out.Size}
}

// fitStyleTo is the arithmetic both re-fits share: `style` carrying the geometry
// that draws a `modules`-module symbol at `want` pixels, or the style untouched
// and false when no scale does.
//
// Separate from its two callers because a size that has stopped matching its
// payload is one defect with two doors — a logo raising the level, and a slug
// lengthening the content — and two copies of this would be two answers to it.
// What the callers keep is the part that differs: which number is being held,
// and what happens when it cannot be.
//
// The margin in modules is written back to [qr.DefaultMargin] rather than left
// as it was, because it is the fallback the drawing uses if this row's payload
// ever outgrows the size again, and a fallback carried over from an older fit is
// a quiet zone nobody chose.
func fitStyleTo(modules int, style qr.Style, want int) (qr.Style, bool) {
	fit, err := qr.FitSize(modules, min(max(want, qr.MinSize), qr.MaxSize))
	if err != nil {
		return style, false
	}
	out := style
	out.Margin, out.Scale, out.Size = qr.DefaultMargin, fit.Scale, fit.Size
	return out, true
}

// refitForLogo is the style a code takes on when an image is put on it: level H,
// and the margin and scale that keep the picture the size it already was (F171,
// D174).
//
// **Level H is not free, and it is not free in pixels.** A logo forces the
// error-correction level up, and H spends its correction budget on modules: an
// 89-byte payload is 37 modules across at L and 53 at H. Carrying the stored
// margin and scale forward therefore *grows* the drawing — at margin 13 and
// scale 31 that is 1953px before the upload and 2449px after, past qr.MaxSize,
// and `GET …/qr.png` begins answering 422 for a code that downloaded a moment
// earlier. Nothing about that is visible from the surface: somebody uploaded a
// picture and a download stopped working.
//
// So the size is re-fitted against the larger symbol, at the size the code was
// already drawn at. **Silently, and the silence is the decision** (D174): the
// recommendation was to re-fit *and* say so, and the owner took the re-fit
// alone. Refusing the upload was the other option and was rejected — it would
// say no to a picture the SVG draws correctly. What travels is unchanged, so a
// code already printed still scans, and the notice the upload already carries
// says the picture is denser than it was, which is now the whole of what
// happened to it.
//
// The style is returned unchanged but for the level when the fit cannot be made:
// fitStyleTo clamps the target into the range qr.FitSize accepts, because a
// style stored before M49 can describe a picture outside it, and a fit that
// fails must not cost a logo that has already been written. **Clamping is not
// enough on its own since D182** — the level-H symbol may need more pixels than
// the picture it is being fitted into, which is `qr.MinSizeFor`'s refusal — and
// that arm lands here as the same "leave the style alone" answer, which draws a
// larger picture rather than none.
//
// **That is where this parts company with refitForPayload**, which shares the
// arithmetic and not this answer (D185). There, the number being held is one the
// reader typed into the size control and the row is written under an operation
// they are watching, so a floor that has risen above it is raised to and
// reported. Here the number is one nobody chose — the size the picture happened
// to be before an upload — and D174 bought the whole re-fit on the promise that
// it moves the style no further than it must.
func refitForLogo(content string, style qr.Style) qr.Style {
	out := style.ForLogo()
	before, err := qr.Encode(content, style.Level)
	if err != nil {
		return out
	}
	after, err := qr.Encode(content, out.Level)
	if err != nil {
		return out
	}
	if after.Size <= before.Size {
		// Nothing grew, so there is nothing to keep. Returning here rather than
		// re-fitting to the same number is what makes "a style already at H comes
		// back byte for byte" true of the struct and not only of the picture: a
		// re-fit would rewrite a pre-M49 row into the newer form for no change a
		// reader could see, and a row nobody asked to have rewritten is a row
		// that should not be.
		return out
	}
	fitted, ok := fitStyleTo(after.Size, out, qr.OutputSize(before.Size, style))
	if !ok {
		return out
	}
	return fitted
}

// ClearQRCodeLogo removes the image from one of a link's codes. The empty slug
// is the link's default code.
//
// **The artefact goes, not just the reference**, which under D134 is one and the
// same write: the bytes are the column. Idempotent, and it does not care whether
// there was a logo — "this code has no logo" is already true for a code that
// never had one, and reporting 404 for it would make the operation care whether
// a preference had ever been expressed, which is the trade ResetQRStyle already
// makes.
//
// **No row is that same case and not a 404**, for the default code only. A
// default code nobody has styled or uploaded to has no row, so it has no logo,
// so the clear has already happened. A named code with no row does not exist,
// and that is still a 404 — which is what stops this operation being a way to
// ask whether a slug was ever issued.
//
// **And the level H the upload forced goes with the image** (F223, D184). It was
// left where it was, on the reasoning that a picture may already be printed and H
// is the safer of the two to be left at — which the owner overruled in as many
// words: *"the old QR should still resolve as long as the link stays the same, so
// a change in the new code shouldn't be an issue."* The payload is untouched, so
// every printed picture goes on resolving; what H costs is ~30% more modules a
// side than the code needs, on a code with nothing left covering it.
//
// **The bytes and the style leave in one statement**, which is the whole of why
// the clear takes a style at all. A style write here would be an upsert, and an
// upsert racing a `DeleteQRCode` finds nothing to conflict with and **inserts a
// fresh row** — the code the reader deleted, back with its slug, because a
// removal wrote to it. One `UPDATE` on the id cannot: a row that is gone updates
// nothing.
func (s *Service) ClearQRCodeLogo(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string,
) error {
	if !actor.Can(PermUpdate) {
		return fmt.Errorf(
			"%w: removing a QR code logo requires %s", domain.ErrForbidden, PermUpdate)
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return err
	}
	row, found, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return err
	}
	if !found {
		if slug != "" {
			return domain.ErrNotFound
		}
		return nil
	}
	// **The style moves only for a code that had a logo**, so a second clear
	// writes back the bytes it read: this is an idempotent operation, and
	// re-fitting a style on a code whose logo left weeks ago would move a size
	// the reader has set since.
	blob := row.Style
	if row.HasLogo {
		styled := refitFromLogo(QRContent(l.ShortURL, row.Slug), decodeQRStyle(row.Style))
		if blob, err = json.Marshal(styled); err != nil {
			return fmt.Errorf("encode qr style: %w", err)
		}
	}
	if _, err := s.q.ClearQRCodeLogo(ctx, dbgen.ClearQRCodeLogoParams{
		ID: row.ID, WorkspaceID: actor.WorkspaceID, Style: blob,
	}); err != nil {
		return fmt.Errorf("clear qr code logo %s: %w", row.ID, err)
	}
	return nil
}

// refitFromLogo is the style a code takes on when its image is removed: the
// level back to the rule, and the size the reader chose held against the smaller
// symbol that produces (F223, D184).
//
// **The level is recomputed rather than restored**, and there is nothing to
// restore in any case — the upload wrote H over whatever the row held, so the
// pre-logo level is not recoverable from the row. That is the same answer the
// owner gave for the reason they gave it: the level a code carries is a property
// of what it encodes, and the payload has not changed.
//
// **The size is the one M49 defends and it survives this.** The rule's symbol is
// never larger than H's — H is one of the levels the rule may return, and it
// returns it only when it costs no version — so a picture that held the level-H
// symbol holds this one, and re-fitting can only find a scale where the fit that
// stored the number already did. What moves is the scale, which nobody set; the
// stored size comes back out of qr.FitSize as itself.
//
// The three ways out are refitForPayload's, for its reasons: a style with no size
// is the pre-M49 form and has no number to hold, content that cannot be encoded
// is a picture whose failure is already reported elsewhere, and a fit that cannot
// be made leaves the style alone rather than costing the removal.
func refitFromLogo(content string, style qr.Style) qr.Style {
	out := style
	out.Level = ""
	if out.Size == 0 {
		return out
	}
	code, err := qr.Encode(content, out.Level)
	if err != nil {
		return out
	}
	fitted, ok := fitStyleTo(code.Size, out, out.Size)
	if !ok {
		return out
	}
	return fitted
}

// Drawing the logo (M50.6).
//
// **The bytes are read once, by the thing that is about to draw them, and by
// nothing else.** M50.5 gave the three reads on `qr_codes` explicit column lists
// so that listing twenty codes does not fetch twenty images; `GetQRCodeLogo` is
// the one query that projects the column, and it runs only for a code whose
// `has_logo` already said there is something to fetch.
//
// **There is still no endpoint that returns a logo.** What M50.6 adds is the
// compositing, so the bytes leave this package inside a picture and never on
// their own. QRCodeLogo below is how the dashboard gets them, and it is a
// service call rather than a route.

// QRCodeLogo returns the image stored against one of a link's codes, or nil.
//
// **`links.read`, the same permission that renders a code**, because that is
// what this is for: the dashboard draws its own SVG rather than fetching the API
// endpoint, so it needs the same input the endpoint's renderer has. Nothing in
// the API document exposes it — the two operations on a logo are still replace
// and remove.
//
// Nil rather than an error for a code with no logo, and for a default code with
// no row: "there is nothing to draw" is the answer in both cases, and it is the
// answer a clear that raced this read should also produce.
func (s *Service) QRCodeLogo(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string,
) ([]byte, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}
	if _, err := s.Get(ctx, actor, linkID); err != nil {
		return nil, err
	}
	// Resolved rather than passed through, for the reason the renderers pass
	// `code.Slug` (D183): the empty string is a request for the default code and
	// not the name of one, so keying the column read on it finds nothing on a
	// link whose default has a slug. Absent is still nil rather than an error —
	// a default code with no row has no logo.
	row, found, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return nil, err
	}
	if !found || !row.HasLogo {
		return nil, nil
	}
	return s.qrLogo(ctx, actor.WorkspaceID, linkID, row.Slug)
}

// qrLogoFor is the read the two renderers make, skipped entirely for a code that
// has no logo — which is nearly every code.
func (s *Service) qrLogoFor(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string, hasLogo bool,
) ([]byte, error) {
	if !hasLogo {
		return nil, nil
	}
	return s.qrLogo(ctx, actor.WorkspaceID, linkID, slug)
}

// qrLogo reads the column. The caller has already been authorized — every path
// here goes through QRCodeBySlug or Get first.
func (s *Service) qrLogo(
	ctx context.Context, workspaceID, linkID uuid.UUID, slug string,
) ([]byte, error) {
	logo, err := s.q.GetQRCodeLogo(ctx, dbgen.GetQRCodeLogoParams{
		LinkID: linkID, WorkspaceID: workspaceID, Slug: slug,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read qr code logo for %s: %w", linkID, err)
	}
	return logo, nil
}

// materializeDefaultQRCode writes the row a link's default code has been
// standing in for, and returns it.
//
// **A default code exists whether or not `qr_codes` holds it** — that is what
// makes "a QR endpoint returns a code for any link" true, and why twenty
// untouched links carry no rows at all. The row appears the first time somebody
// expresses a preference about the code, which until M50.5 meant styling it. A
// logo is the second such preference, and unlike a style it *needs* the row:
// under D134 the bytes are a column on it.
//
// **The style written is the one the code was already being drawn at**, which
// for a code with no row is the product default — `decodeQRStyle(nil)` is
// exactly what every read of that code has been answering with. So nothing
// about the picture changes, and the only visible consequence is `stored`
// turning true, which is already what it means: this code now has a row.
//
// **The row carries the flag and no slug** (D183). The flag is what the code
// being written down *is* — untagged scans resolve through it, which is the
// property it has had since M41 under a different spelling. The slug is not,
// because this row is still the link's only code: it gains one when a second
// code appears beside it and not before, so that writing a preference down never
// changes what a picture says.
//
// The upsert rather than a plain insert, for the reason SetQRStyleBySlug uses
// one: two concurrent uploads to the same untouched default code would
// otherwise be two inserts racing for one (link, slug), and 03700's unique
// index would fail the loser rather than letting it proceed.
func (s *Service) materializeDefaultQRCode(
	ctx context.Context, workspaceID, linkID uuid.UUID,
) (qrRow, error) {
	blob, err := json.Marshal(decodeQRStyle(nil))
	if err != nil {
		return qrRow{}, fmt.Errorf("encode qr style: %w", err)
	}
	row, err := s.q.UpsertQRCode(ctx, dbgen.UpsertQRCodeParams{
		ID: uuid.Must(uuid.NewV7()), LinkID: linkID, WorkspaceID: workspaceID,
		Slug: "", Label: "", Style: blob, IsDefault: true,
	})
	if err != nil {
		return qrRow{}, fmt.Errorf("upsert qr code: %w", err)
	}
	return qrRowFromUpsert(row), nil
}

// qrTargetRow resolves the row a write is about, writing the default code's row
// down if it has none.
//
// For the two writes that need a row to exist before they can happen — a rename,
// which is an UPDATE, and setting the flag, which is a flag on a row. Every other
// write goes through the upsert, which brings the row into being as part of
// writing what it was asked to write.
func (s *Service) qrTargetRow(
	ctx context.Context, workspaceID, linkID uuid.UUID, slug string,
) (qrRow, error) {
	row, found, err := s.storedQRCode(ctx, workspaceID, linkID, slug)
	if err != nil {
		return qrRow{}, err
	}
	if found {
		return row, nil
	}
	if slug != "" {
		return qrRow{}, domain.ErrNotFound
	}
	return s.materializeDefaultQRCode(ctx, workspaceID, linkID)
}

// qrLogoError turns internal/qr's refusals into the 422 a caller can act on.
//
// Every one of them is the caller's mistake — a format this product does not
// take, or an image larger than it will decode — so none of them is a 500. The
// field is `logo` in each case, because that is the multipart part the caller
// named, and the messages say what to do rather than what went wrong: an
// upload refused with "invalid image" is an upload somebody retries unchanged.
//
// **The size refusal names one bound and the measurement that crossed it**
// (F214). It used to name both caps and neither measurement, which for an
// 813×813 upload — inside the side cap, outside the area cap — said nothing the
// reader could act on. The area cap no longer refuses anything at all, so the
// only thing left to say is how wide or tall the file is and what the limit on
// that is; [qr.LogoBoundError] carries both, and the sentence adds that
// everything under it is resized rather than turned away.
func qrLogoError(err error) error {
	field := func(code, message string) error {
		return domain.ValidationErrors{{Field: "logo", Code: code, Message: message}}
	}
	switch {
	case errors.Is(err, qr.ErrLogoEmpty):
		return field("empty", "no image was uploaded")
	case errors.Is(err, qr.ErrLogoSVG):
		return field("unsupported_format",
			"an SVG is a document rather than an image — it can carry script and fetch "+
				"other files — so this product does not accept one; export the logo as a "+
				"PNG or a JPEG")
	case errors.Is(err, qr.ErrLogoFormat):
		return field("unsupported_format",
			"a logo is a PNG or a JPEG, decided by what is in the file rather than by "+
				"what it is called")
	case errors.Is(err, qr.ErrLogoTooLarge):
		// One shape reaches here, and it is the side bound NormalizeLogo raises
		// from the header. The other bound qr.LogoBoundError can carry — the area
		// one Code.prepareLogo raises over an already-stored row — never passes
		// through this function, which is called from SetQRCodeLogo and from
		// nowhere else. So a bound that is not "side" is exactly as unreachable as
		// no LogoBoundError at all, and both fall to the sentence below rather
		// than to a branch nothing can enter.
		var bound *qr.LogoBoundError
		if errors.As(err, &bound) && bound.Bound == "side" {
			side, which := bound.Width, "wide"
			if bound.Height > bound.Width {
				side, which = bound.Height, "tall"
			}
			return field("too_large", fmt.Sprintf(
				"this image is %d×%d and is %d pixels %s; a logo is at most %d pixels on "+
					"a side. Anything within that is accepted and resized down to fit if "+
					"it needs to be, so the size to aim for is only the limit itself",
				bound.Width, bound.Height, side, which, qr.MaxLogoDimension))
		}
		return field("too_large", fmt.Sprintf(
			"a logo is at most %d pixels on a side", qr.MaxLogoDimension))
	case errors.Is(err, qr.ErrLogoStoreTooLarge):
		return field("too_large", fmt.Sprintf(
			"this image re-encodes to more than the %d bytes a stored logo may occupy",
			qr.MaxLogoStoredBytes))
	case errors.Is(err, qr.ErrLogoUndecodable):
		return field("invalid",
			"the file begins like a PNG or a JPEG and then does not decode as one")
	default:
		return fmt.Errorf("normalize qr code logo: %w", err)
	}
}

// storedQRCode reads one code's row. Absent is not an error: for the default
// code it means the default style, and every caller that cares whether a named
// code exists reads the second return.
//
// **The empty slug still means "the link's default code", and that is the one
// seam this reopening needed** (D183). It used to be a column value and it is
// now a flag, so the empty string stopped being something to look up and became
// something to dispatch on — which keeps every caller that already passed it
// meaning what it always meant, at one branch here rather than at each of them.
// A caller holding a real slug never reaches the second query.
func (s *Service) storedQRCode(
	ctx context.Context, workspaceID, linkID uuid.UUID, slug string,
) (qrRow, bool, error) {
	if slug == "" {
		row, err := s.q.GetDefaultQRCode(ctx, dbgen.GetDefaultQRCodeParams{
			LinkID: linkID, WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return qrRow{}, false, nil
			}
			return qrRow{}, false, fmt.Errorf("get default qr code: %w", err)
		}
		return qrRowFromDefault(row), true, nil
	}
	row, err := s.q.GetQRCode(ctx, dbgen.GetQRCodeParams{
		LinkID: linkID, WorkspaceID: workspaceID, Slug: slug,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return qrRow{}, false, nil
		}
		return qrRow{}, false, fmt.Errorf("get qr code: %w", err)
	}
	return qrRowFromGet(row), true, nil
}

// decodeQRStyle reads a stored blob back into a style.
//
// Normalized on the way out as well as on the way in, so a row written before a
// field existed renders with that field's default rather than its zero value —
// and so the zero blob, which is how an unstored default code arrives, reads as
// the product default rather than as black on black.
func decodeQRStyle(blob []byte) qr.Style {
	var style qr.Style
	if len(blob) > 0 {
		if err := json.Unmarshal(blob, &style); err != nil {
			// A jsonb blob that is not a style is a row somebody wrote by hand.
			// Falling back to the default draws a working code instead of
			// failing a page, and the row is left alone rather than repaired
			// under a read.
			style = qr.Style{}
		}
	}
	normalized, _ := style.Normalize()
	return normalized
}

// QRContent is what one of a link's QR codes encodes: its short URL, carrying
// the source parameter that makes a scan tell the analytics what it is, and — for
// a named code — the slug that says which code it was (M50).
//
// Exported because two surfaces render a code — the API and the dashboard — and
// a second copy of this concatenation is a second answer to "what does the
// picture say". M50 gave that property a second job: the redirect path resolves
// the slug this function writes, so the encoded payload and the redirect's
// expectation cannot drift apart.
//
// **The empty slug adds nothing.** The default code's payload is byte for byte
// what every code this product drew before M50 carried, which is what makes an
// already-printed picture go on being counted as the same code it always was.
func QRContent(shortURL, slug string) string {
	if shortURL == "" {
		return ""
	}
	sep := "?"
	if u, err := url.Parse(shortURL); err == nil && u.RawQuery != "" {
		sep = "&"
	}
	out := shortURL + sep + domain.ClickSourceParam + "=" + domain.ClickSourceQR
	if slug != "" {
		out += "&" + domain.ClickCodeParam + "=" + slug
	}
	return out
}
