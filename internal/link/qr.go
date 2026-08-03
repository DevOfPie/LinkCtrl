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
	return &QRCode{
		LinkID:  linkID,
		Content: QRContent(l.ShortURL),
		Style:   style,
		Stored:  stored,
	}, nil
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
	return &QRCode{
		LinkID: linkID, Content: QRContent(l.ShortURL), Style: normalized, Stored: true,
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
