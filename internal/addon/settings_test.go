package addon

import (
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// The settings limb of M68, tested without a database.
//
// What needs one — a save landing in `addon_settings`, and a module reading it
// back through `config_get` on its next invocation — is in
// test/integration/addon_settings_test.go, because the claim there is about a
// round trip through Postgres and a wasm guest. What is here is the arithmetic
// either side of that: which source wins, what a value is allowed to be, and what
// a rendered setting is allowed to say.

// The whole of D347 in one test: a setting the environment answers is what the
// add-on reads, whatever is stored, and the manager renders it as read-only.
//
// Both halves matter and they fail differently. If the precedence were wrong, an
// operator could override their own deployment's configuration from a web page.
// If the *rendering* were wrong — an editable field over a value the environment
// wins — the page would accept a write that changed nothing an add-on reads,
// which is the worse of the two because it looks like it worked.
func TestTheEnvironmentOutranksAStoredSetting(t *testing.T) {
	h := hostWithEnv(map[string]string{"issuer": "https://from-the-environment"})
	m := Manifest{Name: "oidc", Settings: []Setting{
		{Name: "issuer", Type: SettingText},
		{Name: "scope", Type: SettingText, Default: "openid"},
	}}

	// The merge itself, with a stored answer for the same key. It is asserted
	// through mergeSettings rather than through resolveSettings because the stored
	// half comes from Postgres, and the direction of the precedence is not a claim
	// that should need a database to check.
	values := mergeSettings(settingNames(m),
		map[string]dbgen.AddonSettingValuesRow{
			"issuer": {Name: "issuer", Value: "https://typed-into-the-page"},
			"scope":  {Name: "scope", Value: "openid email"},
		},
		h.envSettings(m.Name, settingNames(m)))
	if got := values["issuer"].Reveal(); got != "https://from-the-environment" {
		t.Errorf("the add-on reads %q for issuer; the environment has to win, or a "+
			"page can override the deployment's own configuration", got)
	}
	if got := values["scope"].Reveal(); got != "openid email" {
		t.Errorf("the add-on reads %q for scope; a setting the environment does not "+
			"answer has to come from the store, or saving one changes nothing", got)
	}

	views := h.settingViews(t.Context(), Loaded{Manifest: m})
	byName := map[string]SettingView{}
	for _, v := range views {
		byName[v.Name] = v
	}
	issuer := byName["issuer"]
	if issuer.Source != SourceEnvironment || issuer.Editable() {
		t.Errorf("issuer renders as %s and editable=%v; a field whose write nothing "+
			"would read is worse than no field", issuer.Source, issuer.Editable())
	}
	if issuer.EnvVar != "LINKCTRL_ADDON_OIDC_ISSUER" {
		t.Errorf("the page names %q as the variable to edit", issuer.EnvVar)
	}
	if issuer.Value != "" {
		t.Errorf("the page renders the environment's value %q; there is nothing here "+
			"that could change it and it may be a credential", issuer.Value)
	}
	if scope := byName["scope"]; scope.Source != SourceUnset || scope.Value != "openid" {
		t.Errorf("an unanswered setting renders as %s/%q; it should offer its "+
			"declared default", scope.Source, scope.Value)
	}
}

// A secret's value never leaves this package through a view, whichever source
// answered it and whatever the manifest called the neighbouring fields.
//
// Structural rather than careful: [SettingView.Value] is not populated for the
// type. The test exists because "the template does not print it" is a claim about
// a template, and there are two of those plus a JSON encoder.
func TestASecretsValueIsNeverInAView(t *testing.T) {
	h := hostWithEnv(map[string]string{"client_secret": "s3cr3t-from-env"})
	m := Manifest{Name: "oidc", Settings: []Setting{
		{Name: "client_secret", Type: SettingSecret},
	}}
	for _, v := range h.settingViews(t.Context(), Loaded{Manifest: m}) {
		if strings.Contains(v.Value, "s3cr3t") {
			t.Errorf("the view carries the secret's value: %q", v.Value)
		}
		if !v.Configured {
			t.Error("the view says the secret is unset; the page then offers no hint " +
				"that a value exists, which is the other half of the claim")
		}
	}
}

// What a submitted value is allowed to be, against each declared type.
//
// The select and toggle cases are the ones worth having: a stored value outside a
// select's options would be read back by `config_get` and handed to a module that
// has no branch for it, and a toggle holding "yes" is the same defect with a
// friendlier spelling.
func TestASettingsValueIsCheckedAgainstItsType(t *testing.T) {
	toggle := Setting{Name: "pkce", Type: SettingToggle}
	sel := Setting{Name: "scope", Type: SettingSelect, Options: []string{"openid", "email"}}
	text := Setting{Name: "issuer", Type: SettingText}

	for _, tc := range []struct {
		name    string
		setting Setting
		value   string
		wantErr bool
	}{
		{"a toggle that is true", toggle, "true", false},
		{"a toggle that is false", toggle, "false", false},
		{"a toggle spelled yes", toggle, "yes", true},
		{"a select inside its options", sel, "email", false},
		{"a select outside them", sel, "profile", true},
		{"ordinary text", text, "https://idp.example", false},
		{"text past the bound", text, strings.Repeat("x", MaxSettingValueBytes+1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSettingValue(tc.setting, tc.value)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkSettingValue(%q) = %v, wantErr %v", tc.value, err, tc.wantErr)
			}
		})
	}
}

// A save reaches an instance that already exists, which is the whole reason the
// values sit behind a holder rather than in a map on each copy.
//
// forRequest copies the hostState by value — that is what makes a per-request
// instance's state its own — so a map field would have left every pooled and
// in-flight instance reading the values it was built with until the add-on was
// reloaded. The assertion is against a *copy*, because the original reading the
// new value proves nothing.
func TestASavedValueReachesAnInstanceAlreadyBuilt(t *testing.T) {
	holder := newSettingValues(map[string]config.Secret{"issuer": "before"})
	base := &hostState{values: holder}
	inFlight := base.forRequest(&Request{}, SessionContext{}, RequestIn{})

	holder.replace(map[string]config.Secret{"issuer": "after"})

	if v, ok := inFlight.values.get("issuer"); !ok || v.Reveal() != "after" {
		t.Errorf("an instance built before the save reads %q; a module has to see the "+
			"new value on its next invocation", v.Reveal())
	}
}

// The holder is read from guest goroutines and written from a request's, so the
// race detector has to have seen both happen at once.
func TestSettingValuesSurviveConcurrentReadsAndWrites(t *testing.T) {
	holder := newSettingValues(map[string]config.Secret{"k": "0"})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_, _ = holder.get("k")
				_ = holder.len()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 200 {
			holder.replace(map[string]config.Secret{"k": config.Secret(string(rune('a' + i%26)))})
		}
	}()
	wg.Wait()
}

// A nil holder answers as an add-on with nothing configured, because a hostState
// built by hand in a test has no holder and must not read differently from a
// loaded add-on whose settings are all unset.
func TestANilSettingHolderReadsAsUnset(t *testing.T) {
	var holder *settingValues
	if _, ok := holder.get("anything"); ok {
		t.Error("a nil holder answered a key")
	}
	if holder.len() != 0 {
		t.Error("a nil holder counted values it does not have")
	}
	holder.replace(map[string]config.Secret{"k": "v"})
}

// hostWithEnv is a *Host with no database and a substituted environment, which is
// every case above: what is being tested is the resolution, and the store's half
// of it is nil for an instance that has no database at all.
func hostWithEnv(env map[string]string) *Host {
	return &Host{
		log: slog.New(slog.DiscardHandler),
		settings: func(_ string, declared []string) map[string]config.Secret {
			out := map[string]config.Secret{}
			for _, name := range declared {
				if v, ok := env[name]; ok && v != "" {
					out[name] = config.Secret(v)
				}
			}
			return out
		},
	}
}
