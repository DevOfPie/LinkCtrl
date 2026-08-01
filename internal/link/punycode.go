package link

import (
	"math"
	"strings"
	"unicode/utf8"
)

// Homograph detection, for the low-confidence tier.
//
// The attack is a host that renders as a name the visitor already trusts:
// "аpple.com" with a Cyrillic а, or "аррӏе.com" spelled entirely in Cyrillic.
// Both are ordinary ASCII on the wire — xn--pple-43d, xn--80ak6aa92e — so
// nothing before this point can see them, and a browser shows the reader the
// spelling the attacker chose.
//
// Implemented here rather than pulled in, because the only thing needed from an
// IDNA library is the punycode decoder, and adding a dependency to a repository
// that has kept its go.mod short is a decision the owner should make for a
// better reason than sixty lines. The decoder is RFC 3492 and is pinned by the
// specification's own test vectors.
//
// Low confidence on purpose. This refuses names that are genuinely spelled to
// imitate an ASCII name and it will sometimes be wrong, so the instance owner
// overrules it from the review queue without a rebuild.

// isHomograph reports whether any label of a host is confusable with an
// all-ASCII label.
//
// The test is not "does this host use non-ASCII characters" — müller.de and
// 日本語.jp are ordinary names and are not touched. It is "does every character
// of this label map onto an ASCII letter or digit, with at least one of them
// doing so by resemblance rather than by being that character". A label that
// passes that test has no purpose except to be read as a different name.
func isHomograph(host string) bool {
	for _, label := range strings.Split(host, ".") {
		if !strings.HasPrefix(label, "xn--") {
			continue
		}
		decoded, ok := punycodeDecode(label[len("xn--"):])
		if !ok {
			// Malformed punycode is not a homograph. It is a host that will not
			// resolve, and refusing it here would report the wrong reason.
			continue
		}
		if confusableWithASCII(decoded) {
			return true
		}
	}
	return false
}

// confusableWithASCII reports whether a decoded label reads as ASCII.
//
// True when every rune is either an ASCII letter, digit or hyphen, or a
// character that resembles one — and at least one rune is of the second kind.
// The second condition is what stops this from firing on a label that decoded to
// plain ASCII, which punycode would not have been used for anyway.
func confusableWithASCII(label string) bool {
	if label == "" {
		return false
	}
	imitations := 0
	for _, r := range label {
		switch {
		case r < utf8.RuneSelf:
			if !isASCIIHostRune(r) {
				return false
			}
		default:
			if _, ok := latinConfusables[r]; !ok {
				return false
			}
			imitations++
		}
	}
	return imitations > 0
}

func isASCIIHostRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '-'
}

// latinConfusables maps characters that render as a Latin letter or digit onto
// the letter they imitate.
//
// Cyrillic and Greek, which is where the overwhelming majority of registrable
// homographs come from: both are widely available in IDN registries and both
// have full sets of letters that are visually identical to Latin ones in most
// fonts. This is not the complete Unicode confusables table — that is a large
// data file and a dependency — and it does not need to be, because the tier this
// feeds is the one that guesses and is overruled from the review queue.
//
// The value each maps to is the Latin letter it imitates, which is not used for
// matching but records what the entry is claiming, so a reader can check it.
var latinConfusables = map[rune]rune{
	// Cyrillic lowercase.
	'а': 'a', 'в': 'b', 'с': 'c', 'ԁ': 'd', 'е': 'e', 'ѕ': 's', 'һ': 'h',
	'і': 'i', 'ј': 'j', 'к': 'k', 'ӏ': 'l', 'м': 'm', 'н': 'h', 'о': 'o',
	'р': 'p', 'ԛ': 'q', 'г': 'r', 'т': 't', 'и': 'u', 'ѵ': 'v', 'ԝ': 'w',
	'х': 'x', 'у': 'y', 'ᴜ': 'u',
	// Cyrillic uppercase.
	'А': 'a', 'В': 'b', 'С': 'c', 'Е': 'e', 'Ѕ': 's', 'Н': 'h', 'І': 'i',
	'Ј': 'j', 'К': 'k', 'М': 'm', 'О': 'o', 'Р': 'p', 'Ԛ': 'q', 'Т': 't',
	'Ѵ': 'v', 'Ԝ': 'w', 'Х': 'x', 'У': 'y',
	// Greek lowercase.
	'α': 'a', 'ϲ': 'c', 'ε': 'e', 'ι': 'i', 'κ': 'k', 'ν': 'v', 'ο': 'o',
	'ρ': 'p', 'σ': 'o', 'τ': 't', 'υ': 'u', 'χ': 'x', 'ω': 'w', 'μ': 'u',
	// Greek uppercase.
	'Α': 'a', 'Β': 'b', 'Ε': 'e', 'Ζ': 'z', 'Η': 'h', 'Ι': 'i', 'Κ': 'k',
	'Μ': 'm', 'Ν': 'n', 'Ο': 'o', 'Ρ': 'p', 'Τ': 't', 'Υ': 'y', 'Χ': 'x',
	// Fullwidth Latin, which is neither Cyrillic nor Greek but is the other
	// spelling that renders as an ASCII name.
	'ａ': 'a', 'ｂ': 'b', 'ｃ': 'c', 'ｄ': 'd', 'ｅ': 'e', 'ｇ': 'g', 'ｉ': 'i',
	'ｌ': 'l', 'ｍ': 'm', 'ｎ': 'n', 'ｏ': 'o', 'ｐ': 'p', 'ｒ': 'r', 'ｓ': 's',
	'ｔ': 't', 'ｕ': 'u', 'ｙ': 'y',
}

// RFC 3492 parameters.
const (
	punyBase        = 36
	punyTMin        = 1
	punyTMax        = 26
	punySkew        = 38
	punyDamp        = 700
	punyInitialBias = 72
	punyInitialN    = 128
)

// punycodeDecode implements the RFC 3492 decoding procedure.
//
// Reports false on anything malformed — a bad digit, an overflow, a surrogate or
// an out-of-range code point — rather than returning a partial answer. A caller
// deciding whether a name imitates another must not be handed a guess.
func punycodeDecode(encoded string) (string, bool) {
	if encoded == "" {
		return "", false
	}

	var output []rune
	pos := 0
	// Everything before the last delimiter is literal ASCII. No delimiter means
	// the whole string is extended code points.
	if b := strings.LastIndexByte(encoded, '-'); b >= 0 {
		for i := 0; i < b; i++ {
			c := encoded[i]
			if c >= utf8.RuneSelf {
				return "", false
			}
			output = append(output, rune(c))
		}
		pos = b + 1
	}

	// n is an int rather than a rune while it is being accumulated. The
	// arithmetic below can overflow a code point on malformed input, and a rune
	// that has overflowed is indistinguishable from one that has not — so the
	// bound is checked on a plain integer and the conversion happens once, at
	// the append, where it is provably in range.
	n := punyInitialN
	bias := punyInitialBias
	i := 0
	for pos < len(encoded) {
		oldI, w := i, 1
		for k := punyBase; ; k += punyBase {
			if pos >= len(encoded) {
				return "", false
			}
			digit, ok := punyDigit(encoded[pos])
			if !ok {
				return "", false
			}
			pos++
			if digit > (math.MaxInt32-i)/w {
				return "", false
			}
			i += digit * w
			t := k - bias
			switch {
			case t < punyTMin:
				t = punyTMin
			case t > punyTMax:
				t = punyTMax
			}
			if digit < t {
				break
			}
			if w > math.MaxInt32/(punyBase-t) {
				return "", false
			}
			w *= punyBase - t
		}
		out := len(output) + 1
		bias = punyAdapt(i-oldI, out, oldI == 0)
		if i/out > math.MaxInt32-n {
			return "", false
		}
		n += i / out
		i %= out
		if n < 0 || n > utf8.MaxRune || (n >= 0xD800 && n <= 0xDFFF) {
			return "", false
		}
		output = append(output, 0)
		copy(output[i+1:], output[i:])
		output[i] = rune(n) //nolint:gosec // G115: bounded to [0, utf8.MaxRune] on the line above
		i++
	}
	return string(output), true
}

func punyDigit(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c-'0') + 26, true
	case c >= 'a' && c <= 'z':
		return int(c - 'a'), true
	case c >= 'A' && c <= 'Z':
		return int(c - 'A'), true
	}
	return 0, false
}

func punyAdapt(delta, numPoints int, firstTime bool) int {
	if firstTime {
		delta /= punyDamp
	} else {
		delta /= 2
	}
	delta += delta / numPoints
	k := 0
	for delta > ((punyBase-punyTMin)*punyTMax)/2 {
		delta /= punyBase - punyTMin
		k += punyBase
	}
	return k + (punyBase-punyTMin+1)*delta/(delta+punySkew)
}
