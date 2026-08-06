package link

import (
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// TestUnappealableTierHasNoOverrideSwitch is the milestone's central claim, and
// it is written to fail when somebody *adds* the switch back rather than only
// when they flip one.
//
// The reflection walk is the load-bearing part. Asserting that private addresses
// are refused under the default policy would pass just as happily with a
// BlockPrivateIPs field sitting there set to true, which is exactly the state
// this test exists to prevent — so instead it enumerates every field of
// DestinationPolicy, refuses to run against a field it has not been taught
// about, and sets the ones it knows to the most permissive value each can hold.
// If the struct grows a knob, this fails and somebody has to say out loud why a
// tier documented as unappealable now has one.
func TestUnappealableTierHasNoOverrideSwitch(t *testing.T) {
	// The most permissive policy that can be expressed at all.
	p := DestinationPolicy{}
	for i := 0; i < reflect.TypeOf(p).NumField(); i++ {
		f := reflect.TypeOf(p).Field(i)
		switch f.Name {
		case "Schemes":
			// Everything config validation would refuse, and more. The point is
			// that even a policy nothing could produce cannot loosen the tier.
			p.Schemes = []string{"http", "https", "javascript", "data", "file"}
		case "MaxLength":
			p.MaxLength = 1 << 20
		default:
			t.Fatalf("DestinationPolicy grew field %q. Prove it cannot loosen the "+
				"unappealable tier, then teach this test its most permissive value.", f.Name)
		}
	}

	// Every address the unappealable tier exists for, plus the spellings a
	// browser resolves and netip.ParseAddr does not.
	unappealable := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://0xa9fea9fe/latest/meta-data/",
		"http://2852039166/",
		"http://10.0.0.1/admin",
		"http://192.168.1.1/",
		"http://127.0.0.1:8080/",
		"http://2130706433/",
		"http://localhost:3000/",
		"http://app.localhost/",
		"http://[::1]/",
		"http://[::ffff:169.254.169.254]/",
		"http://100.64.0.1/",
	}
	for _, raw := range unappealable {
		t.Run(raw, func(t *testing.T) {
			_, err := ValidateDestination(raw, p)
			if err == nil {
				t.Fatalf("accepted %q under the most permissive policy expressible", raw)
			}
			if got := codesOf(t, err); !strings.HasPrefix(got, string(TierUnappealable)+".") {
				t.Errorf("refused %q with code %q, want an %s.* code: the tier a "+
					"refusal reports is what tells a caller whether an appeal exists",
					raw, got, TierUnappealable)
			}
		})
	}
}

// The same claim, asked the way it was actually broken (F26).
//
// TestUnappealableTierHasNoOverrideSwitch above walks DestinationPolicy by
// reflection looking for a *field*, and never tries a *host*. That is why it
// stayed green while `http://169.254.169.254./` was answered 201 on a live
// instance: a promise about what could be accepted was guarded by a check on
// struct shape, and one character that no field controls walked past it. This
// test feeds hosts.
//
// The class was already known here. TestHostCandidatesRespectLabelBoundaries
// calls a trailing dot "a one-character bypass" and has asserted the Postgres
// tier against it since M30 shipped; the write side normalized and the read side
// did not.
func TestATrailingDotDoesNotDefeatAnyTier(t *testing.T) {
	p := DefaultDestinationPolicy()

	// Each dotted spelling against its dotless control. Asserting only that the
	// dotted form is refused would pass just as happily on a fix that refuses
	// every dotted host — which destination_test.go says is wrong, because a
	// trailing dot is a fully qualified name and not a malformed one.
	for _, tc := range []struct{ dotted, plain string }{
		{"http://169.254.169.254./latest/meta-data/", "http://169.254.169.254/latest/meta-data/"},
		{"http://127.0.0.1.:8080/", "http://127.0.0.1:8080/"},
		{"http://10.0.0.1./admin", "http://10.0.0.1/admin"},
		// The obfuscated forms, where the empty last label was read as evidence
		// that the host was a name rather than a number.
		{"http://2130706433./", "http://2130706433/"},
		{"http://0x7f000001./", "http://0x7f000001/"},
		// Two dots, because trimming exactly one leaves the same empty label.
		{"http://127.0.0.1../", "http://127.0.0.1/"},
		// Not an address at all, and refused by an equality test that a dot
		// defeats without any parser being involved.
		{"http://localhost./", "http://localhost/"},
		{"http://app.localhost./", "http://app.localhost/"},
	} {
		t.Run(tc.dotted, func(t *testing.T) {
			_, err := ValidateDestination(tc.dotted, p)
			if err == nil {
				t.Fatalf("accepted %q; %q is refused, and they are the same host",
					tc.dotted, tc.plain)
			}
			_, plainErr := ValidateDestination(tc.plain, p)
			if plainErr == nil {
				t.Fatalf("the control %q was accepted; this test proves nothing", tc.plain)
			}
			if got, want := codesOf(t, err), codesOf(t, plainErr); got != want {
				t.Errorf("refused %q with code %q and %q with %q; the same host must "+
					"reach the same tier however it is spelled", tc.dotted, got, tc.plain, want)
			}
		})
	}

	// The high-confidence tier, which is an exact-match map and was defeated
	// identically. It is reached the way Judge reaches it — off the URL the
	// validator returned — because folding the host is the validator's job and a
	// tier that normalized for itself is the shape this finding is about.
	const listed = "metadata.google.internal"
	if _, ok := embeddedHosts[listed]; !ok {
		t.Skipf("%s is not on the list; nothing to assert about matching it", listed)
	}
	_, host := parseForTest(t, "https://"+listed+"./computeMetadata/v1/")
	if host != listed {
		t.Fatalf("the validator produced host %q, want %q: every tier below reads "+
			"this value, so a dot surviving here is a dot surviving all of them",
			host, listed)
	}
	if highConfidence(host) == nil {
		t.Errorf("%q was accepted by the embedded tier; that host is on the list, "+
			"and the list is meant to cost a rebuild to overrule, not one keystroke",
			listed+".")
	}
	// And the runtime list, which trims for itself, is asked about the same
	// candidates it would have been asked for the dotless spelling.
	if !reflect.DeepEqual(HostCandidates(host), HostCandidates(listed)) {
		t.Errorf("candidates for %q differ from %q", host, listed)
	}

	// Canonicalized, never refused: the accepted case, and what it is stored as.
	// The stored form is the point — it is what a visitor's browser is sent to
	// and what every tier judged, so the dot has to be gone from it and not
	// merely ignored while checking.
	got, err := ValidateDestination("https://example.com./", p)
	if err != nil {
		t.Fatalf("rejected https://example.com./: %v. A trailing dot is a fully "+
			"qualified name; refusing it would break a passing test for the wrong "+
			"reason", err)
	}
	if got != "https://example.com/" {
		t.Errorf("normalized to %q, want %q: the dot is folded away on the way in, "+
			"so what is stored and served is what the tiers were shown", got,
			"https://example.com/")
	}
}

// The same claim again, in a different alphabet (F77).
//
// TestATrailingDotDoesNotDefeatAnyTier above feeds hosts, which is what the
// struct-shape walk could not do, and it is still not enough: every host it feeds
// is ASCII. A host written outside ASCII reached none of these checks — U+3002 is
// not the dot looksNumeric splits on, fullwidth Latin is not equal to
// "localhost", "metadata。google。internal" is not the map key, and isHomograph
// returns early unless a label already starts "xn--". Five spellings were
// accepted and stored on a live instance while the ASCII control was refused.
//
// Each spelling is paired with its ASCII control for the reason the dot test
// pairs its own: asserting only that the Unicode form is refused would pass on a
// fix that refuses every non-ASCII host, and that fix kills müller.de, which the
// accepted half below exists to prevent.
func TestAUnicodeSpellingDoesNotDefeatAnyTier(t *testing.T) {
	p := DefaultDestinationPolicy()

	for _, tc := range []struct{ spelled, plain string }{
		// The three separators that are not U+002E.
		{"http://169。254。169。254/latest/meta-data/", "http://169.254.169.254/latest/meta-data/"},
		{"http://169．254．169．254/", "http://169.254.169.254/"},
		{"http://169｡254｡169｡254/", "http://169.254.169.254/"},
		// And the spellings that carry no separator at all, which is why a
		// hand-written map of the three above would have read as a fix without
		// being one.
		{"http://１６９.２５４.１６９.２５４/", "http://169.254.169.254/"},
		{"http://①⑥⑨。２５４。１６９。２５４/", "http://169.254.169.254/"},
		{"http://ｌｏｃａｌｈｏｓｔ/", "http://localhost/"},
		{"http://ａｐｐ。ｌｏｃａｌｈｏｓｔ/", "http://app.localhost/"},
		{"http://127。0。0。1/admin", "http://127.0.0.1/admin"},
		// The obfuscated numeric forms, in the alphabet that hid them from the
		// last-label scan.
		{"http://０x7f000001/", "http://0x7f000001/"},
		{"http://２１３０７０６４３３/", "http://2130706433/"},
		// A separator that is also the trailing label, so the fold has to run
		// before the dot is trimmed and not after.
		{"http://169。254。169。254。/", "http://169.254.169.254/"},
	} {
		t.Run(tc.spelled, func(t *testing.T) {
			_, err := ValidateDestination(tc.spelled, p)
			if err == nil {
				t.Fatalf("accepted %q; %q is refused, and a browser resolves them "+
					"to the same host", tc.spelled, tc.plain)
			}
			_, plainErr := ValidateDestination(tc.plain, p)
			if plainErr == nil {
				t.Fatalf("the control %q was accepted; this test proves nothing", tc.plain)
			}
			if got, want := codesOf(t, err), codesOf(t, plainErr); got != want {
				t.Errorf("refused %q with code %q and %q with %q; the same host must "+
					"reach the same tier however it is spelled", tc.spelled, got, tc.plain, want)
			}
		})
	}

	// The high-confidence tier, reached the way Judge reaches it — off the URL the
	// validator returned — because folding is the validator's job and a tier that
	// normalized for itself is the shape both of this milestone's reopenings are
	// about.
	const listed = "metadata.google.internal"
	if _, ok := embeddedHosts[listed]; !ok {
		t.Skipf("%s is not on the list; nothing to assert about matching it", listed)
	}
	_, host := parseForTest(t, "https://metadata。google。internal/computeMetadata/v1/")
	if host != listed {
		t.Fatalf("the validator produced host %q, want %q: every tier below reads "+
			"this value, so a separator surviving here survives all of them", host, listed)
	}
	if highConfidence(host) == nil {
		t.Errorf("the U+3002 spelling was accepted by the embedded tier; that host " +
			"is on the list, and the list is meant to cost a rebuild to overrule")
	}
	if !reflect.DeepEqual(HostCandidates(host), HostCandidates(listed)) {
		t.Errorf("candidates for %q differ from %q", host, listed)
	}

	// The homograph tier, which is the one built for exactly this attack and which
	// had never been shown one. Its prefix test reads "xn--", so until the host was
	// converted a raw Cyrillic spelling walked past the check written for it.
	u, host := parseForTest(t, "https://аpple.com/")
	if host != "xn--pple-43d.com" {
		t.Fatalf("аpple.com folded to %q, want the punycode form: isHomograph reads "+
			"an xn-- prefix and sees nothing without it", host)
	}
	if b := lowConfidenceHeuristics(u, host); b == nil || b.Rule != RulePunycodeHomograph {
		t.Errorf("a Cyrillic imitation of apple.com was not caught by the homograph " +
			"heuristic; that check exists for this input and had never received it")
	}

	// Canonicalized, never refused. An internationalized name is an ordinary name,
	// and what is stored is the ToASCII form — so the host a visitor's browser
	// resolves is the host the tiers judged, and nothing but the host moved.
	for raw, want := range map[string]string{
		"https://müller.de/":       "https://xn--mller-kva.de/",
		"https://テスト.example/path": "https://xn--zckzah.example/path",
		// Only the host moves. The path is escaped and the query is left exactly
		// as url.URL.String() has always rendered them, which is the evidence
		// that the fold is a host fold and not a rewrite of the destination.
		"https://müller.de/café?q=ü": "https://xn--mller-kva.de/caf%C3%A9?q=ü",
		"https://example.com/":       "https://example.com/",
	} {
		got, err := ValidateDestination(raw, p)
		if err != nil {
			t.Errorf("rejected %q: %v. Refusing non-ASCII hosts closes the hole by "+
				"breaking the product", raw, err)
			continue
		}
		if got != want {
			t.Errorf("normalized %q to %q, want %q", raw, got, want)
		}
	}

	// The profile is WHATWG's, not idna.Lookup's, and that is a choice a future
	// reader could undo without noticing. These three are refused by Lookup — it
	// sets UseSTD3ASCIIRules and CheckHyphens — and are accepted here, so swapping
	// the profile turns this red instead of turning real destinations away.
	for _, raw := range []string{
		"https://my_host.example/",
		"https://under_score.example.com/",
		"https://r3---sn-apo3qvuoxuxbt-j5pe.googlevideo.com/videoplayback",
	} {
		if _, err := ValidateDestination(raw, p); err != nil {
			t.Errorf("rejected %q: %v. A canonicalizer that refuses ordinary "+
				"destinations is one operators route around", raw, err)
		}
	}

	// A host UTS-46 declines to map is refused rather than passed through raw,
	// which is the only direction that fails closed — the raw spelling is exactly
	// the value the tiers cannot read. It is not a tier refusal: nothing here
	// judged the destination, the name simply is not one.
	for _, raw := range []string{
		// A right-to-left override, disallowed outright. Written escaped because
		// a raw one in this file would render the surrounding source backwards,
		// which is the display attack it is here to represent.
		"http://exa\u202emple.com/",
		// A zero-width joiner, refused by the ContextJ rules the profile keeps.
		"http://exa\u200dmple.com/",
	} {
		err := func() error { _, err := ValidateDestination(raw, p); return err }()
		if err == nil {
			t.Errorf("accepted %q, whose host UTS-46 refuses to map; passing the raw "+
				"spelling through is what F77 was", raw)
			continue
		}
		if got := codesOf(t, err); got != "invalid" {
			t.Errorf("refused %q with code %q, want %q: an unmappable host is a "+
				"malformed name, not a verdict about where it points", raw, got, "invalid")
		}
	}
}

// The other half of the same claim: the two appealable tiers can only ever add
// refusals. Neither the embedded list nor the heuristics has a return value that
// means "allowed", so no entry anybody adds to either — and no row M31's review
// queue removes from the Postgres list — can hand a private address an approval
// that ValidateDestination would then have to honour.
func TestAppealableTiersCanOnlyRefuse(t *testing.T) {
	if got := reflect.TypeOf(highConfidence).NumOut(); got != 1 {
		t.Fatalf("highConfidence returns %d values; it must return only a refusal", got)
	}
	if got := reflect.TypeOf(highConfidence).Out(0).String(); got != "*link.Block" {
		t.Errorf("highConfidence returns %s, want *link.Block; a type meaning "+
			"'allowed' is the one thing this tier must not be able to say", got)
	}
	if got := reflect.TypeOf(lowConfidenceHeuristics).Out(0).String(); got != "*link.Block" {
		t.Errorf("lowConfidenceHeuristics returns %s, want *link.Block", got)
	}

	// And they are consulted after the unappealable tier has already refused,
	// so there is no order in which their answer could be reached first.
	if _, err := ValidateDestination("http://169.254.169.254/", DefaultDestinationPolicy()); err == nil {
		t.Fatal("the metadata endpoint must be refused before any tier that can be appealed")
	}
}

// The embedded tier is confined to exact matches on purpose: a false positive
// there costs a rebuild, and a suffix match multiplies how often that happens
// until operators route around the feature.
func TestEmbeddedTierMatchesExactHostsOnly(t *testing.T) {
	const listed = "metadata.google.internal"
	if _, ok := embeddedHosts[listed]; !ok {
		t.Skipf("%s is not on the proposed list; nothing to assert about matching", listed)
	}
	if highConfidence(listed) == nil {
		t.Errorf("%q is on the embedded list and was not refused", listed)
	}
	for _, near := range []string{
		"sub." + listed,               // a child, which a suffix match would catch
		"notmetadata.google.internal", // a different name that ends the same way
		"google.internal",             // the parent
	} {
		if highConfidence(near) != nil {
			t.Errorf("%q was refused by the embedded tier; that tier is exact "+
				"matches only, or every false positive becomes a rebuild", near)
		}
	}
}

// parseHostList panics rather than skipping a line it cannot honour, because a
// blocklist that silently drops an entry leaves the operator believing a host is
// refused when it is not.
func TestEmbeddedListRefusesEntriesItCannotHonour(t *testing.T) {
	bad := map[string]string{
		"a scheme":       "https://evil.example",
		"a path":         "evil.example/login",
		"a port":         "evil.example:8443",
		"credentials":    "user@evil.example",
		"a wildcard":     "*.evil.example",
		"a leading dot":  ".evil.example",
		"a trailing dot": "evil.example.",
		// An address is the unappealable tier's business. Allowing one here
		// would invite a reader to believe that deleting the line makes the
		// address acceptable, which it does not.
		"an address":        "169.254.169.254",
		"an obfuscated one": "2852039166",
		"a hex-written one": "0xa9fea9fe",
		// The same address rule, asked in the alphabet F77 was found in. The
		// numeric test splits on ASCII dots, so this entry has a last label of
		// "２５４" and reads as a name until it has been folded — which is why the
		// fold happens before the address rule and not after it.
		"an address in fullwidth digits": "１６９.２５４.１６９.２５４",
		"an address separated by U+3002": "169。254。169。254",
	}
	for name, entry := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := checkListEntry(entry); err == nil {
				t.Errorf("accepted list entry %q", entry)
			}
		})
	}
	// The shapes that must keep working, or the list cannot hold what it is for.
	for _, entry := range []string{"metadata", "metadata.goog", "kubernetes.default.svc", "instance-data"} {
		if _, err := checkListEntry(entry); err != nil {
			t.Errorf("rejected legitimate entry %q: %v", entry, err)
		}
	}

	// An entry an operator writes in their own script has to land in the alphabet
	// the tier is asked about, or it is a line that refuses nothing forever. The
	// destination side now stores the ToASCII form, so the list side has to
	// produce it too — one fold, both sides, or they cannot meet.
	for entry, want := range map[string]string{
		"münchen.example":          "xn--mnchen-3ya.example",
		"テスト.example":              "xn--zckzah.example",
		"ＥＸＡＭＰＬＥ.example":          "example.example",
		"metadata。google。internal": "metadata.google.internal",
	} {
		got, err := checkListEntry(entry)
		if err != nil {
			t.Errorf("rejected an ordinary internationalized entry %q: %v", entry, err)
			continue
		}
		if got != want {
			t.Errorf("entry %q folded to %q, want %q: an entry and a destination "+
				"that fold differently can never match", entry, got, want)
		}
	}
}

// "Heuristics never write into the embedded tier — asserted structurally, not by
// convention." The structure is that a heuristic has nowhere to put a tier: the
// evaluator stamps every match TierLowConfidence, and no field of the heuristic
// type could carry a different answer.
func TestHeuristicsCannotNameATier(t *testing.T) {
	ht := reflect.TypeOf(heuristic{})
	for i := 0; i < ht.NumField(); i++ {
		f := ht.Field(i)
		if f.Type == reflect.TypeOf(TierLowConfidence) {
			t.Fatalf("heuristic.%s is a Tier. A heuristic that can name its own "+
				"tier can promote itself into the one that costs a rebuild to "+
				"overrule, which is what confines that tier to exact matches.", f.Name)
		}
	}
	if ht.NumField() != 3 {
		t.Errorf("heuristic has %d fields; if one was added, check it cannot carry a tier",
			ht.NumField())
	}

	// Every registered rule is exercised on an ordinary URL, so a heuristic that
	// panics on one is caught here rather than on somebody's first link — and so
	// the registry cannot quietly grow an entry nothing ever calls.
	u, err := url.Parse("https://example.com/path")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range heuristics {
		if h.rule == "" || h.detail == "" || h.match == nil {
			t.Errorf("heuristic %+v is incomplete", h)
			continue
		}
		if h.match(u, "example.com") {
			t.Errorf("heuristic %q refuses https://example.com/path", h.rule)
		}
	}
}

func TestHeuristicsCatchWhatTheyClaimTo(t *testing.T) {
	blocked := map[string]string{
		// Credentials before the host: the authority is evil.example, and the
		// part a reader's eye lands on is not.
		"https://paypal.com@evil.example/signin": RuleURLCredentials,
		"https://user:pass@example.com/":         RuleURLCredentials,
		// xn--pple-43d is "аpple" with a Cyrillic а.
		"https://xn--pple-43d.com/": RulePunycodeHomograph,
	}
	for raw, wantRule := range blocked {
		t.Run(raw, func(t *testing.T) {
			u, host := parseForTest(t, raw)
			b := lowConfidenceHeuristics(u, host)
			if b == nil {
				t.Fatalf("no heuristic matched %q, expected %s", raw, wantRule)
			}
			if b.Rule != wantRule {
				t.Errorf("matched %q, want %q", b.Rule, wantRule)
			}
			if b.Tier != TierLowConfidence {
				t.Errorf("tier = %q, want %q", b.Tier, TierLowConfidence)
			}
		})
	}

	// The counterpart, which is the expensive half to get wrong. Every one of
	// these is an ordinary destination and a heuristic that refuses it is a
	// heuristic operators turn off.
	for _, raw := range []string{
		"https://example.com/path?a=1",
		"https://xn--mller-kva.de/", // müller.de — an ordinary German name
		"https://xn--n3h.example/",  // ☃.example — a symbol, imitating nothing
		"https://93.184.216.34/",
		// A short link is no longer refused by anything computed: the shortener
		// hosts are rows in blocked_destinations (D39), so this registry has
		// nothing to say about one and must not acquire an opinion by accident.
		"https://bit.ly/abc",
	} {
		t.Run(raw, func(t *testing.T) {
			u, host := parseForTest(t, raw)
			if b := lowConfidenceHeuristics(u, host); b != nil {
				t.Errorf("refused an ordinary destination with %q", b.Rule)
			}
		})
	}
}

// Freshly-registered domains is excluded by D13 — it needs a domain-age source,
// which means egress, which collides with the promise that no destination leaves
// the box uninvited. Pinned here so re-adding it is a deliberate act with a
// failing test attached, rather than something that arrives with a library.
func TestFreshlyRegisteredDomainsIsNotAHeuristic(t *testing.T) {
	for _, h := range heuristics {
		if strings.Contains(h.rule, "age") || strings.Contains(h.rule, "fresh") ||
			strings.Contains(h.rule, "registered") {
			t.Errorf("heuristic %q looks like domain age, excluded by D13: it needs "+
				"an egress data source. M32's opt-in feed path is where it belongs.", h.rule)
		}
	}
}

// D39, as a property of the tree rather than a sentence in a decision log.
//
// The rule it states is that a list is compiled when overruling it *should* be
// hard, and is runtime data otherwise — so the test has to show both halves.
// Asserting only that the shortener file is gone would pass just as happily on a
// tree that had stopped compiling any list at all, which is the opposite
// mistake and the more likely one: embedding a list is the cheapest way to add
// one, and it is cheapest at exactly the moment somebody is adding data that
// does not belong in the binary.
func TestOnlyTheTierThatCostsARebuildIsCompiled(t *testing.T) {
	// The high-confidence list is still compiled, because overruling it is meant
	// to cost a release. Its contents are structural claims about metadata
	// services and control planes, and they stay true for years.
	if _, err := os.Stat("blocked_hosts.txt"); err != nil {
		t.Errorf("blocked_hosts.txt: %v. The high-confidence tier is the one list "+
			"that is meant to be compiled; a tier nobody has to rebuild to "+
			"overrule is not the high-confidence tier.", err)
	}
	if len(embeddedHosts) == 0 {
		t.Error("the embedded list is empty; the compiled tier refuses nothing")
	}

	// And the shortener list is not, because a match on it raises a flag the
	// owner may overrule from the review queue. Compiling it charged a release
	// cycle to data carrying no authority.
	if _, err := os.Stat("shortener_hosts.txt"); !os.IsNotExist(err) {
		t.Errorf("shortener_hosts.txt is back in the package (%v). Per D39 those "+
			"hosts are rows in blocked_destinations, seeded by migration 01500 "+
			"and editable without a rebuild.", err)
	}
	for _, h := range heuristics {
		if h.rule == RuleShortenerChain {
			t.Errorf("%q is a compiled heuristic again. A rule whose whole content "+
				"is a list of names is data: keeping it here means a new shortener "+
				"costs a release.", h.rule)
		}
	}
}

// Which rule a matched row reports, and the reason the source column exists at
// all beyond bookkeeping.
func TestTheMatchedRowsSourceDecidesTheRule(t *testing.T) {
	cases := map[string]string{
		SourceShortener: RuleShortenerChain,
		SourceEnv:       RuleOperatorBlocklist,
		SourceReview:    RuleOperatorBlocklist,
		// A source no release has heard of. The column has no CHECK constraint
		// and M32's feeds will add to it, so this is a future state rather than
		// a corruption — and it is still a host somebody listed. Minting a code
		// from the column's contents would put a string in a 422 that no
		// documentation explains.
		"some_later_feed": RuleOperatorBlocklist,
	}
	for source, wantRule := range cases {
		t.Run(source, func(t *testing.T) {
			b := blockForSource(source)
			if b.Rule != wantRule {
				t.Errorf("source %q reports rule %q, want %q", source, b.Rule, wantRule)
			}
			// No source promotes a row out of the tier the owner can overrule.
			// A shortener row that refused at high confidence would cost a
			// rebuild to appeal, which is exactly what D39 moved it out of.
			if b.Tier != TierLowConfidence {
				t.Errorf("source %q refuses at tier %q, want %q",
					source, b.Tier, TierLowConfidence)
			}
			if b.Detail == "" {
				t.Errorf("source %q refuses with no explanation", source)
			}
		})
	}
}

// The label-boundary rule LINKCTRL_DESTINATION_BLOCKLIST has always had, now
// expressed as the set of hosts the database is asked about.
func TestHostCandidatesRespectLabelBoundaries(t *testing.T) {
	got := HostCandidates("deep.sub.evil.example")
	want := []string{"deep.sub.evil.example", "sub.evil.example", "evil.example", "example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}

	// An entry for evil.example is in the candidate set of its children and is
	// not in the candidate set of a name that merely ends with those letters.
	for _, host := range []string{"notevil.example", "myevil.example", "evil.example.org"} {
		for _, c := range HostCandidates(host) {
			if c == "evil.example" {
				t.Errorf("%q would match a blocklist entry for evil.example", host)
			}
		}
	}

	// A trailing dot is the same name fully qualified, and must not produce a
	// different candidate set — or a blocklist is bypassed by typing one.
	if !reflect.DeepEqual(HostCandidates("evil.example."), HostCandidates("evil.example")) {
		t.Error("a trailing dot changed the candidate set; that is a one-character bypass")
	}

	// The shortener hosts are matched by this rule now rather than by exact
	// equality, so the case that used to guard the heuristic is asserted here: a
	// name that merely contains a shortener's is not that shortener.
	for _, c := range HostCandidates("sub.bit.ly.evil-looking-but-not-a-shortener.example") {
		if c == "bit.ly" {
			t.Error("a host that merely contains bit.ly would match the seeded row")
		}
	}
	// A child of one is, which is wider than the compiled list was and is the
	// right width for the tier that is allowed to guess.
	var covered bool
	for _, c := range HostCandidates("links.bit.ly") {
		covered = covered || c == "bit.ly"
	}
	if !covered {
		t.Error("links.bit.ly is not covered by a row for bit.ly")
	}
}

func TestReasonCodesNameTierAndRule(t *testing.T) {
	if got := TierLowConfidence.Code(RulePunycodeHomograph); got != "low_confidence.punycode_homograph" {
		t.Errorf("code = %q", got)
	}
	tier, rule, ok := tierOf("high_confidence.embedded_host")
	if !ok || tier != TierHighConfidence || rule != RuleEmbeddedHost {
		t.Errorf("tierOf = (%q, %q, %v)", tier, rule, ok)
	}

	// The shape errors are not refusals by a tier and must not be recorded as
	// blocked attempts: a URL somebody left blank is a typo, and burying the
	// real refusals under typos is how an audit log stops being read.
	for _, code := range []string{"required", "too_long", "invalid", "no_scheme", "no_host", ""} {
		if _, _, ok := tierOf(code); ok {
			t.Errorf("%q was read as a tiered refusal", code)
		}
	}
}

// The URL is stored as evidence, and evidence gets rendered. Both halves matter:
// inert as markup, so it cannot become script wherever it is displayed, and
// inert as a link, so nobody follows it by reflex while reading the record.
func TestDefangRendersHostileURLsInert(t *testing.T) {
	cases := map[string]string{
		"javascript:alert(1)":                                    "javascript[:]alert(1)",
		"https://evil.example/x":                                 "https[:]//evil[.]example/x",
		"https://evil.example/<script>alert(1)</script>":         "https[:]//evil[.]example/%3Cscript%3Ealert(1)%3C/script%3E",
		`https://evil.example/?q="><img src=x onerror=alert(1)>`: "https[:]//evil[.]example/?q=%22%3E%3Cimg%20src=x%20onerror=alert(1)%3E",
		// An open-redirect payload: the path holds another URL, and the record
		// of the refusal must not contain a followable link to it.
		"https://evil.example/r?to=https://worse.example/": "https[:]//evil[.]example/r?to=https[:]//worse[.]example/",
	}
	for raw, want := range cases {
		if got := Defang(raw); got != want {
			t.Errorf("Defang(%q)\n = %q\nwant %q", raw, got, want)
		}
	}

	// The properties, rather than the exact spelling, for anything that reaches
	// this function from outside a test.
	for _, raw := range []string{
		"javascript:alert(1)",
		"https://evil.example/<script>",
		"https://evil.example/\u202egnp.exe", // a right-to-left override, which reverses what a reader sees
		"https://аррӏе.example/",
		strings.Repeat("https://evil.example/", 500),
	} {
		got := Defang(raw)
		if strings.ContainsAny(got, "<>\"'&`") {
			t.Errorf("Defang(%q) = %q still contains an HTML-active character", raw, got)
		}
		if strings.Contains(got, "://") {
			t.Errorf("Defang(%q) = %q is still a followable URL", raw, got)
		}
		if strings.Contains(strings.ToLower(got), "javascript:") {
			t.Errorf("Defang(%q) = %q still carries a live scheme", raw, got)
		}
		if len(got) > 6*defangMaxBytes {
			t.Errorf("Defang(%q) produced %d bytes; the stored evidence must be bounded",
				raw, len(got))
		}
	}
}

// --- helpers -----------------------------------------------------------------

func parseForTest(t *testing.T, raw string) (*url.URL, string) {
	t.Helper()
	normalized, err := ValidateDestination(raw, DefaultDestinationPolicy())
	if err != nil {
		t.Fatalf("ValidateDestination(%q): %v", raw, err)
	}
	u, err := url.Parse(normalized)
	if err != nil {
		t.Fatalf("parse %q: %v", normalized, err)
	}
	return u, strings.ToLower(u.Hostname())
}

// codesOf returns the reason code of the first field error, which is the one a
// form highlights and the one the audit record carries.
func codesOf(t *testing.T, err error) string {
	t.Helper()
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) || len(ve) == 0 {
		t.Fatalf("error %T carries no field errors: %v", err, err)
	}
	return ve[0].Code
}
