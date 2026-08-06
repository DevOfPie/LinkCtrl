package link

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/gate"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// The management half of the gates (M35). The redirect half is in
// internal/gate; what lives here is everything a signed-in person does *about* a
// gate rather than everything a visitor runs into.

// MaxSignatureTTL bounds how long a signed URL may be valid for.
//
// Thirty days, and the ceiling is the point rather than the number. A signature
// is a bearer capability: whoever holds the URL can follow the link, and nothing
// about the request identifies them. A signature that never expired would be a
// permanent secret pasted into whatever chat window it was shared in, which is
// the property the expiry exists to remove. An owner who wants a link that works
// forever already has one — an unsigned link.
const MaxSignatureTTL = 30 * 24 * time.Hour

// DefaultSignatureTTL is what a request that names no lifetime gets.
const DefaultSignatureTTL = 24 * time.Hour

// hashLinkPassword turns a link password into an argon2id hash.
//
// The same hasher, the same parameters and the same floor as an account
// password. Reusing MinPasswordLength rather than inventing a shorter rule for
// links is deliberate: this hash sits in a row that an operator's database dump
// carries, and a rule that said "twelve for your account, six for your links"
// would be a claim that the second one matters less to whoever guesses it.
func (s *Service) hashLinkPassword(password string) (string, error) {
	if s.hasher == nil {
		return "", errors.New("link: no password hasher configured")
	}
	if len(password) < auth.MinPasswordLength {
		return "", domain.ValidationErrors{{
			Field: "password", Code: "too_short",
			Message: fmt.Sprintf("a link password must be at least %d characters",
				auth.MinPasswordLength),
		}}
	}
	if len(password) > auth.MaxPasswordLength {
		return "", domain.ValidationErrors{{
			Field: "password", Code: "too_long",
			Message: fmt.Sprintf("a link password must be at most %d bytes",
				auth.MaxPasswordLength),
		}}
	}
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return "", fmt.Errorf("hash link password: %w", err)
	}
	return hash, nil
}

// validateClickLimit refuses a ceiling that cannot be honoured.
//
// Zero and negative are refused rather than stored, because a link nobody may
// ever follow is indistinguishable from a link somebody meant to delete, and
// storing it would leave a 410 whose cause is a number in a column nobody looks
// at.
func validateClickLimit(maxClicks *int64) domain.ValidationErrors {
	if maxClicks == nil || *maxClicks >= 1 {
		return nil
	}
	return domain.ValidationErrors{{
		Field: "max_clicks", Code: "out_of_range",
		Message: "max_clicks must be at least 1; to stop a link entirely, archive it",
	}}
}

// SignedLink is a minted signed URL and when it stops working.
type SignedLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Sign mints a signed URL for a link.
//
// Guarded by links.update rather than links.read, and the choice is not
// cosmetic: a signature is the thing that makes a gated link followable, so
// issuing one is handing out access. Somebody who may only read the catalogue
// must not be able to mint a capability for a link they cannot otherwise open.
//
// The workspace secret is minted on first use here, which is why this is the
// only place that can create one — a signature nobody asked for should not
// bring a key into existence.
func (s *Service) Sign(ctx context.Context, actor *auth.Identity, id uuid.UUID, ttl time.Duration) (*SignedLink, error) {
	if !actor.Can(PermUpdate) {
		return nil, fmt.Errorf("%w: signing a link requires %s", domain.ErrForbidden, PermUpdate)
	}
	if s.gates == nil {
		return nil, errors.New("link: signing is not available on this instance")
	}
	switch {
	case ttl <= 0:
		ttl = DefaultSignatureTTL
	case ttl > MaxSignatureTTL:
		return nil, domain.ValidationErrors{{
			Field: "ttl_seconds", Code: "out_of_range",
			Message: fmt.Sprintf("a signature may be valid for at most %d seconds",
				int64(MaxSignatureTTL.Seconds())),
		}}
	}

	row, err := s.q.GetLink(ctx, dbgen.GetLinkParams{ID: id, WorkspaceID: actor.WorkspaceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("load link: %w", err)
	}

	secret, err := s.gates.EnsureSecret(ctx, actor.WorkspaceID)
	if err != nil {
		return nil, err
	}

	expires := time.Now().Add(ttl).UTC().Truncate(time.Second)
	params, err := gate.Sign(secret, row.DomainID, row.Alias, expires)
	if err != nil {
		return nil, err
	}

	// Built on the hostname this link is actually published under — the same
	// helper `short_url` is built with, so a signed URL and an unsigned one
	// really do differ only by the query. The comment here used to say that
	// while the code concatenated `s.baseURL`, which is the instance's own host
	// and not the link's: alias uniqueness is `(domain_id, alias)` and the
	// default domain is shared across workspaces, so a signed URL for a link on
	// go.acme.com did not merely 404 — where that alias also exists on the
	// default domain it resolved a **different workspace's** link and sent the
	// recipient to a stranger's destination.
	//
	// Strict, because this is the one call site where the fallback is the bug
	// again. A hostname lookup that cannot answer must not quietly produce the
	// instance's own host on a capability addressed to somebody.
	base, err := s.shortURLStrict(ctx, row.DomainID, row.Alias)
	if err != nil {
		return nil, fmt.Errorf("assemble signed url: %w", err)
	}
	signed := base + "?" + params.Encode()
	if _, perr := url.Parse(signed); perr != nil {
		return nil, fmt.Errorf("assemble signed url: %w", perr)
	}
	return &SignedLink{URL: signed, ExpiresAt: expires}, nil
}

// withBudget fills in the exact click count for a gated link.
//
// Only for a single link, never for a page of them: it is one query per link,
// and a list of twenty-five would be twenty-five round trips for a number most
// of those links do not have. A read failure leaves the field absent rather than
// failing the request — the budget is a detail beside the link, not the link.
func (s *Service) withBudget(ctx context.Context, l *domain.Link) *domain.Link {
	if s.gates == nil || l == nil || (!l.OneTime && l.MaxClicks == nil) {
		return l
	}
	consumed, _, err := s.gates.Budget(ctx, l.ID)
	if err != nil {
		return l
	}
	l.ClicksConsumed = &consumed
	return l
}
