package addon

import (
	"strings"
	"testing"
)

// mintingManifest is a manifest that has asked for a sign-in link and is
// otherwise the reference one. Every case below mutates one field of it.
func mintingManifest() Manifest {
	m := valid()
	m.Name = "oidc"
	m.Module = "oidc.wasm"
	m.CookiePrefixes = []string{"oidc_state"}
	m.Settings = nil
	m.Permissions = []string{PermissionRoutes, PermissionSessionMint}
	m.SignInLabel = "Sign in with Contoso"
	m.SignInPath = "start"
	return m
}

func TestAMintingAddonMayAskForASignInLink(t *testing.T) {
	if err := mintingManifest().Validate(); err != nil {
		t.Fatalf("the reference sign-in manifest does not validate: %v", err)
	}
}

// The other half of F359's accept-set, and the half that keeps it honest: the
// rule refuses by category rather than by a list, so the thing to guard is that
// it does not refuse a label a publisher would reasonably write. Accents, an
// em-dash, a registered mark, parentheses, digits and an ampersand are all
// ordinary in a button that names a company.
//
// Without this, tightening the rule further would look free.
func TestASignInLabelMayHoldTheCharactersARealNameNeeds(t *testing.T) {
	for _, label := range []string{
		"Sign in with Contoso",
		"Se connecter à Contoso",
		"Anmelden mit Contoso GmbH & Co.",
		"Sign in — Contoso® (SSO)",
		"Contoso ID 2.0",
		"Войти через Contoso",
		"使用 Contoso 登录",
	} {
		m := mintingManifest()
		m.SignInLabel = label
		if err := m.Validate(); err != nil {
			t.Errorf("sign_in_label %q was refused: %v", label, err)
		}
	}
}

// TestASignInDeclarationThatCannotBeHonouredIsRefused walks every shape the host
// will not compose a target from.
//
// **The four the milestone names are the four that matter** — `..`, an absolute
// URL, a scheme and a leading `/` — because each is an attempt to name a
// destination from a file that may only name a place inside the prefix the host
// already gave this add-on. The rest are the coherence rules: a label from an
// add-on that cannot sign anybody in, or that serves no routes, is a button
// leading somewhere this instance will answer 404 for.
func TestASignInDeclarationThatCannotBeHonouredIsRefused(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Manifest)
		want string
	}{
		{"a path that climbs out of the add-on's prefix", func(m *Manifest) {
			m.SignInPath = "../../dashboard"
		}, `must not contain ".."`},
		{"a dot segment", func(m *Manifest) { m.SignInPath = "./start" }, `must not contain ".."`},
		{"an absolute URL", func(m *Manifest) {
			m.SignInPath = "https://evil.example.com/start"
		}, "must be a path and not a URL"},
		{"a scheme with no host", func(m *Manifest) {
			m.SignInPath = "javascript:alert(1)"
		}, "must be a path and not a URL"},
		{"a leading separator", func(m *Manifest) {
			m.SignInPath = "/dashboard"
		}, "must be relative to this add-on's own prefix"},
		{"a protocol-relative reference", func(m *Manifest) {
			m.SignInPath = "//evil.example.com/start"
		}, "must be relative to this add-on's own prefix"},
		{"a backslash", func(m *Manifest) {
			m.SignInPath = `start\..\..\x`
		}, "single rooted path"},
		{"an embedded double separator", func(m *Manifest) {
			m.SignInPath = "start//x"
		}, "single rooted path"},
		{"a query string", func(m *Manifest) {
			m.SignInPath = "start?next=/dashboard"
		}, "no query, no fragment, no escape"},
		{"a fragment", func(m *Manifest) { m.SignInPath = "start#x" }, "no query, no fragment"},
		{"a percent escape", func(m *Manifest) {
			m.SignInPath = "%2e%2e%2fdashboard"
		}, "no query, no fragment"},
		{"a path longer than the bound", func(m *Manifest) {
			m.SignInPath = strings.Repeat("a", MaxSignInPathBytes+1)
		}, "at most 128"},
		{"a label with no path", func(m *Manifest) { m.SignInPath = "" },
			"must name the page inside this add-on's own prefix"},
		{"a path with no label", func(m *Manifest) { m.SignInLabel = "" },
			"the label is what draws the link"},
		{"a label longer than the bound", func(m *Manifest) {
			m.SignInLabel = strings.Repeat("x", MaxSignInLabelBytes+1)
		}, "at most 64"},
		{"a label carrying a newline", func(m *Manifest) {
			m.SignInLabel = "Sign in\nwith Contoso"
		}, "not a printable character"},
		// F359: the three the old control-character rule admitted, on a string drawn
		// on the unauthenticated sign-in page. Each is named separately rather than
		// as one case, because what makes them worth refusing differs — an override
		// makes the label read backwards, a zero-width space makes two labels
		// indistinguishable, and a line separator is a line break wearing a
		// different code point.
		{"a label carrying a right-to-left override", func(m *Manifest) {
			m.SignInLabel = "Sign in with \u202eosotnoC"
		}, "not a printable character"},
		{"a label carrying a zero-width space", func(m *Manifest) {
			m.SignInLabel = "Sign\u200b in with Contoso"
		}, "not a printable character"},
		{"a label carrying a line separator", func(m *Manifest) {
			m.SignInLabel = "Sign in\u2028with Contoso"
		}, "not a printable character"},
		{"a label carrying a non-breaking space", func(m *Manifest) {
			m.SignInLabel = "Sign\u00a0in with Contoso"
		}, "not a printable character"},
		{"a label from an add-on that cannot mint", func(m *Manifest) {
			m.Permissions = []string{PermissionRoutes}
		}, `"session.mint" is not`},
		{"a label from an add-on that serves no routes", func(m *Manifest) {
			m.Permissions = []string{PermissionSessionMint}
		}, `"routes.own_prefix" is not`},
		{"a setting taking the host's own consent name", func(m *Manifest) {
			m.Settings = []Setting{{Name: SignInConsentSetting, Type: SettingToggle, Default: "true"}}
		}, "is this host's own"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := mintingManifest()
			tc.edit(&m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("%s validated", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the reason\n got: %v\nwant substring: %q",
					err, tc.want)
			}
		})
	}
}

// TestNoSignInDeclarationIsStillAValidManifest is the compatibility half: every
// add-on published against schema 1 before this field existed declares neither,
// and neither does the reference manifest.
func TestNoSignInDeclarationIsStillAValidManifest(t *testing.T) {
	m := valid()
	if m.SignInLabel != "" || m.SignInPath != "" {
		t.Fatal("the reference manifest declares a sign-in link; this test asserts the absence")
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("a manifest declaring no sign-in link is refused: %v", err)
	}
	if _, ok := (Loaded{Manifest: m}).SignInHref(); ok {
		t.Error("a manifest declaring no sign-in link produced a link anyway")
	}
}

// TestTheHostComposesTheTargetAndHoldsItInsideThePrefix asserts the second half
// of the answer: validation is what a publisher hears, and this is what a visitor
// gets. Every case here is a [Loaded] built by hand, which is the only way past
// [Manifest.Validate] — and the point is that getting past it still draws nothing.
func TestTheHostComposesTheTargetAndHoldsItInsideThePrefix(t *testing.T) {
	tests := []struct {
		name  string
		label string
		path  string
		want  string
	}{
		{"a page inside the prefix", "Sign in", "start", "/addons/oidc/start"},
		{"a nested page", "Sign in", "auth/start", "/addons/oidc/auth/start"},
		{"a climb out of the prefix", "Sign in", "../../dashboard", ""},
		{"a climb into a neighbour whose name begins the same way", "Sign in", "../oidcx/x", ""},
		{"a climb to the prefix root itself", "Sign in", "..", ""},
		{"an absolute path", "Sign in", "/dashboard", "/addons/oidc/dashboard"},
		{"no path", "Sign in", "", ""},
		{"no label", "", "start", ""},
		{"a label past the bound", strings.Repeat("x", MaxSignInLabelBytes+1), "start", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := Loaded{Manifest: Manifest{
				Name: "oidc", SignInLabel: tc.label, SignInPath: tc.path,
			}}
			href, ok := l.SignInHref()
			if tc.want == "" {
				if ok {
					t.Fatalf("a link was drawn to %q", href)
				}
				return
			}
			if !ok {
				t.Fatalf("no link was drawn; %q was expected", tc.want)
			}
			if href != tc.want {
				t.Errorf("href is %q, want %q", href, tc.want)
			}
			if !strings.HasPrefix(href, l.PathPrefix()) {
				t.Errorf("%q is outside %q", href, l.PathPrefix())
			}
		})
	}
}

// TestTheLabelBoundIsAConstant holds the bound where the milestone requires it:
// in a constant with a test, rather than as a number in a template.
func TestTheLabelBoundIsAConstant(t *testing.T) {
	l := Loaded{Manifest: Manifest{
		Name: "oidc", SignInLabel: strings.Repeat("x", MaxSignInLabelBytes), SignInPath: "start",
	}}
	if _, ok := l.SignInLabel(); !ok {
		t.Errorf("a label of exactly %d bytes is refused; the bound is inclusive",
			MaxSignInLabelBytes)
	}
	l.Manifest.SignInLabel += "x"
	if _, ok := l.SignInLabel(); ok {
		t.Errorf("a label of %d bytes is accepted; the bound is %d",
			MaxSignInLabelBytes+1, MaxSignInLabelBytes)
	}
}

// TestTheConsentTogglesIsTheHostsAndDefaultsToOff asserts what the manager
// renders and saves: the manifest's own settings plus one the host added, off.
func TestTheConsentToggleIsTheHostsAndDefaultsToOff(t *testing.T) {
	asked := managedSettings(mintingManifest())
	if len(asked) != 1 {
		t.Fatalf("an add-on that asked for a link has %d managed settings, want 1", len(asked))
	}
	got := asked[0]
	if got.Name != SignInConsentSetting {
		t.Errorf("the host's setting is called %q, want %q", got.Name, SignInConsentSetting)
	}
	if got.Type != SettingToggle {
		t.Errorf("the consent is a %q, want %q", got.Type, SettingToggle)
	}
	if got.Default != "false" {
		t.Errorf("the consent defaults to %q; it is off until an operator agrees", got.Default)
	}

	quiet := valid()
	if n := len(managedSettings(quiet)); n != len(quiet.Settings) {
		t.Errorf("an add-on that asked for nothing has %d managed settings, want %d",
			n, len(quiet.Settings))
	}
	// And what the module reads is unchanged either way, which is what keeps the
	// operator's answer out of `config_get`.
	for _, name := range settingNames(mintingManifest()) {
		if name == SignInConsentSetting {
			t.Error("the consent is in what the module reads; it is this instance's " +
				"answer about its own sign-in page and no add-on's business")
		}
	}
}

// TestNothingIsOfferedWithoutAHost is the stock instance: no add-ons directory
// means no host, and a nil host answers nothing rather than panicking.
func TestNothingIsOfferedWithoutAHost(t *testing.T) {
	var h *Host
	if links := h.SignInLinks(t.Context()); links != nil {
		t.Errorf("an instance with no add-on host offers %v", links)
	}
}
