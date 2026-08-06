package domain

// Bot blocking (M32.5).
//
// This file holds the whole of the precedence rule, and it lives here rather
// than in internal/link or internal/redirect because both of those need it and
// neither may own it. The redirect path asks "does this link refuse bots"; the
// API and the dashboard ask "may this link's owner turn it off, and what is in
// effect right now". A copy on each side is two rules that agree until somebody
// edits one.
//
// Nothing here looks at a user agent. Deciding *whether* bots are refused is a
// policy question over two stored settings; deciding whether a given request is
// a bot is analytics.Classify's job, and keeping them apart is what stops a
// second classifier appearing next to a second precedence table.

// BotPolicy is a link's own bot-blocking setting.
//
// The zero value is the empty string and means Inherit, which matters because a
// cached snapshot written by a build that predates this feature decodes with
// the field absent. Reading that as "inherit" is right in both directions: it
// is the column default, and on such an instance no domain can be blocking
// anything yet either.
type BotPolicy string

const (
	// BotInherit takes the domain's answer. The default for every link.
	BotInherit BotPolicy = "inherit"
	// BotBlock refuses bots on this link whatever the domain says.
	BotBlock BotPolicy = "on"
	// BotAllow lets bots through — unless the domain enforces, which is the one
	// case a link cannot overrule and the reason enforcement exists.
	BotAllow BotPolicy = "off"
)

// DomainBotPolicy is the domain's setting, as one value.
//
// Stored as two booleans (`block_bots`, `block_bots_enforced`) because they
// answer two questions, and collapsed to three states here because that is what
// precedence actually branches on. The fourth combination of the two booleans —
// enforced without blocking — is refused by a CHECK constraint in migration
// 01800, so it never reaches this type.
type DomainBotPolicy string

const (
	// DomainBotsOff blocks nothing. The default, and the zero value for the
	// same snapshot-compatibility reason as BotPolicy.
	DomainBotsOff DomainBotPolicy = "off"
	// DomainBotsOn blocks bots on links that have not said otherwise.
	DomainBotsOn DomainBotPolicy = "on"
	// DomainBotsEnforced blocks bots on every link beneath it, including the
	// ones whose owners set BotAllow.
	DomainBotsEnforced DomainBotPolicy = "enforced"
)

// DomainBots folds the two stored booleans into the policy.
func DomainBots(blockBots, enforced bool) DomainBotPolicy {
	switch {
	case blockBots && enforced:
		return DomainBotsEnforced
	case blockBots:
		return DomainBotsOn
	default:
		return DomainBotsOff
	}
}

// Booleans is the inverse of DomainBots, for the surfaces that render or store
// the two switches rather than the folded value.
func (p DomainBotPolicy) Booleans() (blockBots, enforced bool) {
	switch p {
	case DomainBotsEnforced:
		return true, true
	case DomainBotsOn:
		return true, false
	case DomainBotsOff:
		return false, false
	default:
		return false, false
	}
}

// ParseBotPolicy reads a link setting from the wire, reporting whether it was
// one of the three. An empty string is Inherit rather than invalid, so an API
// client omitting the field and one sending "" mean the same thing.
func ParseBotPolicy(s string) (BotPolicy, bool) {
	switch BotPolicy(s) {
	case "", BotInherit:
		return BotInherit, true
	case BotBlock:
		return BotBlock, true
	case BotAllow:
		return BotAllow, true
	default:
		return BotInherit, false
	}
}

// BlocksBots reports whether a link refuses automated clients.
//
// All nine combinations, and the shape is worth stating because it is not
// symmetric: the domain wins only when it enforces. An enforcing domain
// overrides a link that says off — that is the entire purpose of enforcement,
// and it must hold for rows written before enforcement was switched on, which
// is why the override lives here and not only in the validation that refuses
// new ones.
//
//	link \ domain   off     on      enforced
//	inherit         false   true    true
//	on              true    true    true
//	off             false   false   true
//
// Unknown values in either argument fall to the safe reading — the link's is
// treated as inherit, the domain's as off — because the only way one arrives is
// a cached payload from a build that did not have this field, and refusing
// traffic on the strength of a value this build cannot interpret would be a
// worse answer than the behaviour that build already had.
func BlocksBots(link BotPolicy, dom DomainBotPolicy) bool {
	if dom == DomainBotsEnforced {
		return true
	}
	switch link {
	case BotBlock:
		return true
	case BotAllow:
		return false
	default:
		// BotInherit, the empty string, and anything this build cannot read.
		return dom == DomainBotsOn
	}
}

// BotPolicyLocked reports whether a link's own setting is being overridden.
//
// The API refuses an explicit BotAllow while this holds, and the dashboard
// disables the control, rather than accepting a value that BlocksBots would
// then ignore. Storing a setting that does nothing is how a link owner comes to
// believe they turned something off.
func BotPolicyLocked(dom DomainBotPolicy) bool { return dom == DomainBotsEnforced }
