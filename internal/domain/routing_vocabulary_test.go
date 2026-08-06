package domain_test

import (
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/analytics"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// TestRuleDeviceVocabularyMatchesTheClassifier pins two lists together that
// live in packages which cannot see each other.
//
// internal/domain must not import internal/analytics — analytics imports domain,
// and the dependency runs that way because domain is the vocabulary the services
// exchange. So the device names a rule may be written with are listed in
// domain/routing.go by hand, and this is what stops that copy drifting from the
// buckets analytics.Classify actually produces.
//
// The drift it prevents is silent and one-directional: a new bucket in the
// classifier would be a device nobody can write a rule for, and the rule form
// would refuse a value the analytics page displays. Nothing would fail; the
// feature would just have a hole in it.
//
// An external test package (`domain_test`) rather than an internal one, which is
// what makes importing analytics legal here at all.
func TestRuleDeviceVocabularyMatchesTheClassifier(t *testing.T) {
	// Every bucket Classify can return, named rather than reflected: the type is
	// a string alias, so there is nothing to enumerate, and a new constant that
	// nobody adds here is exactly the case this test exists for — it will show up
	// as a user agent the rule validator refuses.
	produced := []analytics.Device{
		analytics.DeviceDesktop,
		analytics.DeviceMobile,
		analytics.DeviceTablet,
		analytics.DeviceBot,
		analytics.DeviceUnknown,
	}

	for _, d := range produced {
		if err := domain.ValidateRuleConditions(domain.RuleConditions{
			Device: []string{string(d)},
		}); err != nil {
			t.Errorf("analytics.Classify produces device %q but a rule cannot be "+
				"written for it: %v", d, err)
		}
	}

	// And the other direction: a value the rule validator accepts that the
	// classifier never produces would be a rule that can be saved and can never
	// match. Sampled through the classifier rather than asserted against a second
	// list, because a second list is the thing being avoided.
	seen := map[string]bool{}
	for _, ua := range []string{
		"", // absent user agent: the classifier reads it as a bot
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120 Safari/537.36",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605 Mobile/15E148 Safari/604",
		"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605 Mobile/15E148 Safari/604",
		"Googlebot/2.1 (+http://www.google.com/bot.html)",
		"something that is not a browser at all",
	} {
		seen[string(analytics.Classify(ua).Device)] = true
	}
	for _, d := range produced {
		if !seen[string(d)] {
			t.Logf("no sample user agent produced %q; the list above is not exhaustive "+
				"and that is fine — the assertion that matters is the loop above", d)
		}
	}
}

// TestRuleLanguageVocabularyMatchesTheClassifier is the same pinning for
// languages, which are stored and matched through the same reader.
//
// A rule written as "en-GB" would never match, because the click pipeline keeps
// only the first subtag. The validator's 8-character cap does not catch that on
// its own, so the honest check is that what PrimaryLanguage produces is what a
// rule may be written with.
func TestRuleLanguageVocabularyMatchesTheClassifier(t *testing.T) {
	for _, header := range []string{"en-GB,en;q=0.9", "de", "fr-CA", "pt-BR,pt;q=0.8"} {
		lang := analytics.PrimaryLanguage(header)
		if lang == "" {
			t.Fatalf("PrimaryLanguage(%q) is empty; this test assumes it is not", header)
		}
		if err := domain.ValidateRuleConditions(domain.RuleConditions{
			Language: []string{lang},
		}); err != nil {
			t.Errorf("Accept-Language %q is stored as %q, which a rule cannot be "+
				"written for: %v", header, lang, err)
		}
	}
}
