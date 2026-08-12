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
// **A link has codes, and one of them is the default.** The default is the code
// whose payload carries no code parameter — which is the payload of every code
// this product drew before M50 — so it is what every already-printed picture
// resolves to, and it exists for every link whether or not a row has ever been
// written for it. The rest are named: each carries a generated slug that travels
// in its payload and a label that never leaves the dashboard.
//
// **The single-code operations stayed as they were and now mean the default
// code.** m50.md required this choice be made and recorded, and the alternative —
// growing `GET /links/{id}/qr` an identifier — would have changed what a shipped
// endpoint answers for every client already calling it, which is exactly what
// the contract test exists to catch. Recorded in decisions.md under M50.

// QRCode is one of a link's codes: the style it is drawn with and the URL it
// encodes.
type QRCode struct {
	// ID is the stored row, and the zero uuid means there is no row — a default
	// code nobody has styled or named yet. It is what the per-code API paths
	// address, and it is absent from the JSON for an unstored code rather than
	// answering with a uuid nothing can be done with.
	ID     uuid.UUID `json:"id,omitempty"`
	LinkID uuid.UUID `json:"link_id"`
	// Slug is the identity that travels in the payload, and the empty string is
	// the default code (M50). It is generated, never chosen: it is printed, so a
	// workspace-supplied one would be a name somebody has to keep unique across
	// a link's codes and correct across every copy already in the world.
	Slug string `json:"slug"`
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
	ID      uuid.UUID
	Slug    string
	Label   string
	Style   []byte
	HasLogo bool
}

func qrRowFromGet(r dbgen.GetQRCodeRow) qrRow {
	return qrRow{ID: r.ID, Slug: r.Slug, Label: r.Label, Style: r.Style, HasLogo: r.HasLogo}
}

func qrRowFromList(r dbgen.ListQRCodesRow) qrRow {
	return qrRow{ID: r.ID, Slug: r.Slug, Label: r.Label, Style: r.Style, HasLogo: r.HasLogo}
}

func qrRowFromUpsert(r dbgen.UpsertQRCodeRow) qrRow {
	return qrRow{ID: r.ID, Slug: r.Slug, Label: r.Label, Style: r.Style, HasLogo: r.HasLogo}
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
// The empty slug is the default code and never 404s: a link that has never been
// styled or named still has one, drawn at the default style, which is what "a QR
// endpoint returns a code for any link" has meant since M41. Any other slug is a
// row that must exist, and its absence is a 404 rather than a default — a code
// somebody deleted must stop answering, or a printed identity would go on
// resolving after the workspace retired it.
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
	if len(rows) == 0 || rows[0].Slug != "" {
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
func (s *Service) CreateQRCode(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, label string,
) (*QRCode, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf(
			"%w: adding a QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return nil, err
	}
	label = strings.TrimSpace(label)
	if errs := domain.QRCodeLabelErrors(label); len(errs) > 0 {
		return nil, errs
	}

	count, err := s.q.CountQRCodes(ctx, dbgen.CountQRCodesParams{
		LinkID: linkID, WorkspaceID: actor.WorkspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("count qr codes for %s: %w", linkID, err)
	}
	// The default code counts against the cap whether or not it has a row, so
	// the check is against the number of codes the link *has* rather than the
	// number of rows it holds. Otherwise a link would be allowed one code more
	// than the cap for as long as its default went unstyled.
	held := count
	if _, defaulted, derr := s.storedQRCode(ctx, actor.WorkspaceID, linkID, ""); derr != nil {
		return nil, derr
	} else if !defaulted {
		held++
	}
	if held >= domain.MaxQRCodesPerLink {
		return nil, qrCapError()
	}

	// The style the link is already drawing at, so a second code looks like the
	// first one until somebody changes it.
	current, _, err := s.storedQRStyle(ctx, actor.WorkspaceID, linkID, "")
	if err != nil {
		return nil, err
	}
	blob, err := json.Marshal(current)
	if err != nil {
		return nil, fmt.Errorf("encode qr style: %w", err)
	}
	row, err := s.q.UpsertQRCode(ctx, dbgen.UpsertQRCodeParams{
		ID: uuid.Must(uuid.NewV7()), LinkID: linkID, WorkspaceID: actor.WorkspaceID,
		Slug: domain.NewQRCodeSlug(), Label: label, Style: blob,
	})
	if err != nil {
		return nil, fmt.Errorf("insert qr code: %w", err)
	}
	// The redirect snapshot carries this link's slugs, so a new one has to reach
	// every replica before a scan of it can be attributed. Until it does, a scan
	// resolves as the default code — the safe direction, and the same one a cold
	// cache falls in — but the window is a printed code that is not counted as
	// itself, so it is closed here rather than left to REDIRECT_TTL.
	s.invalidateQRLink(ctx, actor, linkID)
	code := qrCodeFrom(linkID, l.ShortURL, row.Slug, qrRowFromUpsert(row), true)
	return &code, nil
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

	row, found, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return nil, err
	}
	if !found {
		if slug != "" {
			return nil, domain.ErrNotFound
		}
		// Naming the default code is the first thing that makes a row for it,
		// which is the same trade styling one makes: a row appears when somebody
		// expresses a preference and not before.
		style, _, serr := s.storedQRStyle(ctx, actor.WorkspaceID, linkID, "")
		if serr != nil {
			return nil, serr
		}
		blob, merr := json.Marshal(style)
		if merr != nil {
			return nil, fmt.Errorf("encode qr style: %w", merr)
		}
		upserted, uerr := s.q.UpsertQRCode(ctx, dbgen.UpsertQRCodeParams{
			ID: uuid.Must(uuid.NewV7()), LinkID: linkID, WorkspaceID: actor.WorkspaceID,
			Slug: "", Label: label, Style: blob,
		})
		if uerr != nil {
			return nil, fmt.Errorf("insert qr code: %w", uerr)
		}
		code := qrCodeFrom(linkID, l.ShortURL, "", qrRowFromUpsert(upserted), true)
		return &code, nil
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

// DeleteQRCode removes a named code from a link.
//
// **The default code cannot be deleted**, and the refusal is a validation error
// rather than a 404: it is there, and what the caller almost certainly means —
// put it back to plain black on white — is ResetQRStyle. Deleting the code every
// already-printed picture resolves to would leave those pictures resolving to
// nothing, which is not something an interface should offer as a button.
//
// A deleted code's scans stop accumulating; they are not reassigned. A payload
// naming a slug that no longer exists is recorded as no code at all, which the
// analytics show as the default code's own bare `qr`, and the rows the deleted
// code already earned stay exactly where they are under its slug.
func (s *Service) DeleteQRCode(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string,
) error {
	if !actor.Can(PermUpdate) {
		return fmt.Errorf(
			"%w: removing a QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	if _, err := s.Get(ctx, actor, linkID); err != nil {
		return err
	}
	if slug == "" {
		return domain.ValidationErrors{{
			Field: "slug", Code: "invalid",
			Message: "the default code is what every already-printed picture of this link " +
				"resolves to, so it cannot be removed; reset its style instead",
		}}
	}
	row, found, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return err
	}
	if !found {
		return domain.ErrNotFound
	}
	if _, err := s.q.DeleteQRCodeByID(ctx, dbgen.DeleteQRCodeByIDParams{
		ID: row.ID, WorkspaceID: actor.WorkspaceID,
	}); err != nil {
		return fmt.Errorf("delete qr code %s: %w", row.ID, err)
	}
	// The other half of the create's reasoning, and the half that is visible in
	// the data: a replica still holding the deleted slug goes on attributing
	// that printed code to itself, so the row it stopped earning keeps growing
	// for up to REDIRECT_TTL after somebody removed it.
	s.invalidateQRLink(ctx, actor, linkID)
	return nil
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
func qrCodeFrom(
	linkID uuid.UUID, shortURL, slug string, row qrRow, found bool,
) QRCode {
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
	out := QRCode{
		LinkID: linkID, Slug: slug, Content: content,
		Style: style, Stored: found, Size: QROutputSize(content, style),
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
func QROutputSize(content string, style qr.Style) int {
	code, err := qr.Encode(content, style.Level)
	if err != nil {
		return 0
	}
	return qr.OutputSize(code.Size, style)
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
	logo, err := s.qrLogoFor(ctx, actor, linkID, slug, code.HasLogo)
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
	logo, err := s.qrLogoFor(ctx, actor, linkID, slug, code.HasLogo)
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
type QRSizeInput struct {
	Foreground string
	Background string
	// Size is the output size in pixels, before snapping.
	Size int
}

// SetQRSize stores a style described by its output size.
//
// The size is resolved against *this link's* module count, because that is what
// decides how many pixels a scale comes to. A link whose alias grows later keeps
// the margin and scale stored here and therefore draws slightly larger — which
// is the same behaviour every pre-M49 style has and is why the size is derived
// on read rather than written into the row.
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
			return nil, qr.SizeFit{}, domain.ValidationErrors{{
				Field: "style.size", Code: "out_of_range",
				Message: fmt.Sprintf("a code is %d to %d pixels across; %d is not a size "+
					"anything can be printed at", qr.MinSize, qr.MaxSize, in.Size),
			}}
		}
		return nil, qr.SizeFit{}, fmt.Errorf("fit qr size for %s: %w", linkID, err)
	}

	stored, err := s.SetQRStyleBySlug(ctx, actor, linkID, slug, qr.Style{
		Foreground: in.Foreground, Background: in.Background,
		Level: current.Level, Margin: fit.Margin, Scale: fit.Scale,
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
	existing, found, err := s.storedQRCode(ctx, actor.WorkspaceID, linkID, slug)
	if err != nil {
		return nil, err
	}
	if !found && slug != "" {
		return nil, domain.ErrNotFound
	}
	// **A code carrying a logo is stored at level H, whatever was asked for**
	// (M50.6, D141). Accept-and-override rather than refuse: this endpoint is a
	// PUT that replaces the style whole, so an omitted level means `M`, and
	// refusing would make every colour change on a logo'd code a 422 for a field
	// the caller never mentioned. What the milestone forbids is silence, and
	// there is none — the row holds H, the response below returns H, and a `GET`
	// after this `PUT` reports what was applied.
	if existing.HasLogo {
		normalized = normalized.ForLogo()
		if blob, err = json.Marshal(normalized); err != nil {
			return nil, fmt.Errorf("encode qr style: %w", err)
		}
	}
	row, err := s.q.UpsertQRCode(ctx, dbgen.UpsertQRCodeParams{
		ID: uuid.Must(uuid.NewV7()), LinkID: linkID, WorkspaceID: actor.WorkspaceID,
		Slug: slug, Label: existing.Label, Style: blob,
	})
	if err != nil {
		return nil, fmt.Errorf("upsert qr code: %w", err)
	}
	out := qrCodeFrom(linkID, l.ShortURL, slug, qrRowFromUpsert(row), true)
	out.Style = normalized
	return &out, nil
}

// ResetQRStyle returns a link's default code to the default style.
//
// Only the default code. A named code is removed rather than reset — it exists
// because somebody made it, so "put it back to how it was" is deleting it — and
// DeleteQRCode is that operation.
func (s *Service) ResetQRStyle(ctx context.Context, actor *auth.Identity, linkID uuid.UUID) error {
	if !actor.Can(PermUpdate) {
		return fmt.Errorf("%w: styling a QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	// Through Get, so a link in another workspace is a 404 rather than a silent
	// no-op that reports success.
	if _, err := s.Get(ctx, actor, linkID); err != nil {
		return err
	}
	if _, err := s.q.DeleteQRCode(ctx, dbgen.DeleteQRCodeParams{
		LinkID: linkID, WorkspaceID: actor.WorkspaceID,
	}); err != nil {
		return fmt.Errorf("delete qr code: %w", err)
	}
	// No error for a link that had no row. "Draw this at the default style" is
	// already true, and reporting 404 for it would make the operation care
	// whether a preference had ever been expressed.
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
// …/qr.png` and `GET …/qr/codes/{slug}/image.png` are one capability. The
// default code keeps its identity — the *absence* of a slug, which is D130 and
// the whole reason a printed picture goes on counting as what it always was —
// so nothing here gives it a reserved one.
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
// empty slug is the link's default code.
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
		Slug: slug, Label: row.Label, Style: blob,
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
// the target is clamped into the range qr.FitSize accepts, because a style
// stored before M49 can describe a picture outside it, and a fit that fails must
// not cost a logo that has already been written.
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
	want := min(max(qr.OutputSize(before.Size, style), qr.MinSize), qr.MaxSize)
	fit, err := qr.FitSize(after.Size, want)
	if err != nil {
		return out
	}
	out.Margin, out.Scale = fit.Margin, fit.Scale
	return out
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
func (s *Service) ClearQRCodeLogo(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, slug string,
) error {
	if !actor.Can(PermUpdate) {
		return fmt.Errorf(
			"%w: removing a QR code logo requires %s", domain.ErrForbidden, PermUpdate)
	}
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
	if _, err := s.q.ClearQRCodeLogo(ctx, dbgen.ClearQRCodeLogoParams{
		ID: row.ID, WorkspaceID: actor.WorkspaceID,
	}); err != nil {
		return fmt.Errorf("clear qr code logo %s: %w", row.ID, err)
	}
	return nil
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
	return s.qrLogo(ctx, actor.WorkspaceID, linkID, slug)
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
		Slug: "", Label: "", Style: blob,
	})
	if err != nil {
		return qrRow{}, fmt.Errorf("upsert qr code: %w", err)
	}
	return qrRowFromUpsert(row), nil
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
func (s *Service) storedQRCode(
	ctx context.Context, workspaceID, linkID uuid.UUID, slug string,
) (qrRow, bool, error) {
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

// storedQRStyle reads one code's style, falling back to the default.
func (s *Service) storedQRStyle(
	ctx context.Context, workspaceID, linkID uuid.UUID, slug string,
) (qr.Style, bool, error) {
	row, found, err := s.storedQRCode(ctx, workspaceID, linkID, slug)
	if err != nil {
		return qr.Style{}, false, err
	}
	return decodeQRStyle(row.Style), found, nil
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
