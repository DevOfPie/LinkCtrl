package main

import (
	"net/netip"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/httpx"
)

// clickRecorder adapts the redirect handler's event to the ingester's.
//
// It lives in the composition root rather than in either package because it is
// pure wiring, and putting it in analytics would make that package import
// httpx while httpx already imports analytics for the reader — a cycle.
//
// The handler depends only on a one-method interface, so the recorder can be
// swapped for a broker-backed one in Phase 2 without touching the hot path.
type clickRecorder struct {
	ingester *analytics.Ingester
}

func (c clickRecorder) Record(ev httpx.ClickEvent) {
	// The address arrives as a string from the handler and is parsed here.
	// It is consumed to derive the visitor hash and never stored.
	addr, _ := netip.ParseAddr(ev.IP)
	c.ingester.Record(analytics.Event{
		LinkID:      ev.LinkID,
		WorkspaceID: ev.WorkspaceID,
		OccurredAt:  ev.OccurredAt,
		IP:          addr,
		UserAgent:   ev.UserAgent,
		Referrer:    ev.Referrer,
		Language:    ev.Language,
		LatencyUS:   ev.LatencyUS,
		// Carried through rather than re-derived. The handler read it off the
		// link's own rules; the pipeline has no way to know and must not acquire
		// one, because acquiring one means a query per batch (M34).
		TrackReturning: ev.TrackReturning,
		// Which arm of a split test served this click (M36). Zero for every link
		// that has none, which the ingester writes as NULL.
		DestinationID: ev.DestinationID,
	})
}
