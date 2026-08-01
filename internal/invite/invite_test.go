package invite

import (
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
)

// statusOf is the whole of an invitation's state machine, and it is derived
// rather than stored — so the precedence between its three columns is asserted
// here rather than inferred from a list query.
func TestStatusOf(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }

	tests := []struct {
		name              string
		revoked, redeemed *time.Time
		expires           time.Time
		want              string
	}{
		{"live", nil, nil, now.Add(time.Hour), StatusPending},
		{"lapsed", nil, nil, now.Add(-time.Second), StatusExpired},
		// The instant of expiry is expired, not pending: the redemption query
		// requires expires_at > now(), so a boundary that disagreed would have
		// the list showing one thing and redemption doing another.
		{"exactly at expiry", nil, nil, now, StatusExpired},
		{"revoked", at(-time.Hour), nil, now.Add(time.Hour), StatusRevoked},
		{"redeemed", nil, at(-time.Hour), now.Add(time.Hour), StatusRedeemed},
		// Revoked beats redeemed beats expired. A revoked invitation that also
		// lapsed reads revoked, because that is what somebody did to it, and an
		// accepted one that has since lapsed is not "expired" — it produced a
		// member.
		{"revoked and lapsed", at(-time.Hour), nil, now.Add(-time.Hour), StatusRevoked},
		{"redeemed and lapsed", nil, at(-time.Hour), now.Add(-time.Hour), StatusRedeemed},
		{"revoked and redeemed", at(-time.Hour), at(-2 * time.Hour), now, StatusRevoked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusOf(tt.revoked, tt.redeemed, tt.expires, now); got != tt.want {
				t.Errorf("statusOf = %q, want %q", got, tt.want)
			}
		})
	}
}

// The copyable link is the whole delivery path on a mail-free instance, so its
// shape is asserted rather than left to whoever reads it out of a response.
func TestLinkFor(t *testing.T) {
	for _, base := range []string{"https://links.example.com", "https://links.example.com/"} {
		s := &Service{cfg: Config{AppURL: base}}
		if got := s.linkFor("abc"); got != "https://links.example.com/invite/abc" {
			t.Errorf("linkFor with AppURL %q = %q", base, got)
		}
	}
}

// A hasher is required rather than defaulted. Redemption both verifies and
// creates passwords, and one this package invented for itself would use costs
// the operator did not choose — and a nil one would panic on the first
// redemption, which is a stranger's first contact with the product.
func TestNewServiceRequiresAHasher(t *testing.T) {
	if _, err := NewService(nil, Config{}); err == nil {
		t.Error("a service was built with no hasher")
	}
	if _, err := NewService(nil, Config{Hasher: auth.NewHasher(auth.Params{})}); err != nil {
		t.Errorf("a service with a hasher was refused: %v", err)
	}
}

// D29 refused "no expiry" outright, so a Config built by hand cannot end up
// meaning it. Configuration validation refuses a non-positive value before this
// is reached; this is the second line.
func TestNonPositiveTTLFallsBackToAWeek(t *testing.T) {
	s, err := NewService(nil, Config{Hasher: auth.NewHasher(auth.Params{})})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.TTL != 168*time.Hour {
		t.Errorf("TTL = %s, want the documented 168h default", s.cfg.TTL)
	}
}

// The default role is the least powerful one. A caller that did not think about
// it must admit somebody who can do the least, not the most.
func TestDefaultRoleIsTheLeastPowerful(t *testing.T) {
	if DefaultRole != "viewer" {
		t.Errorf("DefaultRole = %q; omitting a role must not hand out more than "+
			"the caller meant to", DefaultRole)
	}
}
