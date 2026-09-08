package addon

import (
	"reflect"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
)

// valid is the manifest every case below mutates one field of, so a failing case
// names exactly one reason.
func valid() Manifest {
	return Manifest{
		SchemaVersion: SchemaVersion,
		Name:          "clickstats",
		Version:       "0.4.0",
		ABIVersion:    1,
		Module:        "clickstats.wasm",
		SHA256:        strings.Repeat("ab", 32),
		FailureClass:  ClassDegrade,
		Permissions:   []string{"storage.own_schema", "redirect.observe"},
		Settings: []Setting{
			{Name: "retention_days", Type: SettingText, Default: "30"},
			{Name: "api_token", Type: SettingSecret},
			{Name: "granularity", Type: SettingSelect, Options: []string{"hour", "day"}, Default: "day"},
			{Name: "count_bots", Type: SettingToggle, Default: "false"},
		},
		CookiePrefixes: []string{"clickstats_state"},
	}
}

func TestValidManifestValidates(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("the reference manifest does not validate: %v", err)
	}
}

func TestManifestValidationRules(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Manifest)
		want string
	}{
		{"a schema version this host does not implement", func(m *Manifest) {
			m.SchemaVersion = 2
		}, "schema_version is 2"},
		{"a missing schema version", func(m *Manifest) {
			m.SchemaVersion = 0
		}, "this host implements 1"},
		{"a name with a capital letter", func(m *Manifest) {
			m.Name = "ClickStats"
		}, "name \"ClickStats\""},
		{"a name with a hyphen, which no Postgres schema wants unquoted", func(m *Manifest) {
			m.Name = "click-stats"
		}, "name \"click-stats\""},
		{"a one-character name", func(m *Manifest) { m.Name = "a" }, "name \"a\""},
		{"no version", func(m *Manifest) { m.Version = "" }, "version \"\""},
		{"a version with a space", func(m *Manifest) {
			m.Version = "0.4.0 beta"
		}, "version \"0.4.0 beta\""},
		{"no ABI version", func(m *Manifest) { m.ABIVersion = 0 }, "abi_version is 0"},
		{"no module", func(m *Manifest) { m.Module = "" }, "module: must name"},
		{"a module that is a path out of the directory", func(m *Manifest) {
			m.Module = "../../etc/passwd.wasm"
		}, "must be a bare filename"},
		{"a module with a Windows separator", func(m *Manifest) {
			m.Module = `..\evil.wasm`
		}, "must be a bare filename"},
		{"a module that is not wasm", func(m *Manifest) {
			m.Module = "clickstats.so"
		}, "must end in .wasm"},
		{"a short digest", func(m *Manifest) { m.SHA256 = "abcd" }, "must be 64 hex characters"},
		{"a digest that is not hex", func(m *Manifest) {
			m.SHA256 = strings.Repeat("zz", 32)
		}, "not hex"},
		{"an uppercase digest", func(m *Manifest) {
			m.SHA256 = strings.Repeat("AB", 32)
		}, "must be lowercase"},
		{"no failure class", func(m *Manifest) { m.FailureClass = "" }, "there is no default"},
		{"a failure class nobody defined", func(m *Manifest) {
			m.FailureClass = "retry"
		}, "failure_class \"retry\""},
		{"a permission that is not dotted", func(m *Manifest) {
			m.Permissions = []string{"storage"}
		}, "is not a permission name"},
		{"a permission declared twice", func(m *Manifest) {
			m.Permissions = []string{"links.read", "links.read"}
		}, "declared twice"},
		{"a setting declared twice", func(m *Manifest) {
			m.Settings = []Setting{
				{Name: "mode", Type: SettingText},
				{Name: "mode", Type: SettingText},
			}
		}, "\"mode\" is declared twice"},
		{"a setting with no type", func(m *Manifest) {
			m.Settings = []Setting{{Name: "mode"}}
		}, "type is missing"},
		{"a setting type the manager cannot render", func(m *Manifest) {
			m.Settings = []Setting{{Name: "mode", Type: "textarea"}}
		}, "type \"textarea\" is not one of"},
		{"a select with one option", func(m *Manifest) {
			m.Settings = []Setting{{Name: "mode", Type: SettingSelect, Options: []string{"only"}}}
		}, "needs at least two options"},
		{"a select whose default is not an option", func(m *Manifest) {
			m.Settings = []Setting{{
				Name: "mode", Type: SettingSelect,
				Options: []string{"a", "b"}, Default: "c",
			}}
		}, "is not one of its options"},
		{"a text setting carrying options", func(m *Manifest) {
			m.Settings = []Setting{{Name: "mode", Type: SettingText, Options: []string{"a", "b"}}}
		}, "takes no options"},
		{"a toggle whose default is not a boolean", func(m *Manifest) {
			m.Settings = []Setting{{Name: "mode", Type: SettingToggle, Default: "yes"}}
		}, "takes a default of"},
		{"a secret with a default, which every installation would share", func(m *Manifest) {
			m.Settings = []Setting{{Name: "token", Type: SettingSecret, Default: "hunter2"}}
		}, "may not carry a default"},
		{"a cookie prefix outside the add-on's own namespace", func(m *Manifest) {
			m.CookiePrefixes = []string{"state"}
		}, "must begin with \"clickstats_\""},
		{"a cookie prefix claiming another add-on's namespace", func(m *Manifest) {
			m.CookiePrefixes = []string{"oidc_state"}
		}, "must begin with \"clickstats_\""},
		{"a cookie prefix in this product's own namespace", func(m *Manifest) {
			m.Name = "linkctrl"
			m.CookiePrefixes = []string{"linkctrl_session"}
		}, "reaches this product's own cookie namespace"},
		{"an empty cookie prefix, which is every cookie", func(m *Manifest) {
			m.CookiePrefixes = []string{""}
		}, "not a usable cookie-name prefix"},
		{"a one-character cookie prefix", func(m *Manifest) {
			m.CookiePrefixes = []string{"c"}
		}, "not a usable cookie-name prefix"},
		{"a cookie prefix with a capital letter", func(m *Manifest) {
			m.CookiePrefixes = []string{"clickstats_State"}
		}, "not a usable cookie-name prefix"},
		{"a cookie prefix declared twice", func(m *Manifest) {
			m.CookiePrefixes = []string{"clickstats_state", "clickstats_state"}
		}, "\"clickstats_state\" is declared twice"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := valid()
			tc.edit(&m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("%s validated", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not mention %q:\n%v", tc.want, err)
			}
		})
	}
}

// Validation is aggregated, for the reason config.Validate is: a publisher
// writing their first manifest should see the whole list.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	m := valid()
	m.Name = "Bad Name"
	m.Version = ""
	m.ABIVersion = 0
	m.FailureClass = ""

	err := m.Validate()
	if err == nil {
		t.Fatal("four problems validated")
	}
	for _, want := range []string{"name", "version", "abi_version", "failure_class"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the aggregated error does not mention %s:\n%v", want, err)
		}
	}
}

// A field this host does not know means a manifest written for a schema this host
// does not implement. Accepting it would instantiate a module whose author is
// expecting behaviour that will not happen.
func TestUnknownFieldsAreRefused(t *testing.T) {
	doc := `{
	  "schema_version": 1,
	  "name": "clickstats",
	  "version": "0.4.0",
	  "abi_version": 1,
	  "module": "clickstats.wasm",
	  "sha256": "` + strings.Repeat("ab", 32) + `",
	  "failure_class": "degrade",
	  "redirect_deadline_ms": 5
	}`
	if _, err := parseManifest(strings.NewReader(doc)); err == nil {
		t.Fatal("a manifest with an unknown field parsed")
	} else if !strings.Contains(err.Error(), "redirect_deadline_ms") {
		t.Errorf("the error does not name the unknown field:\n%v", err)
	}
}

func TestTrailingContentIsRefused(t *testing.T) {
	doc := `{"schema_version": 1, "name": "a"} {"schema_version": 1}`
	if _, err := parseManifest(strings.NewReader(doc)); err == nil {
		t.Fatal("two objects in one manifest parsed")
	}
}

// TestNoDeclarableCookiePrefixReachesThisProductsCookies is D232's boundary from
// the manifest side, and the falsifiable half of it: the session cookie's names
// are read from the package that sets them rather than copied here, so a rename
// there fails this test instead of quietly widening what an add-on may declare.
func TestNoDeclarableCookiePrefixReachesThisProductsCookies(t *testing.T) {
	hostCookies := []string{
		auth.SessionCookieName,
		auth.SessionCookieNameInsecure,
		// internal/httpx's appearance cookie, named rather than read: this package
		// must not import httpx, because M64 makes httpx import this one. It is
		// inside the same `linkctrl` namespace as the two above, which is what the
		// assertion below is really about.
		"linkctrl_theme",
	}

	// Every prefix of every one of them, because a prefix is what a manifest
	// declares and a short one reaches further than a long one.
	for _, name := range hostCookies {
		for i := 1; i <= len(name); i++ {
			if !reachesHostCookie(name[:i]) {
				t.Errorf("%q is a prefix of this product's cookie %q and the namespace list "+
					"does not catch it", name[:i], name)
			}
		}
	}

	// And the rule as a whole: the one add-on name that could otherwise own this
	// product's namespace cannot declare a prefix inside it, however it is spelled.
	for i := len("linkctrl_"); i <= len(auth.SessionCookieNameInsecure); i++ {
		prefix := auth.SessionCookieNameInsecure[:i]
		m := valid()
		m.Name = "linkctrl"
		m.CookiePrefixes = []string{prefix}
		if err := m.Validate(); err == nil {
			t.Errorf("a manifest named linkctrl declaring the cookie prefix %q validated", prefix)
		}
	}
}

// --- F286: the file says what the host reads ---------------------------------

// manifestDoc is a well-formed manifest as JSON text, with the given lines spliced
// in before the closing brace. Text rather than a marshalled struct, because every
// case below is about a spelling `encoding/json` will not produce.
func manifestDoc(extra ...string) string {
	lines := []string{
		`"schema_version": 1`,
		`"name": "clickstats"`,
		`"version": "0.4.0"`,
		`"abi_version": 1`,
		`"module": "clickstats.wasm"`,
		`"sha256": "` + strings.Repeat("ab", 32) + `"`,
		`"failure_class": "degrade"`,
	}
	return "{" + strings.Join(append(lines, extra...), ",\n") + "}"
}

// The reference document, so a refusal below is the spliced line's doing and not
// the frame's.
func TestTheReferenceDocumentParses(t *testing.T) {
	m, err := parseManifest(strings.NewReader(manifestDoc()))
	if err != nil {
		t.Fatalf("the reference manifest document does not parse: %v", err)
	}
	if m.Name != "clickstats" || m.SchemaVersion != SchemaVersion {
		t.Fatalf("parsed as %+v", m)
	}
}

// F286, and it is the whole battery the finding reproduced.
//
// `encoding/json` takes the **last** occurrence of a repeated key and binds a
// struct tag case-insensitively when no exact match exists, so every document
// here used to load — each of them meaning, to the host, something other than what
// its text says. The manifest is the artifact another repository compiles against
// and the thing a reviewer reads before installing, and nothing hashes its bytes,
// so the parse is the only place the two readings can be made to agree.
func TestAManifestMeansWhatItReadsAs(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			"permissions listed and then emptied, which used to grant nothing",
			manifestDoc(`"permissions": ["session.mint"]`, `"permissions": []`),
			`"permissions" appears more than once`,
		},
		{
			"permissions emptied and then listed, which used to grant everything",
			manifestDoc(`"permissions": []`, `"permissions": ["session.mint", "config.read"]`),
			`"permissions" appears more than once`,
		},
		{
			"a schema this host does not implement, repeated as one it does",
			`{"schema_version": 99, ` + manifestDoc()[1:],
			`"schema_version" appears more than once`,
		},
		{
			"a permission declaration spelled to be missed by a reader grepping for it",
			manifestDoc(`"Permissions": ["session.mint"]`),
			`key "Permissions" is not spelled`,
		},
		{
			"the schema version in capitals, alone",
			`{"SCHEMA_VERSION": 1, ` + manifestDoc()[len(`{"schema_version": 1,`):],
			`key "SCHEMA_VERSION" is not spelled`,
		},
		{
			"a migration naming one file to a reader and hashing another",
			manifestDoc(`"permissions": ["storage.own_schema"]`,
				`"migrations": [{"file": "001_a.sql", "file": "002_b.sql", "sha256": "`+sum+`"}]`),
			`"file" appears more than once`,
		},
		{
			"a setting whose type is spelled where no reader would look for it",
			manifestDoc(`"settings": [{"name": "mode", "TYPE": "secret"}]`),
			`key "TYPE" is not spelled`,
		},
		{
			"a cookie prefix declaration in capitals",
			manifestDoc(`"COOKIE_PREFIXES": ["clickstats_state"]`),
			`key "COOKIE_PREFIXES" is not spelled`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseManifest(strings.NewReader(tc.doc))
			if err == nil {
				t.Fatalf("the manifest parsed, as %+v", m)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not say %q:\n%v", tc.want, err)
			}
		})
	}
}

// The message has to name the spelling a publisher should have used, because
// "Permissions" and "permissions" differ by one character and the eye that wrote
// the first will not find it by staring.
func TestACaseVariantKeyIsToldItsDocumentedSpelling(t *testing.T) {
	_, err := parseManifest(strings.NewReader(manifestDoc(`"Cookie_Prefixes": []`)))
	if err == nil {
		t.Fatal("a case-variant key parsed")
	}
	if !strings.Contains(err.Error(), `write "cookie_prefixes"`) {
		t.Errorf("the error does not name the documented spelling:\n%v", err)
	}
	// And names the file once. The walk seeds its own path with the filename, so a
	// caller that wrapped it again produced `addon.json: addon.json: key
	// "Permissions" is not spelled…` — in a line an operator reads out of the boot
	// log, on the one refusal whose whole job is to be read carefully.
	if n := strings.Count(err.Error(), ManifestFile); n != 1 {
		t.Errorf("the error names %s %d times, want once:\n%v", ManifestFile, n, err)
	}
}

// The key rules hold at every nesting level, so the check is walked against the
// struct rather than listed — a field added to Manifest or Setting is covered
// without anybody remembering, and this asserts the derivation rather than the
// list. Every documented key of the two nested types is reachable.
func TestEveryDocumentedKeyIsExactlyOneSpelling(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[Manifest](), reflect.TypeFor[Setting](), reflect.TypeFor[MigrationFile](),
	} {
		fields := jsonFields(typ)
		if len(fields) != typ.NumField() {
			t.Errorf("%s has %d fields and %d documented keys", typ, typ.NumField(), len(fields))
		}
		for name := range fields {
			if name != strings.ToLower(name) {
				t.Errorf("%s documents the key %q, which no lowercase manifest can spell", typ, name)
			}
		}
	}
}

// The manifest is read into memory to be walked twice, so the size it may reach is
// now this parser's business rather than the filesystem's.
func TestAManifestLargerThanTheBoundIsRefused(t *testing.T) {
	padding := strings.Repeat("x", maxManifestBytes)
	doc := manifestDoc(`"version": "` + padding + `"`)
	if _, err := parseManifest(strings.NewReader(doc)); err == nil {
		t.Fatal("a manifest past the bound parsed")
	} else if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("the error does not say the file is too large:\n%v", err)
	}
}

// Every reserved add-on **name** is refused, walked off the list rather than
// spelled out here.
//
// The list has grown three times — `inline` and `instantiate` at M65, `pool` at
// M66.5, `fetch` and `route` at M68.5 — and each addition means an add-on in a
// directory of that name stops loading on upgrade. A test naming the entries would
// have had to grow with it and did not exist at all; this one cannot go stale,
// which is the property this phase keeps discovering it needs.
func TestEveryReservedNameIsRefusedAsAnAddonName(t *testing.T) {
	t.Parallel()
	if len(config.AddonReservedNames) < 5 {
		t.Fatalf("the reserved list holds %d names and was written with more",
			len(config.AddonReservedNames))
	}
	for _, name := range config.AddonReservedNames {
		m := valid()
		m.Name = name
		// Cleared, because a cookie prefix is derived from the name and would
		// otherwise be a second reason and a confusing message.
		m.CookiePrefixes = nil
		err := m.Validate()
		if err == nil {
			t.Errorf("a manifest named %q validates, and this product reads a variable "+
				"of its own from LINKCTRL_ADDON_%s_*", name, strings.ToUpper(name))
			continue
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%q is refused without saying it is reserved: %v", name, err)
		}
	}
}
