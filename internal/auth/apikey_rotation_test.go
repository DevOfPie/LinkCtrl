package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// The rule the whole feature rests on: a rotation cannot add a scope.
//
// If this stopped holding, `apikeys.*` being non-delegable would still be true
// and still be worthless — a key granted `links.read` could rotate itself into
// one holding everything its owner does, and the ceiling `resolveScopes`
// enforces at mint time would apply to nothing after the first generation.
func TestRotationCannotWidenScopes(t *testing.T) {
	held := []string{"links.read", "links.create"}

	cases := map[string][]string{
		"a scope the key never held":      {"links.read", "links.delete"},
		"only a scope the key never held": {"members.write"},
		"the owner's whole vocabulary":    {"links.read", "links.create", "org.delete"},
	}

	for name, requested := range cases {
		t.Run(name, func(t *testing.T) {
			scopes, errs := narrowScopes(held, requested)
			if len(errs) == 0 {
				t.Fatalf("rotating %v into %v was accepted, producing %v; a successor is "+
					"identical or narrower, and a key that can widen itself makes every "+
					"scope ceiling a one-generation guarantee", held, requested, scopes)
			}
			for _, fe := range errs {
				if fe.Field != "scopes" {
					t.Errorf("refusal names field %q, want scopes", fe.Field)
				}
			}
		})
	}
}

func TestRotationNarrowsAndInherits(t *testing.T) {
	held := []string{"links.read", "links.create", "tags.read"}

	// Nil means identical. Not "the actor's current permissions", which would
	// silently shrink a key whose owner had been demoted since it was minted.
	same, errs := narrowScopes(held, nil)
	if len(errs) > 0 {
		t.Fatalf("rotating with no scopes named was refused: %v", errs)
	}
	if len(same) != len(held) {
		t.Errorf("inherited %v, want %v", same, held)
	}

	// The successor must not alias the predecessor's slice: the caller writes
	// this into a new row, and a shared backing array is the kind of bug that
	// only shows up once something sorts it.
	same[0] = "mutated"
	if held[0] == "mutated" {
		t.Error("the inherited scopes alias the predecessor's slice")
	}

	fewer, errs := narrowScopes(held, []string{"links.read", "links.read", " tags.read "})
	if len(errs) > 0 {
		t.Fatalf("narrowing was refused: %v", errs)
	}
	if len(fewer) != 2 {
		t.Errorf("narrowed to %v, want two scopes with the duplicate collapsed and the padding trimmed", fewer)
	}
}

// A permission that became non-delegable after the key was minted must not ride
// a rotation into a new credential.
//
// This is the one place rotation deliberately refuses to copy. `apikeys.*` has
// been non-delegable since it existed so no key holds it today, but the set has
// grown four times in Phase 2 — destinations.review, webhooks.write,
// automation.write, audit.read — and each of those additions bound only new
// keys. Rotation is where an old key would otherwise carry one forward forever.
func TestRotationRefusesToCarryANowNonDelegableScope(t *testing.T) {
	held := []string{"links.read", "webhooks.write"}

	if _, errs := narrowScopes(held, nil); len(errs) == 0 {
		t.Error("a key holding webhooks.write rotated into a successor holding it too; " +
			"a scope that stopped being delegable must not survive a rotation")
	}
	// Named explicitly, the rotation succeeds without it: the refusal has to
	// leave a way forward, or the key is stranded.
	scopes, errs := narrowScopes(held, []string{"links.read"})
	if len(errs) == 0 {
		t.Fatalf("dropping the refused scope was still refused; got %v", scopes)
	}
}

func TestRotationRefusesAnEmptySuccessor(t *testing.T) {
	_, errs := narrowScopes([]string{"links.read"}, []string{"  "})
	if len(errs) != 1 || errs[0].Code != "required" {
		t.Errorf("rotating into nothing gave %v, want one required error: a successor "+
			"with no scopes could do nothing, and revoking is how to say that", errs)
	}
}

// The successor gets the predecessor's lifetime, measured from now — not its
// deadline, which would make rotating an expiring key pointless.
func TestSuccessorInheritsLifetimeNotDeadline(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	if got := successorExpiry(created, nil, now); got != nil {
		t.Errorf("a key that never expires rotated into one expiring %s", got)
	}

	deadline := created.Add(90 * 24 * time.Hour)
	got := successorExpiry(created, &deadline, now)
	if got == nil {
		t.Fatal("a key with an expiry rotated into one with none; the deadline must survive rotation")
	}
	if want := now.Add(90 * 24 * time.Hour); !got.Equal(want) {
		t.Errorf("successor expires %s, want %s (the predecessor's 90-day lifetime, from now)", got, want)
	}
	if !got.After(deadline) {
		t.Error("the successor expires no later than the key it replaced, which makes rotation pointless")
	}
}

// The grace window's floor exists because of `last_used_at`, and the two numbers
// are only meaningful together. This asserts the relationship the documentation
// claims rather than restating either number.
func TestMinimumGraceOutlastsTheUsageFlushTolerance(t *testing.T) {
	const flushTolerance = 30 * time.Second // APIKeyConfig's default UsageFlushInterval

	if MinRotationGrace <= flushTolerance {
		t.Errorf("MinRotationGrace is %s and last_used_at is flushed every %s, so the "+
			"grace window can close before anybody can tell whether the old key is "+
			"still in use", MinRotationGrace, flushTolerance)
	}
	if MinRotationGrace < 10*flushTolerance {
		t.Errorf("MinRotationGrace is %s, under ten flush intervals (%s); the floor is "+
			"documented as an order of magnitude above the tolerance",
			MinRotationGrace, 10*flushTolerance)
	}
	if DefaultRotationGrace < MinRotationGrace || DefaultRotationGrace > MaxRotationGrace {
		t.Errorf("the default grace %s is outside its own bounds [%s, %s]",
			DefaultRotationGrace, MinRotationGrace, MaxRotationGrace)
	}
	if MaxRotationGrace > 24*time.Hour {
		t.Errorf("MaxRotationGrace is %s; the ceiling is what keeps D9's accepted trade "+
			"finite, so a key rotated away must not outlive the day it was rotated on",
			MaxRotationGrace)
	}
}

// A session cannot rotate, and the refusal happens before anything touches the
// database — which is why this needs no pool.
func TestOnlyAKeyRotatesItself(t *testing.T) {
	svc := &APIKeyService{}

	if _, err := svc.Rotate(t.Context(), nil, RotateAPIKeyInput{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("rotating with no identity = %v, want unauthorized", err)
	}

	session := &Identity{permissions: map[string]struct{}{PermAPIKeysWrite: {}}}
	if _, err := svc.Rotate(t.Context(), session, RotateAPIKeyInput{}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a signed-in session holding %s rotated a key: %v. Rotation replaces the "+
			"credential that made the request, and a session is not one",
			PermAPIKeysWrite, err)
	}
}

func TestRotationGraceMustBeInRange(t *testing.T) {
	keyID := uuid.Must(uuid.NewV7())
	actor := &Identity{APIKeyID: &keyID}
	svc := &APIKeyService{}

	for _, grace := range []time.Duration{
		time.Second,
		MinRotationGrace - time.Second,
		MaxRotationGrace + time.Second,
		30 * 24 * time.Hour,
		-time.Hour,
	} {
		_, err := svc.Rotate(t.Context(), actor, RotateAPIKeyInput{Grace: grace})
		var ve domain.ValidationErrors
		if !errors.As(err, &ve) {
			t.Errorf("a grace window of %s gave %v, want a validation error", grace, err)
			continue
		}
		if ve[0].Field != "grace" {
			t.Errorf("refusal for %s names field %q, want grace", grace, ve[0].Field)
		}
	}
}
