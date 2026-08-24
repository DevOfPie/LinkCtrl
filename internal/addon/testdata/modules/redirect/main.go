//go:build wasip1

// Command redirect is the redirect fixture (M66): a module that runs in both
// redirect classes and tries, from the guest side, everything the host has to
// refuse it there.
//
// It exists for the reason the `pages` fixture exists. M66's claims are about a
// boundary, and a boundary tested only from the host's side is a boundary tested
// against the host's own idea of what a guest would send. This module is a real
// consumer of the generated SDK, compiled the way a published add-on is
// compiled, and the statuses it reports are the ones an add-on in another
// repository would get.
//
// The exported names are the literals `linkctrl_redirect_inline` and
// `linkctrl_redirect_observe`: a //go:wasmexport directive cannot take a
// constant, so the host looks up abi.GuestRedirectInline and
// abi.GuestRedirectObserve and this file writes the strings, and the test that
// loads this module is what proves the two agree.
//
// **What it does is decided by the alias**, so one fixture and one manifest
// serve every case the host-side test drives:
//
//	veto      — answers veto
//	strip     — rewrites the query, dropping every fbclid and utm_ parameter
//	drop      — rewrites the query to nothing at all
//	badquery  — writes a query with a `#` in it, which the host must refuse
//	remember  — reports what the last invocation of this instance left behind
//	twice     — answers twice, and the second must be refused
//	verdict   — writes a verdict outside the vocabulary
//	anything  — allows, unchanged
package main

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/DevOfPie/LinkCtrl/sdk"
)

// decision and answer are the RedirectDecision and RedirectAnswer records,
// guest-side. Written out here rather than shared, because that is a publisher's
// position: the ABI's records are a documented JSON shape, not a Go type another
// repository can import.
type decision struct {
	LinkID      string `json:"link_id"`
	WorkspaceID string `json:"workspace_id"`
	Alias       string `json:"alias"`
	Destination string `json:"destination"`
}

type answer struct {
	Verdict string `json:"verdict,omitempty"`
	Rewrite bool   `json:"rewrite,omitempty"`
	Query   string `json:"query,omitempty"`
}

type event struct {
	LinkID       string `json:"link_id"`
	WorkspaceID  string `json:"workspace_id"`
	OccurredAt   string `json:"occurred_at"`
	VisitorHash  string `json:"visitor_hash"`
	IsFirstVisit bool   `json:"is_first_visit"`
	Country      string `json:"country"`
	Device       string `json:"device"`
	Browser      string `json:"browser"`
	OS           string `json:"os"`
	Language     string `json:"language"`
	ReferrerHost string `json:"referrer_host"`
	IsBot        bool   `json:"is_bot"`
}

// remembered is guest state that outlives one invocation, and it exists for the
// reuse test: a pooled instance that did not reset its guest hands the next
// redirect whatever this holds.
var remembered string

func report(check, outcome string) { _ = sdk.Log(sdk.LevelInfo, "redirect: "+check+"="+outcome) }

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// status names the ABI status an error carries, so the host-side test asserts on
// a word rather than on a sentence the SDK happens to spell a particular way.
func status(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, sdk.ErrDenied):
		return "denied"
	case errors.Is(err, sdk.ErrNotFound):
		return "not_found"
	case errors.Is(err, sdk.ErrInvalid):
		return "invalid"
	case errors.Is(err, sdk.ErrInternal):
		return "internal"
	case errors.Is(err, sdk.ErrNotAvailable):
		return "not_available"
	default:
		return "other:" + errText(err)
	}
}

// Package initialization, which runs *during* instantiation — and for a redirect
// instance that means it runs **inside** the invocation, because the host attaches
// the subject to the state before it instantiates.
//
// So the answer here depends on which instance is initializing, and reporting it
// rather than asserting it is the point: the load-time instance has nothing to
// read and a per-invocation one already has its subject. That is the same shape
// the routes limb has, and it is why *outside a request* is a state rather than a
// flag — an instance made for nothing cannot be talked into believing otherwise.
//
// **It reads and never writes.** An answer written from init would be this
// module's one answer, and the invocation's own would then be refused as a second
// one — which is correct behaviour and would make every case below untestable.
func init() {
	_, err := sdk.RedirectDecisionRead()
	report("init_decision_read", status(err))
	_, err = sdk.RedirectEventRead()
	report("init_event_read", status(err))
}

//go:wasmexport linkctrl_redirect_inline
func inline() int32 {
	raw, err := sdk.RedirectDecisionRead()
	if err != nil {
		report("decision_read", status(err))
		return -1
	}
	var d decision
	if err := json.Unmarshal(raw, &d); err != nil {
		report("decision_parse", errText(err))
		return -1
	}
	report("alias", d.Alias)
	report("destination", d.Destination)

	// The redirect-safe subset, probed from inside an inline invocation. Each of
	// these is a capability this add-on's manifest **declares** and the host
	// refuses here anyway, because an inline invocation is holding a visitor's
	// redirect open. They are called rather than asserted about, because a status
	// a guest received is a fact only the guest can report.
	_, err = sdk.StorageQuery("select 1", nil)
	report("inline_storage_query", status(err))
	report("inline_storage_exec", status(sdk.StorageExec("select 1", nil)))
	_, err = sdk.SessionContextRead()
	report("inline_session_context", status(err))
	_, err = sdk.HTTPRequestRead()
	report("inline_http_request", status(err))
	_, err = sdk.RedirectEventRead()
	report("inline_event_read", status(err))
	// And the other half: what an inline invocation *may* call still works, so
	// the refusals above are about the subset rather than about a broken instance.
	if _, err := sdk.HostABIVersion(); err != nil {
		report("inline_abi_version", status(err))
	}
	if _, err := sdk.ConfigGet("retention_days"); err != nil {
		report("inline_config_get", status(err))
	}

	switch d.Alias {
	case "veto":
		report("answer_veto", status(write(answer{Verdict: "veto"})))
	case "strip":
		report("answer_strip", status(write(answer{Rewrite: true, Query: stripped(d.Destination)})))
	case "drop":
		report("answer_drop", status(write(answer{Rewrite: true})))
	case "badquery":
		report("answer_badquery", status(write(answer{Rewrite: true, Query: "a=1#b"})))
	case "verdict":
		report("answer_bad_verdict", status(write(answer{Verdict: "maybe"})))
	case "remember":
		// The pooling fixture (M66.5). It reports what the *previous* invocation of
		// this same instance left in a package-level variable, then leaves its own
		// destination there for the next one. On a fresh instance, and on a pooled
		// one whose guest memory was reset, the report is empty; on a pooled one
		// that was handed back dirty, it is the last visitor's destination.
		if remembered == "" {
			report("remembered", "<none>")
		} else {
			report("remembered", remembered)
		}
		remembered = d.Destination
		report("answer_allow", "silent")
	case "twice":
		report("answer_first", status(write(answer{})))
		report("answer_second", status(write(answer{Verdict: "veto"})))
	default:
		report("answer_allow", "silent")
	}
	return 0
}

//go:wasmexport linkctrl_redirect_observe
func observe() int32 {
	raw, err := sdk.RedirectEventRead()
	if err != nil {
		report("event_read", status(err))
		return -1
	}
	var e event
	if err := json.Unmarshal(raw, &e); err != nil {
		report("event_parse", errText(err))
		return -1
	}
	report("observed_link", e.LinkID)
	report("observed_country", e.Country)
	report("observed_browser", e.Browser)
	report("observed_bot", boolWord(e.IsBot))
	// The other class's payload, from this one. What comes back depends on what
	// this add-on's manifest declared, and both answers are right: **denied** when
	// it did not declare `redirect.inline`, because the permission check comes
	// first and a module that declared nothing must not learn what a host
	// implements; **not found** when it did, because an observing invocation is off
	// the path and there is simply no decision — this redirect was answered before
	// this module was called. Reported rather than asserted, so the host-side test
	// says which manifest it installed.
	_, err = sdk.RedirectDecisionRead()
	report("observe_decision_read", status(err))
	report("observe_answer_write", status(sdk.RedirectAnswerWrite([]byte(`{}`))))
	// Storage is the difference between the two classes, stated as a call: an
	// observing add-on writing to the schema it owns is the whole point of the
	// class, and this host has no database, so the answer is ErrInternal — the
	// status that means the host failed at something that is not the module's
	// fault — rather than the ErrDenied the same call gets inline.
	_, err = sdk.StorageQuery("select 1", nil)
	report("observe_storage_query", status(err))
	return 0
}

func write(a answer) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return sdk.RedirectAnswerWrite(raw)
}

// stripped removes the tracking parameters from a destination's query, which is
// the case D317 moved the bound from *nothing* to *the query* for.
//
// Written the long way — split, filter, join — rather than with net/url, because
// what a publisher does here is their business and the fixture should exercise
// the host's bound rather than the standard library's escaping.
func stripped(destination string) string {
	_, query, ok := strings.Cut(destination, "?")
	if !ok {
		return ""
	}
	var kept []string
	for _, pair := range strings.Split(query, "&") {
		name, _, _ := strings.Cut(pair, "=")
		if name == "fbclid" || name == "gclid" || strings.HasPrefix(name, "utm_") {
			continue
		}
		if pair != "" {
			kept = append(kept, pair)
		}
	}
	return strings.Join(kept, "&")
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func main() {}
