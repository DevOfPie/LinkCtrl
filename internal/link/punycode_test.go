package link

import "testing"

// The RFC 3492 sample strings, plus the two shapes the homograph heuristic is
// actually built on. A decoder that is wrong here reports the wrong name, and a
// heuristic reading the wrong name refuses the wrong hosts.
func TestPunycodeDecodeMatchesTheSpecification(t *testing.T) {
	cases := map[string]string{
		// RFC 3492 §7.1, verbatim.
		"egbpdaj6bu4bxfgehfvwxn":                   "ليهمابتكلموشعربي؟",
		"ihqwcrb4cv8a8dqg056pqjye":                 "他们为什么不说中文",
		"3B-ww4c5e180e575a65lsy2b":                 "3年B組金八先生",
		"Hello-Another-Way--fc4qua05auwb3674vfr0b": "Hello-Another-Way-それぞれの場所",
		// Ordinary internationalized names, which must decode and must not be
		// mistaken for imitations.
		"mller-kva": "müller",
		"n3h":       "☃",
		"zckzah":    "テスト",
		// The two homographs the heuristic exists for: one mixed-script, one
		// spelled entirely in Cyrillic.
		"pple-43d":   "аpple",
		"80ak6aa92e": "аррӏе",
	}
	for encoded, want := range cases {
		got, ok := punycodeDecode(encoded)
		if !ok {
			t.Errorf("punycodeDecode(%q) failed", encoded)
			continue
		}
		if got != want {
			t.Errorf("punycodeDecode(%q) = %q, want %q", encoded, got, want)
		}
	}
}

func TestPunycodeDecodeRefusesMalformedInput(t *testing.T) {
	for _, encoded := range []string{
		"",
		"!!!",
		"99999999999999999999999999",
		"a-!",
		"héllo", // non-ASCII in what is meant to be an ASCII encoding
	} {
		if got, ok := punycodeDecode(encoded); ok {
			t.Errorf("punycodeDecode(%q) = %q, want a refusal: a caller deciding "+
				"whether a name imitates another must not be handed a guess", encoded, got)
		}
	}
}

// The heuristic's actual question: does this label read as an ASCII name it is
// not? The expensive mistake is the false positive, so the "not a homograph"
// half is the half worth reading.
func TestIsHomograph(t *testing.T) {
	homographs := []string{
		"xn--pple-43d.com",     // аpple.com — one Cyrillic letter
		"xn--80ak6aa92e.com",   // аррӏе.com — entirely Cyrillic
		"www.xn--pple-43d.com", // any label, not only the registrable one
	}
	for _, host := range homographs {
		if !isHomograph(host) {
			t.Errorf("%q was not detected as a homograph", host)
		}
	}

	ordinary := []string{
		"example.com",
		"xn--mller-kva.de",   // müller.de
		"xn--n3h.example",    // ☃.example
		"xn--zckzah.example", // テスト.example
		// Decodes to control characters rather than to anything readable. Not a
		// homograph — a host that will not resolve — and reporting it as one
		// would name the wrong reason.
		"xn--not-valid-punycode-at-all",
		"bücher.example", // already decoded, no xn-- prefix to read
	}
	for _, host := range ordinary {
		if isHomograph(host) {
			t.Errorf("%q was called a homograph; a heuristic that refuses ordinary "+
				"internationalized names is one operators route around", host)
		}
	}
}

func TestConfusableWithASCIINeedsAnImitation(t *testing.T) {
	// All-ASCII decodes to itself and imitates nothing — punycode would not have
	// been used for it, and treating it as an imitation would fire on noise.
	if confusableWithASCII("apple") {
		t.Error("an all-ASCII label was called an imitation")
	}
	if confusableWithASCII("") {
		t.Error("the empty label was called an imitation")
	}
	if !confusableWithASCII("аpple") {
		t.Error("a Cyrillic а in an otherwise ASCII word is exactly the attack")
	}
	// A name with a character that resembles nothing Latin is a real name.
	if confusableWithASCII("日本語") {
		t.Error("a name in a script with no Latin lookalikes was called an imitation")
	}
}
