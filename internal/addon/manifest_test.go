package addon

import (
	"strings"
	"testing"
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
