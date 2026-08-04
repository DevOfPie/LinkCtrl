package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Custom-domain verification warnings (M40).
//
// **Decision D70 turns on somebody being told.** A hostname that stops passing
// its DNS challenge keeps serving for a bounded grace window and then stops for
// good, and the only thing that makes that stop fair rather than arbitrary is
// that the workspace heard about it while there was still time to fix it. The
// window is short enough to state in a runbook precisely because the warning
// goes out at the first failure rather than at the last one.

const (
	// KindDomainFailing is the warning: this hostname has stopped verifying and
	// will stop being served at a stated time unless the record comes back.
	KindDomainFailing = "domain.failing"
	// KindDomainUnverified is the stop itself. A separate kind rather than a
	// second message under the first, because they are different facts and an
	// operator filtering their inbox for "what went dark" should not have to
	// read the bodies to tell them apart.
	KindDomainUnverified = "domain.unverified"
)

// **Exactly two messages per failing streak, and the caller is what bounds
// them.** internal/link warns on the *first* failure of a run and again at the
// stop, so an hourly check on a hostname whose record was deleted produces two
// notifications over the grace window rather than twenty-five. There is
// deliberately no rate limit here to enforce that: NotifiedSince is per user and
// per kind, so a limit would suppress a *second* domain's first warning, and the
// message that must never be deduplicated away is the one saying somebody's
// links are about to go dark.

// WarnDomainFailing tells a workspace's owners that its hostname is failing, and
// when serving stops if nothing changes.
//
// Addressed to the owners this hostname's workspace has, which is who OwnersOf
// answers with — a wider set than the audit-growth warning reaches, because that
// one belongs to no workspace.
// The notification carries the owning workspace so it appears in that
// workspace's inbox rather than wherever the reader happens to be standing.
//
// The deadline is in the body as a time and in the jsonb as a timestamp, because
// the sentence has to be readable now and the value has to be renderable later
// without parsing English back out of it.
func (s *Service) WarnDomainFailing(
	ctx context.Context, orgID uuid.UUID, workspaceID *uuid.UUID,
	hostname, reason string, stopsAt time.Time,
) error {
	return s.warnDomain(ctx, orgID, workspaceID, KindDomainFailing,
		hostname+" is failing verification",
		fmt.Sprintf("%s no longer passes its DNS check: %s. Links on it keep working "+
			"until %s, after which this instance stops serving the hostname until the "+
			"record is published again.",
			hostname, reason, stopsAt.UTC().Format(time.RFC1123)),
		map[string]any{
			"hostname": hostname,
			"reason":   reason,
			"stops_at": stopsAt.UTC().Format(time.RFC3339),
		})
}

// WarnDomainUnverified tells a workspace's owners that the hostname has stopped
// being served.
func (s *Service) WarnDomainUnverified(
	ctx context.Context, orgID uuid.UUID, workspaceID *uuid.UUID,
	hostname, reason string,
) error {
	return s.warnDomain(ctx, orgID, workspaceID, KindDomainUnverified,
		hostname+" has stopped being served",
		fmt.Sprintf("%s failed its DNS check for the whole grace window (%s), so this "+
			"instance no longer serves links on it and requests for the hostname get a "+
			"404. Publish the TXT record again and verify the domain to restore it.",
			hostname, reason),
		map[string]any{"hostname": hostname, "reason": reason})
}

// warnDomain is the shared delivery: the owners a domain in this workspace is
// news for.
//
// The workspace goes to OwnersOf rather than only onto the notification,
// because it decides *who hears* and not only where the row files itself. A
// hostname belongs to a workspace, so the organization's owners hear about it
// and so do that workspace's own owners — and an owner scoped to some other
// workspace does not, having no membership through which the hostname is
// theirs. A registration held at organization level passes nil and reaches the
// organization-wide owners alone.
//
// A failure to write one inbox row does not stop the others — an owner whose
// row failed has heard nothing, and the point of notifying several people is
// that one of them reads it.
func (s *Service) warnDomain(
	ctx context.Context, orgID uuid.UUID, workspaceID *uuid.UUID,
	kind, title, body string, data map[string]any,
) error {
	owners, err := s.OwnersOf(ctx, orgID, workspaceID)
	if err != nil {
		return fmt.Errorf("notify: owners of %s: %w", orgID, err)
	}
	var errs []error
	for _, owner := range owners {
		if err := s.Notify(ctx, owner.UserID, Event{
			Kind: kind, Title: title, Body: body,
			Data:        data,
			WorkspaceID: workspaceID,
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
