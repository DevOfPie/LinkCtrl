package alias

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed reserved.txt
var reservedRaw string

//go:embed profanity.txt
var profanityRaw string

var (
	reserved          map[string]struct{}
	profaneWords      map[string]struct{}
	profaneSubstrings []string
)

func init() {
	reserved = parseSet(reservedRaw)

	profaneWords = make(map[string]struct{})
	for _, line := range parseLines(profanityRaw) {
		prefix, term, ok := strings.Cut(line, ":")
		if !ok {
			panic(fmt.Sprintf("alias: profanity.txt entry %q missing word:/sub: prefix", line))
		}
		switch prefix {
		case "word":
			// Store the normalized form so lookups need no extra work and so a
			// list entry written with leetspeak still behaves.
			profaneWords[normalizeLeet(term)] = struct{}{}
		case "sub":
			profaneSubstrings = append(profaneSubstrings, normalizeLeet(term))
		default:
			panic(fmt.Sprintf("alias: profanity.txt entry %q has unknown prefix %q", line, prefix))
		}
	}
}

func parseSet(raw string) map[string]struct{} {
	lines := parseLines(raw)
	set := make(map[string]struct{}, len(lines))
	for _, l := range lines {
		set[l] = struct{}{}
	}
	return set
}

func parseLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.ToLower(line))
	}
	return out
}

// IsReserved reports whether an alias is on the reserved list. The input is
// canonicalized first, so callers need not do it.
func IsReserved(s string) bool {
	_, ok := reserved[Canonical(s)]
	return ok
}

// Reserved returns a copy of the reserved list. Used by the router test that
// asserts every registered top-level route is reserved.
func Reserved() []string {
	out := make([]string, 0, len(reserved))
	for k := range reserved {
		out = append(out, k)
	}
	return out
}

// IsProfane reports whether an alias contains disallowed language.
//
// Two passes, for the reason documented in profanity.txt: whole-token matching
// for terms that legitimately appear inside other words, and substring matching
// (with separators stripped, defeating "n-i-g-g-e-r") for terms that do not.
func IsProfane(s string) bool {
	normalized := normalizeLeet(Canonical(s))

	// Whole alias, or any hyphen/underscore-delimited token within it.
	if _, ok := profaneWords[stripSeparators(normalized)]; ok {
		return true
	}
	for _, token := range strings.FieldsFunc(normalized, func(r rune) bool {
		return r == '-' || r == '_'
	}) {
		if _, ok := profaneWords[token]; ok {
			return true
		}
	}

	compact := stripSeparators(normalized)
	for _, sub := range profaneSubstrings {
		if strings.Contains(compact, sub) {
			return true
		}
	}
	return false
}

// normalizeLeet folds common digit-for-letter substitutions so that "sh1t" and
// "f4ggot" are caught by the same list entries as the plain spellings.
//
// This is intentionally lossy and one-directional. It is only ever used for
// blocklist comparison — never for storage, never for the cache key, and never
// for anything a user sees.
func normalizeLeet(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '0':
			b.WriteRune('o')
		case '1':
			b.WriteRune('i')
		case '3':
			b.WriteRune('e')
		case '4', '@':
			b.WriteRune('a')
		case '5', '$':
			b.WriteRune('s')
		case '7':
			b.WriteRune('t')
		case '8':
			b.WriteRune('b')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func stripSeparators(s string) string {
	return strings.NewReplacer("-", "", "_", "").Replace(s)
}
