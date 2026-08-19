package addon

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// ManifestFile is the name every add-on directory must hold. Fixed rather than
// configurable: discovery has to be able to tell an add-on from a directory the
// operator happened to leave there, and a name it looks for is the cheapest test
// that does not involve reading arbitrary files.
const ManifestFile = "addon.json"

// SchemaVersion is the manifest schema this host understands, and it is checked
// for equality rather than for "at least".
//
// Versioned from the first commit, before any add-on exists, because the manifest
// is the first artifact that crosses a repository boundary — the OIDC add-on in
// DevOfPie/LinkCtrl-OIDC is built against it — and a schema that acquires its
// version field later cannot describe the manifests written before it. The cost
// is one integer per file; retrofitting it is a breaking change to every add-on
// published in the meantime. See m60.md's third risk.
const SchemaVersion = 1

// FailureClass is what an add-on declares should happen when it will not load.
//
// Declared by the add-on and not by the operator, which is the owner's answer of
// 2026-08-18: the module's author knows whether the instance is still the product
// without it, and an operator installing an authentication provider does not
// necessarily know that sign-in stops if it is skipped.
type FailureClass string

const (
	// ClassRequired stops the instance, naming the add-on and the reason.
	ClassRequired FailureClass = "required"
	// ClassDegrade logs, counts, and lets the instance serve without the add-on.
	ClassDegrade FailureClass = "degrade"
)

// SettingType is the input the host renders for a declared setting.
//
// Four, and exactly the four the owner named when the Add-on manager's detail
// page took a Settings section (2026-08-18). M60 parses and stores them; M68
// renders them and saves the values. Nothing here is a UI decision — it is the
// vocabulary M68 is allowed to meet, fixed now so an add-on published against
// this schema cannot describe an input the manager will not draw.
type SettingType string

const (
	SettingText   SettingType = "text"
	SettingSecret SettingType = "secret"
	SettingSelect SettingType = "select"
	SettingToggle SettingType = "toggle"
)

// Setting is one declared configuration value.
//
// Options is meaningful only for SettingSelect — the owner's term was
// "select-with-options", so the options travel with the declaration rather than
// being fetched from the add-on at render time. Default is a string for every
// type, including toggle, because a manifest is JSON written by hand and one
// representation is one fewer thing for a publisher to get wrong; the validation
// below is what makes a toggle's default honest.
type Setting struct {
	Name    string      `json:"name"`
	Type    SettingType `json:"type"`
	Options []string    `json:"options,omitempty"`
	Default string      `json:"default,omitempty"`
}

// Manifest is an add-on's identity and its intent, read before its code is.
//
// Every field is consumed by a later milestone and none is decorative:
// ABIVersion by M61, Permissions by M62, Settings by M68. They are parsed and
// stored here so the file format is settled once, in the milestone that
// publishes it, rather than growing a field per milestone across a boundary
// another repository is already compiling against.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Version       string `json:"version"`

	// ABIVersion is which host ABI the module was built against. Parsed now,
	// consumed at M61, where an unsupported value becomes a refusal.
	ABIVersion int `json:"abi_version"`

	// Module is the .wasm this manifest accompanies, as a bare filename resolved
	// inside the add-on's own directory. Validation refuses a separator, so a
	// manifest cannot name a path out of it.
	Module string `json:"module"`

	// SHA256 is the digest of Module, lowercase hex. This is the load-time half
	// of the owner's "both" answer on build verification; the published-
	// provenance half belongs to the add-on's release process and is M69's.
	SHA256 string `json:"sha256"`

	FailureClass FailureClass `json:"failure_class"`

	// Permissions is what the add-on says it needs. Stored and not interpreted:
	// the vocabulary and the enforcement are M62's, and inventing either here
	// would mean M62 arriving to find its own decision already made.
	Permissions []string `json:"permissions,omitempty"`

	Settings []Setting `json:"settings,omitempty"`
}

// nameRe bounds an add-on's name to what can be three things at once: a
// Prometheus label value, a Postgres schema name (M63 gives each add-on a schema
// of its own), and a directory name on any filesystem an operator might deploy
// on. The intersection is narrower than any of them alone, which is why it is
// stated once here instead of at each of the three sites.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,30}$`)

// permissionRe matches this product's own permission spelling — dotted,
// lowercase, links.read — because an add-on's declared permissions will be read
// beside API-key scopes in the same manager page, and two spellings for the same
// idea is a thing somebody has to hold in their head forever.
//
// The vocabulary is deliberately not checked. M62 decides which tokens exist;
// this refuses only shapes no vocabulary would want.
var permissionRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// versionRe is loose on purpose. An add-on's own version is its author's
// business — the SemVer promise the owner set is about the ABI, not about every
// module's release numbering — so this refuses only what would make a log line or
// a metric label unreadable.
var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,31}$`)

// ReadManifest reads and validates the manifest in an add-on's directory.
//
// Unknown fields are refused. That is the strict choice and it is deliberate:
// SchemaVersion is checked for equality, so a manifest carrying a field this host
// does not know was written for a schema this host does not implement, and
// accepting it would mean instantiating a module whose author expects behaviour
// that will not happen. A publisher who needs a new field needs a new schema
// version, which is what the field is for.
func ReadManifest(dir string) (Manifest, error) {
	// The operator names the directory; the filename is this package's constant, so
	// the only variable part is the path they configured. That is the feature.
	f, err := os.Open(filepath.Join(dir, ManifestFile)) //nolint:gosec // G304: operator-supplied directory, by design
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = f.Close() }()
	return parseManifest(f)
}

func parseManifest(r io.Reader) (Manifest, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	// A second document in the same file is a manifest whose author believed
	// something this host will not read.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("%s: trailing content after the manifest object", ManifestFile)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate reports every problem with a manifest at once.
//
// Aggregated rather than fail-on-first, for the reason config.Validate is: the
// person reading the output is publishing an add-on for the first time and should
// see the whole list, not discover it one boot at a time.
func (m Manifest) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if m.SchemaVersion != SchemaVersion {
		// Reported, and then nothing else is: every rule below belongs to this
		// schema, so listing violations of it against a manifest that declares a
		// different one would be noise about the wrong contract.
		return fmt.Errorf("schema_version is %d; this host implements %d",
			m.SchemaVersion, SchemaVersion)
	}

	if !nameRe.MatchString(m.Name) {
		add("name %q: must be 2 to 31 characters, lowercase letters, digits and "+
			"underscores, starting with a letter — it names a metric label and, "+
			"from M63, a Postgres schema", m.Name)
	}
	if !versionRe.MatchString(m.Version) {
		add("version %q: must be 1 to 32 characters of letters, digits, dot, plus, "+
			"underscore or hyphen", m.Version)
	}
	if m.ABIVersion < 1 {
		add("abi_version is %d: must be at least 1", m.ABIVersion)
	}

	switch {
	case m.Module == "":
		add("module: must name the .wasm file this manifest accompanies")
	case strings.ContainsRune(m.Module, '/'), strings.ContainsRune(m.Module, '\\'),
		m.Module == ".", m.Module == "..":
		// The whole path-traversal answer, and it is a refusal rather than a
		// cleaning step: a manifest naming ../../etc/anything is not a manifest
		// with a fixable path, it is one that should never load.
		add("module %q: must be a bare filename inside the add-on's own directory", m.Module)
	case !strings.HasSuffix(m.Module, ".wasm"):
		add("module %q: must end in .wasm", m.Module)
	}

	switch {
	case len(m.SHA256) != 64:
		add("sha256 %q: must be 64 hex characters", m.SHA256)
	case strings.ToLower(m.SHA256) != m.SHA256:
		// Lowercase is required rather than folded, so the digest in a manifest is
		// byte-identical to what sha256sum prints and a publisher comparing the
		// two by eye is comparing the same string.
		add("sha256 %q: must be lowercase", m.SHA256)
	default:
		if _, err := hex.DecodeString(m.SHA256); err != nil {
			add("sha256 %q: not hex: %v", m.SHA256, err)
		}
	}

	switch m.FailureClass {
	case ClassRequired, ClassDegrade:
	case "":
		add("failure_class: must be %q or %q; there is no default, because guessing "+
			"it decides whether an instance boots without an add-on somebody's "+
			"sign-in depends on", ClassRequired, ClassDegrade)
	default:
		add("failure_class %q: must be %q or %q", m.FailureClass, ClassRequired, ClassDegrade)
	}

	seenPerm := make(map[string]bool, len(m.Permissions))
	for _, p := range m.Permissions {
		if !permissionRe.MatchString(p) {
			add("permissions: %q is not a permission name; expected dotted "+
				"lowercase, as in links.read", p)
			continue
		}
		if seenPerm[p] {
			add("permissions: %q is declared twice", p)
		}
		seenPerm[p] = true
	}

	seenSetting := make(map[string]bool, len(m.Settings))
	for _, s := range m.Settings {
		switch {
		case !nameRe.MatchString(s.Name):
			add("settings: %q is not a usable setting name; same shape as an "+
				"add-on name", s.Name)
		case seenSetting[s.Name]:
			add("settings: %q is declared twice", s.Name)
		}
		seenSetting[s.Name] = true
		errs = append(errs, s.validate()...)
	}

	return errors.Join(errs...)
}

func (s Setting) validate() []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	noOptions := func() {
		if len(s.Options) > 0 {
			add("settings %q: type %q takes no options", s.Name, s.Type)
		}
	}

	switch s.Type {
	case SettingSelect:
		// Two, not one: a select with a single option is a value the manifest
		// could have stated itself, and rendering it asks somebody to make a
		// choice that does not exist.
		if len(s.Options) < 2 {
			add("settings %q: type %q needs at least two options", s.Name, s.Type)
		}
		for _, o := range s.Options {
			if o == "" {
				add("settings %q: an option may not be empty", s.Name)
			}
		}
		if s.Default != "" && len(s.Options) > 0 && !slices.Contains(s.Options, s.Default) {
			add("settings %q: default %q is not one of its options", s.Name, s.Default)
		}
	case SettingToggle:
		noOptions()
		if s.Default != "" && s.Default != "true" && s.Default != "false" {
			add("settings %q: type %q takes a default of \"true\" or \"false\", got %q",
				s.Name, s.Type, s.Default)
		}
	case SettingSecret:
		noOptions()
		// A default secret is a secret every installation of the add-on shares,
		// which is the failure mode of every product that ever shipped one. The
		// manager will not echo the value back (M68); it must not put one there to
		// begin with.
		if s.Default != "" {
			add("settings %q: type %q may not carry a default", s.Name, s.Type)
		}
	case SettingText:
		noOptions()
	case "":
		add("settings %q: type is missing; one of %q, %q, %q, %q",
			s.Name, SettingText, SettingSecret, SettingSelect, SettingToggle)
	default:
		add("settings %q: type %q is not one of %q, %q, %q, %q",
			s.Name, s.Type, SettingText, SettingSecret, SettingSelect, SettingToggle)
	}
	return errs
}
