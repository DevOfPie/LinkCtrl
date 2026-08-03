package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Campaigns (M41).
//
// A campaign is a label a link carries, and the whole of what it does is let a
// workspace ask "which links belong to this piece of work" and get a list. It
// stores a name, a URL-safe slug that is unique in the workspace, a description
// and an optional schedule.
//
// **What it deliberately does not do is analytics.** There is no campaign
// rollup, no per-campaign click series and no aggregate anywhere. Rolling up a
// campaign means a new pass over `click_events` grouped by a column that is null
// on most links, stacking load on the rollup job M37 has just fixed — and
// Plan.md keeps it in Phase 2+ until that fix has been watched at scale. The
// links list filtered by campaign is what the product answers with meanwhile,
// and it is the same query the folder filter uses.
//
// **The schedule is descriptive, not enforced.** `starts_at` and `ends_at`
// record when the work runs; nothing consults them at redirect time, and a link
// in a finished campaign redirects exactly as it did before. Expiry is what
// stops a link working and it is a property of the link — putting a second,
// weaker one on the campaign would give two answers to "why did this stop", and
// the redirect path would have to read a second table to find out.

// MaxCampaignNameLength bounds a campaign name, in runes. The same 64 a folder
// name and a tag name get: they sit in the same lists and are read the same way.
const MaxCampaignNameLength = 64

// MaxCampaignSlugLength bounds the slug. Shorter than the name, because a slug
// is what goes in a filter URL and a query string that wraps is one nobody
// copies correctly.
const MaxCampaignSlugLength = 48

// MaxCampaignDescriptionLength bounds the description, in runes.
const MaxCampaignDescriptionLength = 500

// MaxCampaignsPerWorkspace bounds the list.
//
// The campaigns page and every campaign `<select>` load all of them in one
// query, exactly as the folder tree does, so this is the number that keeps those
// unpaginated. A workspace wanting more than this is labelling links at a
// granularity tags already serve.
const MaxCampaignsPerWorkspace = 500

// Campaign is one campaign as the product understands it.
//
// LinkCount is computed rather than stored, like Folder.LinkCount, so it cannot
// drift from the links that actually carry the campaign.
type Campaign struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	// StartsAt and EndsAt describe the schedule and enforce nothing. See the
	// package comment above.
	StartsAt  *time.Time `json:"starts_at,omitempty"`
	EndsAt    *time.Time `json:"ends_at,omitempty"`
	LinkCount int64      `json:"link_count"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Active reports whether now falls inside the campaign's schedule. A campaign
// with no bounds is always active. Presentation only — nothing authorizes on it.
func (c Campaign) Active(now time.Time) bool {
	if c.StartsAt != nil && now.Before(*c.StartsAt) {
		return false
	}
	if c.EndsAt != nil && !now.Before(*c.EndsAt) {
		return false
	}
	return true
}

// ValidateCampaignName trims and checks a name.
func ValidateCampaignName(name string) (string, ValidationErrors) {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return "", ValidationErrors{{
			Field: "name", Code: "required", Message: "a campaign needs a name",
		}}
	case utf8.RuneCountInString(trimmed) > MaxCampaignNameLength:
		return "", ValidationErrors{{
			Field: "name", Code: "too_long",
			Message: fmt.Sprintf("a campaign name may be at most %d characters",
				MaxCampaignNameLength),
		}}
	case strings.ContainsFunc(trimmed, isControl):
		return "", ValidationErrors{{
			Field: "name", Code: "invalid",
			Message: "a campaign name may not contain control characters",
		}}
	}
	return trimmed, nil
}

// ValidateCampaignSlug normalizes and checks a slug, deriving one from the name
// when none is given.
//
// **Lowercased here rather than only in the index.** `campaigns_workspace_slug_key`
// is on `lower(slug)`, so "Summer" and "summer" already collide; storing the
// case somebody typed would mean two campaigns whose slugs look different, one
// of which cannot be created. Folding on the way in makes the stored value and
// the constraint agree.
func ValidateCampaignSlug(slug, name string) (string, ValidationErrors) {
	s := strings.TrimSpace(slug)
	if s == "" {
		s = SlugifyCampaign(name)
	} else {
		s = SlugifyCampaign(s)
	}
	switch {
	case s == "":
		return "", ValidationErrors{{
			Field: "slug", Code: "required",
			Message: "a campaign slug is made of letters, numbers and hyphens; " +
				"the name given has none of them, so one has to be typed",
		}}
	case len(s) > MaxCampaignSlugLength:
		return "", ValidationErrors{{
			Field: "slug", Code: "too_long",
			Message: fmt.Sprintf("a campaign slug may be at most %d characters",
				MaxCampaignSlugLength),
		}}
	}
	return s, nil
}

// ValidateCampaignSchedule checks the two bounds against each other.
func ValidateCampaignSchedule(starts, ends *time.Time) ValidationErrors {
	if starts == nil || ends == nil {
		return nil
	}
	if !ends.After(*starts) {
		return ValidationErrors{{
			Field: "ends_at", Code: "invalid",
			Message: "a campaign ends after it starts",
		}}
	}
	return nil
}

// ValidateCampaignDescription trims and bounds the description.
func ValidateCampaignDescription(desc string) (string, ValidationErrors) {
	trimmed := strings.TrimSpace(desc)
	if utf8.RuneCountInString(trimmed) > MaxCampaignDescriptionLength {
		return "", ValidationErrors{{
			Field: "description", Code: "too_long",
			Message: fmt.Sprintf("a campaign description may be at most %d characters",
				MaxCampaignDescriptionLength),
		}}
	}
	return trimmed, nil
}

// SlugifyCampaign reduces a string to lowercase letters, digits and single
// hyphens.
//
// ASCII-only by construction: anything outside `a-z0-9` becomes a separator, so
// a name written in a non-Latin script slugs to the empty string and the caller
// is asked for a slug rather than handed a percent-encoded one. A slug is what
// goes in a URL and in a `?campaign=` filter, and one that survives being
// copied out of a browser bar is worth more than one that preserves the name.
func SlugifyCampaign(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastHyphen := true // leading hyphens are dropped
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
