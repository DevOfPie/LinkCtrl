package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

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

// QRCode is a link's code: the style it is drawn with and the URL it encodes.
type QRCode struct {
	LinkID uuid.UUID `json:"link_id"`
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
}

// QRCode returns a link's code: its content and the style it is drawn with.
func (s *Service) QRCode(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID,
) (*QRCode, error) {
	if !actor.Can(PermRead) {
		return nil, domain.ErrForbidden
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return nil, err
	}
	style, stored, err := s.storedQRStyle(ctx, actor.WorkspaceID, linkID)
	if err != nil {
		return nil, err
	}
	content := QRContent(l.ShortURL)
	return &QRCode{
		LinkID:  linkID,
		Content: content,
		Style:   style,
		Stored:  stored,
		Size:    QROutputSize(content, style),
	}, nil
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

// RenderQR draws a link's code as SVG.
func (s *Service) RenderQR(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID,
) ([]byte, error) {
	code, err := s.QRCode(ctx, actor, linkID)
	if err != nil {
		return nil, err
	}
	svg, err := qr.Render(code.Content, code.Style)
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
	code, err := s.QRCode(ctx, actor, linkID)
	if err != nil {
		return nil, err
	}
	out, err := qr.RenderPNG(code.Content, code.Style)
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
	if !actor.Can(PermUpdate) {
		return nil, qr.SizeFit{}, fmt.Errorf(
			"%w: styling a QR code requires %s", domain.ErrForbidden, PermUpdate)
	}
	l, err := s.Get(ctx, actor, linkID)
	if err != nil {
		return nil, qr.SizeFit{}, err
	}
	current, _, err := s.storedQRStyle(ctx, actor.WorkspaceID, linkID)
	if err != nil {
		return nil, qr.SizeFit{}, err
	}

	code, err := qr.Encode(QRContent(l.ShortURL), current.Level)
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

	stored, err := s.SetQRStyle(ctx, actor, linkID, qr.Style{
		Foreground: in.Foreground, Background: in.Background,
		Level: current.Level, Margin: fit.Margin, Scale: fit.Scale,
	})
	if err != nil {
		return nil, qr.SizeFit{}, err
	}
	return stored, fit, nil
}

// SetQRStyle stores how a link's code is drawn.
func (s *Service) SetQRStyle(
	ctx context.Context, actor *auth.Identity, linkID uuid.UUID, style qr.Style,
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
	if _, err := s.q.UpsertQRCode(ctx, dbgen.UpsertQRCodeParams{
		ID: uuid.Must(uuid.NewV7()), LinkID: linkID,
		WorkspaceID: actor.WorkspaceID, Style: blob,
	}); err != nil {
		return nil, fmt.Errorf("upsert qr code: %w", err)
	}
	content := QRContent(l.ShortURL)
	return &QRCode{
		LinkID: linkID, Content: content, Style: normalized, Stored: true,
		Size: QROutputSize(content, normalized),
	}, nil
}

// ResetQRStyle returns a link's code to the default style.
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

// storedQRStyle reads a link's style, falling back to the default.
func (s *Service) storedQRStyle(
	ctx context.Context, workspaceID, linkID uuid.UUID,
) (qr.Style, bool, error) {
	row, err := s.q.GetQRCode(ctx, dbgen.GetQRCodeParams{LinkID: linkID, WorkspaceID: workspaceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			style, _ := qr.Style{}.Normalize()
			return style, false, nil
		}
		return qr.Style{}, false, fmt.Errorf("get qr code: %w", err)
	}
	var style qr.Style
	if len(row.Style) > 0 {
		if err := json.Unmarshal(row.Style, &style); err != nil {
			// A jsonb blob that is not a style is a row somebody wrote by hand.
			// Falling back to the default draws a working code instead of
			// failing a page, and the row is left alone rather than repaired
			// under a read.
			style = qr.Style{}
		}
	}
	// Normalized on the way out as well as on the way in, so a row written
	// before a field existed renders with that field's default rather than its
	// zero value.
	normalized, _ := style.Normalize()
	return normalized, true, nil
}

// QRContent is what a link's QR code encodes: its short URL, carrying the
// source parameter that makes a scan tell the analytics what it is.
//
// Exported because two surfaces render a code — the API and the dashboard — and
// a second copy of this concatenation is a second answer to "what does the
// picture say".
func QRContent(shortURL string) string {
	if shortURL == "" {
		return ""
	}
	sep := "?"
	if u, err := url.Parse(shortURL); err == nil && u.RawQuery != "" {
		sep = "&"
	}
	return shortURL + sep + domain.ClickSourceParam + "=" + domain.ClickSourceQR
}
