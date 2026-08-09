package httpx

import (
	"net/url"

	"github.com/DevOfPie/LinkCtrl/internal/dispute"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
)

// Where a notification leads (M48).
//
// A notification you cannot act on is buried in exactly the way a QR code eight
// screens down is, which is why this sits in the same milestone as the panels:
// the inbox has said "an automation rule fired" since M43 and left the reader to
// find the automation page themselves.
//
// **This package, rather than internal/notify.** The vocabulary is split across
// two packages — five kinds in notify, two in dispute — and dispute imports
// notify, so notify cannot see dispute's. httpx sees both, and it is also the
// only package that knows what the dashboard's paths are: a URL is this layer's
// noun, not the notifier's.

// notificationDestination answers where one kind of notification leads, given
// its data.
//
// A destination is a *dashboard* path, which is the surface the mapping is for.
// The empty string means this kind leads nowhere, and it is a real answer rather
// than a gap — see notificationTargets.
type notificationDestination func(data map[string]any) string

// notificationTargets is the enumeration, one entry per declared kind.
//
// A map rather than a switch, so that "has a mapping" is a question code can
// ask. TestEveryNotificationKindHasADestination reads the kind constants out of
// the source and looks each one up here, so a kind added in a later phase fails
// the build instead of silently becoming unclickable — which is the whole reason
// the mapping is enumerated rather than defaulted.
//
// **An entry returning "" is not a missing entry.** Two kinds genuinely lead
// nowhere, and saying so explicitly is what stops them being confused with a
// kind somebody forgot: the reader gets no open control at all rather than a
// link back to the list they are already reading.
var notificationTargets = map[string]notificationDestination{
	// Nowhere. The audit log has no dashboard page — it is an API surface and a
	// retention environment variable — so the only honest destination for "the
	// audit log has passed its size threshold" is none. The recipient is the
	// instance principal, and what they have to do about it is set
	// LINKCTRL_AUDIT_RETENTION_DAYS on the deployment, which no page offers.
	notify.KindAuditGrowth: func(map[string]any) string { return "" },

	// Nowhere, and for the same reason the audit-growth warning leads nowhere.
	// The recipient is the instance principal, and what they do about a new
	// release is pull an image and restart a service — there is no page in this
	// product that upgrades it, and there is not going to be one. Sending them to
	// a dashboard page would be pretending the product can act on its own
	// version.
	notify.KindUpdateAvailable: func(map[string]any) string { return "" },

	// The account page, which is where the second factor lives: the enrolment
	// offer, the recovery-code count, the regenerate control and the disable
	// form. Every one of the four changes this kind carries is answered there,
	// which is why there is one kind rather than four.
	notify.KindMFAChanged: func(map[string]any) string { return "/account" },

	// The invitation that was accepted, on the page that holds its lifecycle.
	// Not /members: the reader is the person who sent it, and what they are
	// being told is that the thing they created reached its end state.
	notify.KindInviteAccepted: func(map[string]any) string { return "/invites" },

	// The rule that fired. The page lists every rule with its last run, which is
	// what somebody clicking "Automation rule fired" is going to read next —
	// there is no per-rule page to reach, and inventing one is M43's decision to
	// revisit rather than this milestone's.
	notify.KindAutomationFired: func(map[string]any) string { return "/automation" },

	// The hostname that is failing, and the one that stopped being served. Both
	// land on the registered-hostnames page, where the verify button is. `data`
	// carries the hostname rather than the domain id, and the page has no
	// per-hostname anchor, so there is nothing narrower to aim at that would not
	// be a fabricated fragment.
	notify.KindDomainFailing:    func(map[string]any) string { return "/domains" },
	notify.KindDomainUnverified: func(map[string]any) string { return "/domains" },

	// The dispute, in the queue, scrolled to. The recipients are the instance's
	// reviewers, so the queue is a page they can open; `dispute_id` is what turns
	// "somebody is waiting" into the row they are waiting on.
	dispute.KindFiled: func(data map[string]any) string {
		return disputeAnchor(data, "/disputes")
	},

	// The outcome, and this is the one mapping that reads its data for more than
	// a fragment.
	//
	// The recipient is whoever *filed* the dispute, and they are an ordinary
	// account: /disputes needs destinations.review, which a filer has no reason
	// to hold, so sending them to the queue would be sending them to a refusal.
	// What an allowed dispute means for them is written in the notification's own
	// body — "you can create that link now" — and the links page is where they do
	// it. An upheld one means the destination still cannot be used here, and
	// there is nowhere that leads: no page shows a refusal that stands.
	dispute.KindDecided: func(data map[string]any) string {
		if s, _ := data["status"].(string); s == dispute.StatusAllowed {
			return "/links"
		}
		return ""
	},
}

// disputeAnchor points at one row of the review queue.
//
// The queue defaults to the waiting filter, and a dispute that has since been
// decided is not on it — so `all=1` travels with the fragment. Landing on the
// whole queue when the id is unreadable is the safe direction: the reader still
// arrives somewhere true.
//
// The id is escaped even though every writer of it is `uuid.String()`. `data` is
// a jsonb column, and a column is not a type — the day something writes a
// different shape into it, this must produce a broken link rather than a crafted
// one.
func disputeAnchor(data map[string]any, base string) string {
	id, _ := data["dispute_id"].(string)
	if id == "" {
		return base
	}
	return base + "?all=1#dispute-" + url.PathEscape(id)
}

// notificationTarget is where clicking a notification goes.
//
// One function from kind plus data to a URL, which is the shape m48.md asks for.
// It returns "" when there is nowhere to lead, and the surfaces that render a
// notification use that to decide whether to draw an open control at all.
//
// An **unknown** kind lands on the notifications list rather than erroring. That
// branch should be unreachable — the test above notificationTargets is what
// keeps it so — and it exists because the alternative for a kind added in a
// later phase, or read out of a row written by an older binary, is a redirect to
// nowhere or a 500 on a page that was working.
func notificationTarget(kind string, data map[string]any) string {
	to, ok := notificationTargets[kind]
	if !ok {
		return "/notifications"
	}
	return to(data)
}
