package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// DomainSettings is what an operator can configure about the hostname short
// links are served on.
type DomainSettings struct {
	Hostname string `json:"hostname"`
	// RootRedirectURL is where https://<link host>/ sends a visitor. Empty means
	// the root answers 404, which is the default and reveals nothing.
	RootRedirectURL string `json:"root_redirect_url,omitempty"`
	// SplitHosts reports whether the setting is in effect at all. On a
	// single-host deployment the root belongs to the dashboard.
	SplitHosts bool `json:"split_hosts"`
}

// DomainSettings reads the link domain's settings.
//
// Readable by anyone who can read links: it is one URL an operator chose, and
// every visitor to the bare domain sees where it points anyway.
func (s *Service) DomainSettings(ctx context.Context, actor *auth.Identity) (*DomainSettings, error) {
	if !actor.Can(PermRead) {
		return nil, fmt.Errorf("%w: reading domain settings requires %s", domain.ErrForbidden, PermRead)
	}
	row, err := s.q.GetDefaultDomainSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read domain settings: %w", err)
	}
	out := &DomainSettings{Hostname: row.Hostname, SplitHosts: s.splitHosts}
	if row.RootRedirectUrl != nil {
		out.RootRedirectURL = *row.RootRedirectUrl
	}
	return out, nil
}

// SetRootRedirect points the link domain's root somewhere, or clears it.
//
// Three refusals, each of which would otherwise be discovered late.
//
// It needs domains.write rather than links.update: this is not one link, it is
// where every visitor who trims a short link back to its domain ends up.
//
// It is refused outright on a single-host deployment. There "/" is the
// dashboard, and honouring this would take the dashboard away from the person
// who just set it — a failure that reads as the product breaking rather than as
// a setting doing what it says.
//
// The destination goes through exactly the same validation as a link's, which
// matters more here than anywhere: a root redirect that skipped the private,
// loopback and metadata refusals would be a cleaner SSRF than the one the
// validator exists to prevent, because reaching it needs no link and no alias.
func (s *Service) SetRootRedirect(ctx context.Context, actor *auth.Identity, rawURL string) (*DomainSettings, error) {
	if !actor.Can(PermDomainsWrite) {
		return nil, fmt.Errorf("%w: changing domain settings requires %s",
			domain.ErrForbidden, PermDomainsWrite)
	}
	if !s.splitHosts {
		return nil, domain.ValidationErrors{{
			Field: "root_redirect_url", Code: "not_applicable",
			Message: "the link domain root is the dashboard on a single-host deployment; " +
				"set LINK_BASE_URL to a separate host first",
		}}
	}

	var stored *string
	if trimmed := strings.TrimSpace(rawURL); trimmed != "" {
		normalized, err := ValidateDestination(trimmed, s.policy)
		if err != nil {
			var ve domain.ValidationErrors
			if errors.As(err, &ve) {
				// Reported against this field rather than "url", so the form
				// highlights the box the operator typed in.
				for i := range ve {
					ve[i].Field = "root_redirect_url"
				}
				return nil, ve
			}
			return nil, err
		}
		stored = &normalized
	}

	// Read before the write, because the audit record is worth little without
	// what it replaced: "the root now points at example.com" does not tell an
	// operator whether that was a change or a no-op, and the previous value is
	// unrecoverable a moment later.
	previous := ""
	if before, err := s.q.GetDefaultDomainSettings(ctx); err == nil && before.RootRedirectUrl != nil {
		previous = *before.RootRedirectUrl
	}

	row, err := s.q.SetDefaultDomainRootRedirect(ctx, stored)
	if err != nil {
		return nil, fmt.Errorf("set root redirect: %w", err)
	}

	// The audit event M20 promised. This is one setting that redirects every
	// stray visitor to the whole domain, which is the class of change worth
	// being able to ask about months later.
	//
	// After the write and outside it: the change is what the operator asked
	// for, and failing it because the record could not be written would trade a
	// missing audit line for a setting that did not take effect. Logged at warn
	// rather than swallowed, so the gap is visible to whoever goes looking.
	if s.audit != nil {
		to := ""
		if row.RootRedirectUrl != nil {
			to = *row.RootRedirectUrl
		}
		if err := s.audit.Record(ctx, actor, audit.Event{
			Action:     audit.ActionDomainRootRedirectChanged,
			TargetType: "domain",
			TargetID:   &row.ID,
			Metadata: map[string]any{
				"hostname": row.Hostname,
				"from":     previous,
				"to":       to,
			},
		}); err != nil {
			s.log.Warn("root redirect changed but the audit record was not written",
				slog.String("hostname", row.Hostname), slog.Any("error", err))
		}
	}

	// The redirect tree caches this; without invalidation the change waits out
	// the TTL on the one URL an operator is most likely to reload immediately
	// to check their work.
	if s.rootCache != nil {
		s.rootCache.InvalidateRoot()
	}

	out := &DomainSettings{Hostname: row.Hostname, SplitHosts: true}
	if row.RootRedirectUrl != nil {
		out.RootRedirectURL = *row.RootRedirectUrl
	}
	return out, nil
}

// LoadRootRedirect reads the current value for the redirect path. Unexported
// callers only: the hot path uses it through a cache.
func (s *Service) LoadRootRedirect(ctx context.Context) (string, error) {
	row, err := s.q.GetDefaultDomainSettings(ctx)
	if err != nil {
		return "", err
	}
	if row.RootRedirectUrl == nil {
		return "", nil
	}
	return *row.RootRedirectUrl, nil
}
