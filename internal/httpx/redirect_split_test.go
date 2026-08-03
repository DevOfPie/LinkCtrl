package httpx

import (
	"testing"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/redirect"
)

// Weighted selection (M36), tested where the arithmetic lives.
//
// The end-to-end behaviour is in test/integration/split_test.go. What is here is
// the boundary arithmetic that a statistical test cannot see: a weight of zero,
// every weight zero, and the last arm being reachable at all — an off-by-one in
// the running sum would make the highest-weighted arm unreachable while leaving
// the distribution looking plausible.

func armsOf(n int) []redirect.Choice {
	out := make([]redirect.Choice, n)
	for i := range out {
		out[i] = redirect.Choice{ID: uuid.UUID{byte(i + 1)}, URL: "https://example.com/"}
	}
	return out
}

// TestWeightedPickReachesEveryPositiveArm is the off-by-one guard.
//
// Two arms weighted 1 and 1, sampled enough times that missing either is not
// variance. A `<=` where the walk has `<` — or a running sum that stops one arm
// short — fails here and would pass any test that only checked the proportions
// of a large split.
func TestWeightedPickReachesEveryPositiveArm(t *testing.T) {
	arms := armsOf(3)
	weights := []int32{1, 1, 1}

	seen := map[uuid.UUID]bool{}
	for range 300 {
		out := weightedPick(arms, weights)
		if !out.chosen {
			t.Fatal("weightedPick chose nothing with three positive weights")
		}
		seen[out.choice.ID] = true
	}
	if len(seen) != 3 {
		t.Errorf("only %d of 3 arms were ever chosen; one of them is unreachable", len(seen))
	}
}

// TestWeightedPickSkipsZeroWeightedArms is what "parked" means.
//
// Zero is a legitimate setting — a way to hold an arm out of a running test
// without deleting it and losing the clicks already attributed to its
// destination — so it must receive nothing rather than an equal share.
func TestWeightedPickSkipsZeroWeightedArms(t *testing.T) {
	arms := armsOf(2)
	weights := []int32{0, 5}
	for range 100 {
		out := weightedPick(arms, weights)
		if !out.chosen || out.choice.ID != arms[1].ID {
			t.Fatalf("a zero-weighted arm was chosen: %+v", out)
		}
	}
}

// TestWeightedPickChoosesNothingWhenEveryArmIsParked is the fall-through.
//
// Picking uniformly instead would invent traffic for arms whose owner set them
// to receive none; the caller falls through to the fallback or to the link's own
// destination, which is what the link did before the split existed.
func TestWeightedPickChoosesNothingWhenEveryArmIsParked(t *testing.T) {
	if out := weightedPick(armsOf(3), []int32{0, 0, 0}); out.chosen || out.failed {
		t.Errorf("weightedPick = %+v, want no choice and no failure", out)
	}
	if out := weightedPick(nil, nil); out.chosen || out.failed {
		t.Errorf("weightedPick over no arms = %+v", out)
	}
}
