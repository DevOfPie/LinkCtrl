package link

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// outboundFromTheJudge matches a symbol that would let this package reach the
// network on its own.
//
// Blunt and over-broad on purpose, like the sibling gate in internal/dispute. A
// false positive is a line somebody justifies in a comment; a false negative is
// a destination leaving the box by a route the disclosure page does not know
// about, on an instance whose operator was told nothing leaves.
var outboundFromTheJudge = regexp.MustCompile(
	`\b(?:` +
		`http\.(?:Get|Head|Post|PostForm|Do|NewRequest|NewRequestWithContext|Client|DefaultClient|` +
		`DefaultTransport|Transport)` +
		`|httputil\.` +
		`|net\.(?:Dial|DialTimeout|DialIP|DialTCP|DialUDP|Dialer|Resolver|LookupHost|LookupIP|LookupAddr|LookupCNAME)` +
		`|exec\.(?:Command|CommandContext)` +
		`|smtp\.|websocket\.` +
		`)`)

// TestTheFeedIsTheOnlyWayADestinationLeaves.
//
// This package decides what a destination is allowed to be, and M32 gave it one
// reason to talk to anybody: the opt-in reputation feed, held behind
// FeedChecker, nil unless an operator named one. That interface is the disclosed
// exception, and it is only an exception if it is the *only* one — a DNS lookup
// added here to "check the host resolves", or a HEAD request to "see if it is
// alive", would each send a user's destination somewhere while /feeds went on
// saying nothing leaves.
//
// So: no outbound symbol anywhere in this package. Egress goes through the
// interface or it does not happen.
func TestTheFeedIsTheOnlyWayADestinationLeaves(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	var scanned int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name) //nolint:gosec // G304: names come from this package's own directory
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		for i, line := range strings.Split(string(b), "\n") {
			if m := outboundFromTheJudge.FindString(line); m != "" {
				t.Errorf("%s:%d uses %s. The reputation feed is the one thing in this "+
					"program that sends a destination anywhere, it is off by default, "+
					"and it is disclosed at /feeds. Anything else here that reaches the "+
					"network is egress nobody was told about.",
					filepath.ToSlash(name), i+1, m)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no files; the walk is broken rather than the package clean")
	}
}

// TestAFeedVerdictCannotClaimATierOfItsOwn.
//
// The bullet says feed verdicts act *only* as low-confidence signals. That is
// structural here rather than checked: askFeed builds the Block itself and
// stamps TierLowConfidence, and FeedChecker has no method through which an
// answer could name a tier — the interface returns a Result, which is three
// words. This asserts the stamp, and the surrounding types are what stop it
// being circumventable.
func TestAFeedVerdictCannotClaimATierOfItsOwn(t *testing.T) {
	block := feedBlock("Stub Feed")
	if block == nil {
		t.Fatal("a malicious verdict produced no refusal")
	}
	if block.Tier != TierLowConfidence {
		t.Errorf("tier = %q, want %q. A third party's claim must never reach a tier "+
			"this product tells operators costs a rebuild to overrule.",
			block.Tier, TierLowConfidence)
	}
	if block.Rule != RuleFeedReputation {
		t.Errorf("rule = %q, want %q", block.Rule, RuleFeedReputation)
	}
	if !strings.Contains(block.Detail, "Stub Feed") {
		t.Errorf("the refusal does not name the feed: %q", block.Detail)
	}
}
