package httpx

import (
	"net/url"
	"strings"
	"testing"
)

// appendQuery is the only string surgery on the redirect hot path and had no
// direct test. The defect it grew was a silent one: url.Query() throws away
// ParseQuery's error along with every pair it could not read, so switching
// forward_query on quietly rewrote the destination.
func TestAppendQueryMergesWithoutLosingParameters(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		incoming string
		want     string
	}{
		{
			name:     "no destination query",
			target:   "https://shop.example/search",
			incoming: "utm_source=mail",
			want:     "https://shop.example/search?utm_source=mail",
		},
		{
			name:     "merges alongside existing",
			target:   "https://shop.example/search?q=shoes",
			incoming: "utm_source=mail",
			want:     "https://shop.example/search?q=shoes&utm_source=mail",
		},
		{
			name:     "destination wins on conflict",
			target:   "https://shop.example/search?q=shoes",
			incoming: "q=boots",
			want:     "https://shop.example/search?q=shoes",
		},
		{
			name:     "nothing incoming",
			target:   "https://shop.example/search?q=shoes",
			incoming: "",
			want:     "https://shop.example/search?q=shoes",
		},
		{
			name:     "fragment survives",
			target:   "https://shop.example/p#reviews",
			incoming: "utm_source=mail",
			want:     "https://shop.example/p?utm_source=mail#reviews",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appendQuery(tt.target, tt.incoming); got != tt.want {
				t.Errorf("appendQuery(%q, %q) = %q, want %q",
					tt.target, tt.incoming, got, tt.want)
			}
		})
	}
}

// The regression: a destination whose query Go's parser rejects must keep every
// one of its own parameters. Browsers accept all of these, and
// ValidateDestination does not reject them, so they reach the redirect intact
// with forwarding off — and used to be silently rewritten with it on.
func TestAppendQueryPreservesUnparseableDestinationQueries(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		incoming string
		// keepAll names the parameter substrings that must survive verbatim.
		keepAll []string
	}{
		{
			name:     "stray percent",
			target:   "https://shop.example/search?q=50%&size=2",
			incoming: "utm_source=mail",
			keepAll:  []string{"q=50%", "size=2", "utm_source=mail"},
		},
		{
			name:     "semicolon separator",
			target:   "https://shop.example/s?a=1;b=2",
			incoming: "utm_source=mail",
			keepAll:  []string{"a=1;b=2", "utm_source=mail"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendQuery(tt.target, tt.incoming)
			for _, want := range tt.keepAll {
				if !strings.Contains(got, want) {
					t.Errorf("appendQuery(%q, %q) = %q, which dropped %q",
						tt.target, tt.incoming, got, want)
				}
			}
			// It still has to be a URL a client can follow.
			if _, err := url.Parse(got); err != nil {
				t.Errorf("result does not parse: %v", err)
			}
		})
	}
}

// Even on the raw path the destination's own parameters must win, or a visitor
// can override a configured one by arriving with it in the URL.
func TestAppendQueryRawPathStillLetsTheDestinationWin(t *testing.T) {
	got := appendQuery("https://shop.example/s?q=50%&ref=house", "ref=visitor&extra=1")
	if strings.Contains(got, "ref=visitor") {
		t.Errorf("%q took the visitor's ref over the destination's", got)
	}
	if !strings.Contains(got, "ref=house") {
		t.Errorf("%q lost the destination's own ref", got)
	}
	if !strings.Contains(got, "extra=1") {
		t.Errorf("%q dropped a non-conflicting incoming parameter", got)
	}
}
