package addon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// This file is M68's settings limb: where a declared setting's value comes from,
// what saving one does, and why the two sources it can come from do not compete.
//
// # Two sources, and the environment wins
//
// D263 gave an add-on's settings one source — `LINKCTRL_ADDON_<NAME>_<SETTING>`,
// read at load. M68 adds a second, because the manager's detail page saves what an
// operator types, and a value typed into a page has to live somewhere the page can
// read it back from. So a declared setting now has a stored answer in
// `addon_settings` and possibly an environment answer as well, and something has to
// decide.
//
// **The environment wins, and the page says so in place of the control.** That is
// not a new rule; it is the one this product already applies to the only other
// value with these two sources. `LINKCTRL_UPDATE_CHECK=false` makes the first-run
// prompt render a sentence instead of a checkbox, because "an air-gapped instance
// must not appear to be asking a question it has already had answered for it"
// (internal/httpx/web.go, D149). A setting an operator pinned in the deployment's
// environment is the same shape: the answer is already given, the page cannot
// change it, and a form field whose value nothing would read is worse than no
// field. So [Host.resolveSettings] layers the environment on top of the store, and
// [SettingView.Source] is what the manager renders the field from.
//
// The reverse order was considered and is worse in the direction that matters. If
// the stored value won, an operator could silently override the deployment's own
// configuration from a web page — including a `required`-class authentication
// add-on's credentials — and the environment variable would keep sitting in the
// compose file describing something that was no longer true.
//
// # A save reaches instances that already exist
//
// Values were read once at load (D263) because `config_get` is on a request's path
// and must not read the environment per call. That is still true, and it is why
// [settingValues] exists: the map lives behind one atomic pointer, every copy of a
// hostState made by `forRequest` and every pooled instance holds the same holder,
// and a save swaps the map. `config_get` is still a pointer load and a map lookup.
//
// A guest that read a value *once* — during package initialization, which runs
// while the instance is being made — is the case the holder cannot reach, and
// [Host.SaveSettings] answers it by draining the add-on's instance pool. Without
// that, M66.5's kept instances make "next invocation" false for up to
// [DefaultPoolTTL]. The routed and page paths instantiate per request, so they
// need nothing.
//
// What it costs is stated in m68.md's third risk and is real: a saved value changes
// what a module does on its **next** invocation, and an invocation already inside
// the guest reads whatever it read. There is no transactional relationship between
// a form submission and a module's flow, and pretending otherwise would need a
// quiesce this product has no reason to build.

// settingValues is one add-on's configured values, replaceable while the add-on is
// running.
//
// The map inside is never mutated — a save builds a new one and swaps the pointer —
// so a reader that has loaded the pointer holds an immutable map and needs no lock.
type settingValues struct {
	m atomic.Pointer[map[string]config.Secret]
}

// newSettingValues wraps a resolved map. The argument is not retained by the
// caller anywhere else; it becomes the holder's first generation.
func newSettingValues(m map[string]config.Secret) *settingValues {
	sv := &settingValues{}
	sv.m.Store(&m)
	return sv
}

// get is what `config_get` asks. Nil-safe, because a hostState a test built by
// hand has no holder and an add-on with no configured settings must read exactly
// as one whose settings are all unset.
func (s *settingValues) get(key string) (config.Secret, bool) {
	if s == nil {
		return "", false
	}
	m := s.m.Load()
	if m == nil {
		return "", false
	}
	v, ok := (*m)[key]
	return v, ok
}

// len is how many settings have a value, which is what the boot log counts and
// what the manager reads to draw "set" beside a secret it must never echo.
func (s *settingValues) len() int {
	if s == nil {
		return 0
	}
	m := s.m.Load()
	if m == nil {
		return 0
	}
	return len(*m)
}

// replace swaps in a freshly resolved map. Called on a save, from the request's
// goroutine, while guests are reading the old one.
func (s *settingValues) replace(m map[string]config.Secret) {
	if s == nil {
		return
	}
	s.m.Store(&m)
}

// SettingSource says where the value the add-on will read came from.
//
// Three states rather than two, because "nothing has been set" and "the operator
// set it here" are different things to draw: the first offers an empty field, the
// second offers a field with a value in it and a way to clear it.
type SettingSource string

const (
	// SourceUnset means neither route has answered, so the add-on reads the
	// manifest's default or nothing at all.
	SourceUnset SettingSource = "unset"
	// SourceStored means the value came from `addon_settings` — somebody typed it
	// into the manager, and the manager can change it.
	SourceStored SettingSource = "stored"
	// SourceEnvironment means `LINKCTRL_ADDON_<NAME>_<SETTING>` is set. The field
	// is read-only and the page names the variable, for the reason the first-run
	// update-check prompt names its own (D149).
	SourceEnvironment SettingSource = "environment"
)

// SettingView is one declared setting as the Add-on manager renders it.
//
// **It never carries a secret's value**, whichever source it came from, and that
// is structural rather than a rule the template has to remember: the field does
// not exist on this type for a secret, because [Value] is populated only for the
// three types whose value is not a credential. `Configured` is what tells the page
// a secret has been set, which is the whole of what it may say about one.
//
// [Type] is the declared type **or** the type the stored value was written under,
// whichever withholds. See [Host.settingViews] for why a manifest cannot demote a
// stored credential to a text box by re-declaring it.
type SettingView struct {
	Name    string        `json:"name"`
	Type    SettingType   `json:"type"`
	Options []string      `json:"options,omitempty"`
	Default string        `json:"default,omitempty"`
	Source  SettingSource `json:"source"`
	// Value is the effective value for a text, select or toggle setting, and is
	// always empty for a secret.
	Value string `json:"value,omitempty"`
	// Configured is whether anything has answered this setting — the only thing
	// said about a secret that has a value.
	Configured bool `json:"configured"`
	// EnvVar is the variable that answered it, and is empty unless Source is
	// SourceEnvironment. Named so an operator who wants to change a pinned value
	// knows what to edit.
	EnvVar string `json:"env_var,omitempty"`
	// UpdatedAt is when the stored value was last written, and is nil for every
	// other source.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`

	// Origin is whether this setting names where the add-on may make outbound
	// requests (M68.5), and the page renders it differently for one reason: filling
	// it in **authorizes a server-side request from this instance** to whatever is
	// typed in it. m68.5.md's second risk is exactly that — *they name an origin and
	// thereby authorize a server-side request to it; if the page does not make that
	// consequence plain, the setting reads like a URL field*.
	//
	// It rides on the view rather than being derived from the name, because the
	// declaration is the manifest's and the page must not be guessing which of an
	// add-on's fields is the dangerous one.
	Origin bool `json:"origin,omitempty"`

	// SignIn marks the one setting on this page the host declares rather than the
	// add-on: the operator's consent to that add-on's link appearing on the
	// sign-in page (M69.5). The page renders it differently for the reason
	// [SettingView.Origin] does — turning it on changes what every visitor to this
	// instance sees before they have authenticated, which is not a consequence a
	// bare checkbox conveys.
	SignIn bool `json:"sign_in,omitempty"`
}

// Editable reports whether the manager may write this setting. False for one the
// environment answers, which the page renders as a sentence rather than a field.
func (v SettingView) Editable() bool { return v.Source != SourceEnvironment }

// IsText reports whether this setting is a plain text field.
//
// It and its three neighbours below are the type predicates the manager's
// template branches on. Methods rather than a comparison in the template,
// because [SettingType] is a named string type and `eq .Type "toggle"` compares
// a SettingType with an untyped constant — which text/template resolves at
// render time and gets wrong quietly. A method is checked by the compiler
// against the same vocabulary Manifest.Validate enforces.
func (v SettingView) IsText() bool { return v.Type == SettingText }

// IsSecret reports whether this setting is a credential. See [SettingView.IsText].
func (v SettingView) IsSecret() bool { return v.Type == SettingSecret }

// IsSelect reports whether this setting is a fixed choice. See [SettingView.IsText].
func (v SettingView) IsSelect() bool { return v.Type == SettingSelect }

// IsToggle reports whether this setting is a boolean. See [SettingView.IsText].
func (v SettingView) IsToggle() bool { return v.Type == SettingToggle }

// IsSignIn reports whether this setting is the operator's consent to an add-on's
// sign-in link. See [SettingView.IsText] for why these are methods.
func (v SettingView) IsSignIn() bool { return v.SignIn }

// IsOrigin reports whether this setting names where the add-on may reach. See
// [SettingView.IsText] for why these are methods.
func (v SettingView) IsOrigin() bool { return v.Origin }

// On is whether a toggle's box is ticked. False for every other type, so the
// template never has to ask twice.
func (v SettingView) On() bool { return v.IsToggle() && v.Value == "true" }

// storedSettings reads what the manager has saved for one add-on.
//
// A host with no database has none, and says so with nil rather than an error:
// that is a test's host and an add-on whose settings then come from the
// environment alone, which is exactly the pre-M68 behaviour.
func (h *Host) storedSettings(ctx context.Context, name string) map[string]dbgen.AddonSettingValuesRow {
	if h == nil || h.db == nil {
		return nil
	}
	rows, err := dbgen.New(h.db).AddonSettingValues(ctx, name)
	if err != nil {
		// Logged and skipped rather than failing the load. A `required` add-on whose
		// stored settings could not be read would otherwise stop an instance because
		// of a transient database error, and the add-on still has its environment
		// values and its manifest defaults — which is more than it had before this
		// table existed.
		h.log.Warn("an add-on's stored settings could not be read; it starts with "+
			"whatever the environment and its manifest give it",
			slog.String("addon", name), slog.Any("error", err))
		return nil
	}
	out := make(map[string]dbgen.AddonSettingValuesRow, len(rows))
	for _, r := range rows {
		out[r.Name] = r
	}
	return out
}

// envSettings is the operator's environment answers for one add-on, nil-safe.
//
// Nil-safe for the reason [Host.overrides] is: a *Host a test built by hand has
// no function here, and both of this file's readers reach it. An absent lookup is
// an add-on nothing was configured for in the environment, which is what an
// operator who set no variables has.
func (h *Host) envSettings(name string, declared []string) map[string]config.Secret {
	if h == nil || h.settings == nil {
		return nil
	}
	return h.settings(name, declared)
}

// resolveSettings is what an add-on reads: its stored values, with the
// environment's on top.
//
// Declared settings only, in both directions. A stored row for a setting the
// manifest no longer declares is not handed to the module — `config_get` would
// refuse the key anyway (D263) — and it is deliberately not deleted, so an add-on
// downgraded and re-upgraded finds what an operator typed still there.
func (h *Host) resolveSettings(ctx context.Context, m Manifest) map[string]config.Secret {
	declared := settingNames(m)
	return mergeSettings(declared, h.storedSettings(ctx, m.Name),
		h.envSettings(m.Name, declared))
}

// mergeSettings is the precedence itself, separated from where either side comes
// from.
//
// A function rather than two loops inside [Host.resolveSettings], because the
// direction is D347 and a rule worth a decision entry is worth being able to
// assert without a database and a process environment. The order of the two loops
// below *is* the decision: stored first, environment on top, so the environment
// wins.
func mergeSettings(
	declared []string,
	stored map[string]dbgen.AddonSettingValuesRow,
	env map[string]config.Secret,
) map[string]config.Secret {
	out := make(map[string]config.Secret, len(declared))
	for _, name := range declared {
		if row, ok := stored[name]; ok && row.Value != "" {
			out[name] = config.Secret(row.Value)
		}
	}
	for name, v := range env {
		out[name] = v
	}
	return out
}

// settingViews builds the render model for one loaded add-on's settings.
func (h *Host) settingViews(ctx context.Context, l Loaded) []SettingView {
	stored := h.storedSettings(ctx, l.Manifest.Name)
	env := h.envSettings(l.Manifest.Name, settingNames(l.Manifest))
	// The manifest's own list plus the host's sign-in consent toggle, when the
	// add-on asked for a link (M69.5). The manager is the one surface that renders
	// and saves both; what the *module* reads is [Host.resolveSettings], which
	// stays on the manifest's list alone.
	declared := managedSettings(l.Manifest)
	out := make([]SettingView, 0, len(declared))
	for _, s := range declared {
		v := SettingView{
			Name: s.Name, Type: s.Type, Options: slices.Clone(s.Options),
			Default: s.Default, Source: SourceUnset,
			// Read off the manifest in hand, unlike Type below: what an origin setting
			// is worth saying about is what the add-on *now* declares, and a stored
			// value written under an older manifest that did not mark it names no
			// origin the host will dial anyway.
			Origin: s.Origin,
			// Read off the name, because this one setting is not the manifest's: it is
			// the host's, added by managedSettings, and no manifest may declare the
			// name (Manifest.Validate).
			SignIn: s.Name == SignInConsentSetting,
		}
		switch row, hasStored := stored[s.Name]; {
		case env[s.Name] != "":
			v.Source, v.Configured = SourceEnvironment, true
			v.EnvVar = config.AddonSettingVar(l.Manifest.Name, s.Name)
			// Deliberately not revealed, and not only for a secret: the value is a
			// deployment's own configuration and the page has nothing to offer that
			// would let anybody change it here.
		case hasStored && row.Value != "":
			v.Source, v.Configured = SourceStored, true
			// **The stored row's own answer outranks the manifest's**, in the
			// withholding direction. A value written for a `secret` keeps the Secret
			// treatment whatever the manifest in hand now declares, which is what
			// makes "never echoed" a property of the column rather than of a
			// manifest's honesty — M67 made remove-then-install the documented way to
			// replace an add-on, so a successor re-declaring `client_secret` as
			// `text` is a path a person can walk rather than a hypothetical.
			//
			// It changes the *type* rather than only the value, because the two
			// cannot be separated: rendering a withheld value as a text box gives a
			// blank field, and a blank text field means *unset*, so the next save
			// would delete the credential without anybody asking for it. As a secret
			// the same blank means *keep what is stored*, which is the reading that
			// preserves it. Clearing one is still the checkbox, and after that the
			// setting is whatever the manifest says.
			if row.Secret {
				v.Type = SettingSecret
			}
			updated := row.UpdatedAt
			v.UpdatedAt = &updated
			if !v.IsSecret() {
				v.Value = row.Value
			}
		default:
			if s.Type != SettingSecret {
				v.Value = s.Default
			}
		}
		out = append(out, v)
	}
	return out
}

// SaveSettings writes the values an operator typed into the manager's detail page.
//
// # What it refuses
//
// A key the manifest does not declare, and a value its declared type does not
// admit: a `toggle` that is not `true` or `false`, a `select` that is not one of
// its options. Both are [domain.ValidationErrors], so the page puts the message
// beside the field and the API answers 422 — the same shape every other form in
// this product uses. A setting the environment answers is refused too, and that is
// the load-bearing one: without it the page would accept a value, store it, and
// change nothing about what the add-on reads.
//
// # One transaction, and a full replace of what was sent
//
// The form posts every editable setting, so a value that arrives empty means
// *unset* and its row is deleted rather than stored as an empty string — the
// environment route already reads a set-and-empty variable as unset, and two
// spellings of "no value" that behaved differently would be a trap. All of it is
// one transaction, so no `config_get` can observe half a form.
//
// The audit record names the settings the save **touched** and never their
// values. Touched rather than *changed*, and touched rather than *wrote*: the form
// carries every editable field on every submission, so what is recorded is the set
// this save reached — which includes a key that arrived empty and had its row
// deleted, because clearing a credential is the half of this operation an auditor
// would most want to find. That is the same reading `updated_at` takes and is
// defended in query/addonsettings.sql — the question asked of a configuration
// record is *when was this last touched* rather than *when did it last differ*,
// and one record answering one way while the column beside it answered the other
// would be two accounts of one act. A save that carried a subset — the API's PUT
// can — records that subset.
//
// No value is ever in it. A secret is the obvious reason; a non-secret is the same
// reason one step removed, because what an operator configures an add-on with is
// not a thing the audit log is a safe place for.
func (h *Host) SaveSettings(
	ctx context.Context, actor *auth.Identity, name string, values map[string]string,
) ([]SettingView, error) {
	if !actor.Can(auth.PermAddonsManage) {
		return nil, fmt.Errorf("%w: saving an add-on's settings requires %s",
			domain.ErrForbidden, auth.PermAddonsManage)
	}
	if h == nil || h.db == nil {
		return nil, ErrNoAddonDatabase
	}
	l := h.find(name)
	if l == nil {
		return nil, domain.ErrNotFound
	}
	managed := managedSettings(l.Manifest)
	declared := make(map[string]Setting, len(managed))
	for _, s := range managed {
		declared[s.Name] = s
	}
	env := h.envSettings(l.Manifest.Name, settingNames(l.Manifest))

	var verrs domain.ValidationErrors
	for key, raw := range values {
		s, ok := declared[key]
		if !ok {
			verrs = append(verrs, domain.FieldError{
				Field: key, Code: "unknown",
				Message: "this add-on's manifest declares no setting called " + strconv.Quote(key),
			})
			continue
		}
		if env[key] != "" {
			verrs = append(verrs, domain.FieldError{
				Field: key, Code: "read_only",
				Message: "this setting is answered by " +
					config.AddonSettingVar(l.Manifest.Name, key) +
					"; change it there and restart, or unset it to configure it here",
			})
			continue
		}
		// **Empty is *unset*, and unset is a legal state for every declared type.**
		// It is checked here rather than inside [checkSettingValue] because that
		// function answers *is this a value this setting may hold*, and the empty
		// string is not a value at all — it is the absence of one, which the write
		// below expresses as a deleted row. Without this a `select` could be set and
		// never unset: its check is membership of the option list, "" is in no
		// option list, and the form's own empty option would come back 422.
		if raw == "" {
			continue
		}
		if err := checkSettingValue(s, raw); err != nil {
			verrs = append(verrs, domain.FieldError{
				Field: key, Code: "invalid", Message: err.Error(),
			})
		}
	}
	if len(verrs) > 0 {
		return nil, verrs
	}

	tx, err := h.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := dbgen.New(tx)
	touched := make([]string, 0, len(values))
	for key, raw := range values {
		if raw == "" {
			if err := q.DeleteAddonSetting(ctx, dbgen.DeleteAddonSettingParams{
				Addon: l.Manifest.Name, Name: key,
			}); err != nil {
				return nil, fmt.Errorf("clear %s's %s setting: %w", l.Manifest.Name, key, err)
			}
		} else if err := q.SaveAddonSetting(ctx, dbgen.SaveAddonSettingParams{
			Addon: l.Manifest.Name, Name: key, Value: raw,
			// From the manifest in hand, which is what makes the column describe the
			// value being written rather than what an earlier manifest called it.
			// Withholding reads it in the other direction — see [Host.settingViews].
			Secret: declared[key].Type == SettingSecret,
		}); err != nil {
			return nil, fmt.Errorf("save %s's %s setting: %w", l.Manifest.Name, key, err)
		}
		touched = append(touched, key)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	slices.Sort(touched)

	// The running module's view, replaced from the committed state rather than
	// from the form: the environment still wins, and a setting the form did not
	// carry keeps whatever it had.
	l.settings.replace(h.resolveSettings(ctx, l.Manifest))

	// **And the pooled instances go, which is the other half of "on its next
	// invocation".**
	//
	// The holder above reaches every instance that asks — `config_get` is a pointer
	// load through it, so an instance built an hour ago reads the new value the
	// moment it calls. What it does not reach is a value the guest read *once* and
	// kept: package initialization runs during instantiation, and a module that
	// caches a setting there holds whatever it was instantiated with. M66.5 made
	// the redirect path keep instances rather than destroy them, so without this
	// such a module would go on using the old value until its pool entry aged out —
	// [DefaultPoolTTL], one minute — and the sentence the page shows after a save
	// would be wrong for as long.
	//
	// **It reaches the busy instances too**, which is what makes the sentence true
	// rather than nearly true. Emptying the idle set by itself reaches only what is
	// resting at this instant, and an add-on under traffic has its instances *out* —
	// so the one case the drain most needs to cover would have been the one it
	// missed. [poolEntry.gen] is what closes it: an entry made before this drain is
	// destroyed when its invocation releases it instead of going back into the pool.
	//
	// Draining costs the next redirect an instantiation, bounded by
	// `LINKCTRL_ADDON_INSTANTIATE_DEADLINE`, on an act an operator performs by hand.
	// Remove drains for the same reason from the *old* pool map; a save leaves the
	// set alone, so this one reads the current one.
	//
	// The routed and page paths need nothing: they instantiate per request already.
	h.drainPool(ctx, l.Manifest.Name, h.current().pools)

	h.log.Info("an add-on's settings were saved from the Add-on manager",
		slog.String("addon", l.Manifest.Name),
		// Names only. See this function's doc for why no value is here.
		slog.Any("settings", touched))
	h.recordSettings(ctx, actor, l.Manifest.Name, touched)
	return h.settingViews(ctx, *l), nil
}

// ErrNoAddonDatabase is what a host with no database answers to an act that needs
// one — saving an add-on's settings, and purging what a removed add-on left.
//
// Its own sentinel wrapping [domain.ErrUnavailable] for the reason
// [ErrNoAddonsDir] is: the caller did nothing wrong, and the operation would work
// on an instance that has one. In this product only a test builds such a host.
//
// Worded for the *host* rather than for either act, because it is answered by
// both. It said *an add-on's settings cannot be stored* while [Host.PurgeData]
// returned it, which told an operator the wrong thing about a drop — and it was
// never seen, because cmd/linkctrl opens the pools before it opens the host.
var ErrNoAddonDatabase = fmt.Errorf("%w: this instance has no database, so an "+
	"add-on's stored data cannot be reached", domain.ErrUnavailable)

// checkSettingValue holds a submitted value against what its type admits.
//
// The same vocabulary Manifest.Validate applies to a declared *default*, which is
// deliberate: a default and a configured value are the same kind of thing, and a
// select whose default must be one of its options but whose stored value need not
// be would be two rules for one field.
func checkSettingValue(s Setting, raw string) error {
	switch s.Type {
	case SettingToggle:
		if raw != "true" && raw != "false" {
			return errors.New(`a toggle is "true" or "false"`)
		}
	case SettingSelect:
		if !slices.Contains(s.Options, raw) {
			return fmt.Errorf("must be one of %v", s.Options)
		}
	case SettingText, SettingSecret:
		if len(raw) > MaxSettingValueBytes {
			return fmt.Errorf("at most %d bytes", MaxSettingValueBytes)
		}
	default:
		// Unreachable: Manifest.Validate refuses a type outside the four, so a
		// loaded add-on cannot declare one. Refused rather than accepted, because
		// the alternative is storing a value nothing knows how to render.
		return fmt.Errorf("%q is not a setting type this host renders", s.Type)
	}
	return nil
}

// MaxSettingValueBytes bounds one stored setting.
//
// Far above any credential — a JWK set pasted whole is a few kilobytes — and far
// below anything that makes a row worth worrying about. It exists because the
// column is unbounded text and the form body's own cap (`maxFormBytes`, 64 KiB)
// covers the whole submission rather than one field, so without this a single
// field could take all of it and the refusal would name the form rather than the
// setting.
const MaxSettingValueBytes = 8 << 10

// recordSettings writes the audit row for a save.
//
// Best-effort in the same shape [Host.record] is, and for the same reason: the
// settings are committed, and failing the operator's request because the record
// could not be written would leave them believing nothing was saved.
func (h *Host) recordSettings(ctx context.Context, actor *auth.Identity, name string, touched []string) {
	if h.auditor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := h.auditor.Record(ctx, actor, audit.Event{
		Action:     audit.ActionAddonSettingsSaved,
		TargetType: "addon",
		Metadata: map[string]any{
			"module":   name,
			"settings": touched,
		},
		InstanceWide: true,
	}); err != nil {
		h.log.Error("an add-on's settings were saved and the audit record was not written",
			slog.String("addon", name), slog.Any("error", err))
	}
}

// find is the loaded add-on with this name, or nil.
func (h *Host) find(name string) *Loaded {
	if h == nil {
		return nil
	}
	loaded := h.current().loaded
	i := slices.IndexFunc(loaded, func(l Loaded) bool { return l.Manifest.Name == name })
	if i < 0 {
		return nil
	}
	return &loaded[i]
}
