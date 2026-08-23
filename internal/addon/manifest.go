package addon

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/config"
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

// MigrationsDir is the directory inside an add-on's own directory that holds its
// DDL. Fixed rather than configurable, for the reason [ManifestFile] is: the host
// has to be able to find it without being told, and a name is the cheapest test.
const MigrationsDir = "migrations"

// MigrationFile is one migration this add-on ships, and the digest of it.
type MigrationFile struct {
	// File is a bare filename inside the add-on's `migrations/` directory. The
	// same refusal the module gets: a separator, a dot entry or a name that is not
	// `.sql` is a manifest that should not load rather than a path to clean up.
	File string `json:"file"`
	// SHA256 is the digest of that file, lowercase hex — byte-identical to what
	// `sha256sum` prints, for the reason [Manifest.SHA256] is lowercase.
	SHA256 string `json:"sha256"`
}

// Manifest is an add-on's identity and its intent, read before its code is.
//
// Every field is consumed by a later milestone and none is decorative:
// ABIVersion by M61, Permissions by M62, Settings by M68, CookiePrefixes by
// M64. They are parsed and stored here so the file format is settled once, in
// the milestone that publishes it, rather than growing a field per milestone
// across a boundary another repository is already compiling against —
// CookiePrefixes being the exception that proves the cost, added by M61 the
// commit after this schema was written, because the ABI record it bounds was
// published in the same commit and a field is cheapest to get right before
// anything is built against it (D232).
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

	// Permissions is what the add-on says it needs, and it is the whole of what it
	// may do: the vocabulary is abi.Permissions, every host function names the
	// grant it costs, and a call whose grant is not declared here is refused
	// (M62). Validation below refuses a token outside the vocabulary; whether a
	// declared one is actually *held* is resolveGrants', because a permission can
	// exist and be grantable by no host yet.
	Permissions []string `json:"permissions,omitempty"`

	Settings []Setting `json:"settings,omitempty"`

	// Migrations is the DDL this add-on ships, one entry per file, each with the
	// digest of the file it names.
	//
	// **Enumerated with a digest each rather than summarised**, and both halves
	// earn their place. The digest extends M60's answer from the module to the
	// DDL: the host runs these statements, an operator did not write them, and
	// the manifest is what makes them *the add-on author's* rather than whatever
	// is on disk. Enumerating closes the set — a `.sql` file present in the
	// directory and absent here refuses the add-on, so nothing can be added to
	// what the host will execute without editing the manifest that describes it.
	// A publisher computes these with the same `sha256sum` they already used for
	// the module, which is why it is a digest per file and not one aggregate over
	// a canonical ordering nobody could reproduce by hand (D247).
	//
	// Files live in the add-on's own `migrations/` directory and are goose SQL:
	// the host applies them at load, inside the schema the add-on owns, with the
	// add-on's own role. An add-on that lists any of these must also declare
	// `storage.own_schema`; validation refuses the pair being incoherent.
	Migrations []MigrationFile `json:"migrations,omitempty"`

	// CookiePrefixes is the cookie namespace this add-on owns: the request record
	// M64 hands it carries the cookies whose names begin with one of these and no
	// others, and the cookies it may set are bounded the same way.
	//
	// Declared rather than granted wholesale because this product's sessions are
	// server-side and opaque, which makes the Cookie header the credential itself
	// — an add-on handed it verbatim could act as whoever is signed in. Owner-set
	// 2026-08-18 (D232). Parsed and validated here, where the manifest's other
	// declarations live; consumed at M64, which is where a request first reaches
	// an add-on.
	CookiePrefixes []string `json:"cookie_prefixes,omitempty"`
}

// nameRe bounds an add-on's name to what can be three things at once: a
// Prometheus label value, a Postgres schema name (M63 gives each add-on a schema
// of its own), and a directory name on any filesystem an operator might deploy
// on. The intersection is narrower than any of them alone, which is why it is
// stated once here instead of at each of the three sites.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,30}$`)

// permissionRe matches this product's own permission spelling — dotted,
// lowercase, links.read — because an add-on's declared permissions are read
// beside API-key scopes in the same manager page, and two spellings for the same
// idea is a thing somebody has to hold in their head forever.
//
// The shape is checked before the vocabulary so that a typo reads as a typo. The
// vocabulary itself is abi.Permissions and is **closed** (M62): a token outside it
// refuses the add-on, for the reason DisallowUnknownFields refuses an unknown
// field — a declaration this host cannot interpret is a manifest whose author
// expects behaviour that will not happen, and there is no safe direction to guess
// in. A token that is in the vocabulary and not grantable on this host
// is a different case and loads: see resolveGrants. There is none today —
// `redirect.inline` was the last, until M66 admitted an add-on onto the redirect
// path — and the shape stays because it is what lets a class be declared a
// milestone before it works.
var permissionRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// migrationFileRe is the filename shape goose reads a version out of: digits, an
// underscore, then anything. Checked here rather than left to goose, because a
// file goose cannot version is one it silently ignores — and a migration that
// silently never ran is the worst available failure for DDL.
var migrationFileRe = regexp.MustCompile(`^[0-9]+_[^\n]*\.sql$`)

// cookiePrefixRe bounds a declared cookie prefix to the shape a cookie name can
// have here. Narrower than RFC 6265's token, and deliberately the same alphabet
// as an add-on's name, because every prefix has to begin with one.
var cookiePrefixRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

// hostCookieNamespaces is every prefix a cookie this product sets itself begins
// with: `linkctrl_session` and `linkctrl_theme` today, the first of them also
// spelled `__Host-linkctrl_session` on a secure deployment. No add-on may
// declare a prefix that reaches into one, which is the half of D232's answer the
// name rule below cannot supply on its own — an add-on *named* `linkctrl` would
// otherwise own the namespace this product is already in.
//
// A test asserts the session cookie's real names, read from internal/auth
// rather than copied, are caught by this list, so the list cannot quietly stop
// covering the credential it exists for.
var hostCookieNamespaces = []string{"linkctrl", "__Host-", "__Secure-"}

// reachesHostCookie reports whether a declared prefix could match a cookie of
// this product's. Both directions of the comparison: a prefix inside a
// namespace, and a prefix so short that a namespace is inside *it* — the second
// is unreachable while every prefix must begin with the add-on's own name and
// underscore, and it is checked anyway because that rule is not this function's
// to rely on.
func reachesHostCookie(prefix string) bool {
	for _, ns := range hostCookieNamespaces {
		if strings.HasPrefix(prefix, ns) || strings.HasPrefix(ns, prefix) {
			return true
		}
	}
	return false
}

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

// maxManifestBytes bounds the manifest this host will read, and it is documented
// where a publisher reads — docs/configuration.md's manifest section, beside the
// other refusals — because meeting an undocumented bound as a boot failure is the
// same experience as a bug.
//
// It exists because the file is read into memory to be walked twice — once decoded
// into [Manifest], once as a token stream by [checkManifestKeys] — and a reader
// that streamed is not one an author can make allocate whatever they like. A real
// manifest is a few hundred bytes; the largest this schema can honestly describe —
// a `migrations` list of a few hundred entries — is under 40 KiB, so 64 KiB is
// past anything the format can mean and short of anything worth worrying about.
//
// **Its own number, not [maxStringIn]'s**, though the two are the same 64 KiB.
// They govern unrelated contracts: maxStringIn is the ABI's published bound on one
// value crossing from a guest into the host (docs/SECURITY.md), and this is what a
// publisher's manifest may weigh. Aliasing them made one constant the definition
// of both, so a change made for an ABI reason would silently move what manifests
// this host accepts — and a change here would move a number the ABI publishes.
// Equal by coincidence is not the same as equal by contract.
const maxManifestBytes = 64 << 10

func parseManifest(r io.Reader) (Manifest, error) {
	// One extra byte, so a file that is exactly at the bound is told apart from one
	// that ran past it.
	data, err := io.ReadAll(io.LimitReader(r, maxManifestBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if len(data) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("%s: larger than %d bytes; a manifest describes an "+
			"add-on and is not a place to carry data", ManifestFile, maxManifestBytes)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
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
	// After the decode and before Validate, because it is a rule about the *bytes*
	// and the decode is what proves they are a manifest at all. See below for why
	// a successful decode is not enough on its own.
	//
	// Returned unwrapped: the walk seeds its path with ManifestFile and names every
	// level below it, so every message already begins `addon.json`. Wrapping it
	// again said the filename twice in a line an operator reads out of the boot log.
	if err := checkManifestKeys(data); err != nil {
		return Manifest{}, err
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// checkManifestKeys refuses a manifest whose keys are not, exactly and once each,
// the keys this schema documents.
//
// **A successful decode is not enough**, which is F286 and the reason this
// function exists. `encoding/json` takes the **last** occurrence of a repeated key
// and matches a struct tag case-insensitively when no exact match exists, so
// `{"permissions": ["session.mint"], "permissions": []}` decodes to no
// permissions and `{"SCHEMA_VERSION": 7, "schema_version": 1}` decodes to schema
// 1. Both files load. Neither says what it does to anyone who reads it — and the
// manifest is read by more parties than this host: it is the artifact another
// repository is compiled against, and the thing a reviewer inspects before
// installing an add-on on their own instance.
//
// **Nothing covers the manifest's own bytes.** The `sha256` field is the digest of
// the *module* and `MigrationFile.SHA256` covers the DDL; there is no
// canonicalization and no signature over `addon.json` itself, and the published-
// provenance half of that is M69's. So the only thing standing between the file's
// readable text and what the host acts on is this parse, and it has to be the
// same reading a person gets.
//
// Two rules, and the first is the one that makes the second cheap:
//
//   - **A key is spelled exactly as documented.** This is the rule
//     DisallowUnknownFields already states — a key this host does not know was
//     written for a schema it does not implement — applied to spelling rather than
//     only to identity, since a second accepted spelling of `permissions` is a
//     second way to write the format another repository compiles against.
//   - **A key appears once.** Exact repeats, which the rule above leaves as the
//     only remaining collision.
//
// The walk is type-directed and recursive, so it holds at every nesting level and
// not only at the top: a `migrations` entry carrying `file` twice would otherwise
// name one `.sql` to a reader and hash another, which is the same deception one
// level down and the one with DDL behind it.
func checkManifestKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return checkKeys(dec, reflect.TypeFor[Manifest](), ManifestFile)
}

// checkKeys walks one JSON value against the Go type the manifest decodes it into.
//
// Reached only after a successful decode, so the document is well-formed and every
// key resolves to a field. Anything this function cannot recognise — a value where
// the type says object, a kind with no keys under it — is skipped rather than
// refused: the decode has already had its say about shape, and a second opinion
// here could only disagree with it.
func checkKeys(dec *json.Decoder, typ reflect.Type, path string) error {
	switch typ.Kind() {
	case reflect.Struct:
		return checkObject(dec, typ, path)
	case reflect.Slice, reflect.Array:
		return checkArray(dec, typ.Elem(), path)
	default:
		// A scalar, or a named string type like FailureClass. Consumed whole so the
		// token stream stays aligned with the walk.
		return dec.Decode(new(json.RawMessage))
	}
}

func checkArray(dec *json.Decoder, elem reflect.Type, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		// null, or something the decode already accepted for this field.
		return nil
	}
	for i := 0; dec.More(); i++ {
		if err := checkKeys(dec, elem, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	_, err = dec.Token() // the closing ]
	return err
}

func checkObject(dec *json.Decoder, typ reflect.Type, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil // null
	}
	fields := jsonFields(typ)
	seen := make(map[string]bool, len(fields))
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kt.(string)
		if !ok {
			return nil // unreachable in well-formed JSON
		}
		field, exact := fields[key]
		switch {
		case !exact:
			// Not spelled as documented. The decode accepted it, which means it bound
			// a field case-insensitively — the only way an unknown key survives
			// DisallowUnknownFields — so the field it bound is nameable and the
			// message says which, because a publisher looking at `"Permissions"` will
			// not otherwise see what is wrong with it.
			return fmt.Errorf("%s: key %q is not spelled as this schema documents it; "+
				"write %q — keys are matched exactly, so one field has one spelling",
				path, key, documentedSpelling(fields, key))
		case seen[key]:
			return fmt.Errorf("%s: key %q appears more than once; JSON keeps the last "+
				"occurrence, so the file would not mean what it reads as", path, key)
		}
		seen[key] = true
		if err := checkKeys(dec, field, path+"."+key); err != nil {
			return err
		}
	}
	_, err = dec.Token() // the closing }
	return err
}

// jsonFields is the exact key set for a struct, mapped to the type behind each.
//
// Read from the struct tags rather than listed, so a field added to [Manifest]
// or [Setting] is covered by this check without anybody remembering to add it
// here — the failure of a list would be silent and would look exactly like the
// defect this file is fixing.
func jsonFields(typ reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, typ.NumField())
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}
		if name == "-" {
			continue
		}
		out[name] = f.Type
	}
	return out
}

// documentedSpelling is the key the given one differs from only in case. It is
// only ever called for a key the decode bound, so one exists; the fallback is
// there because this function is not the place to discover otherwise.
func documentedSpelling(fields map[string]reflect.Type, key string) string {
	for name := range fields {
		if strings.EqualFold(name, key) {
			return name
		}
	}
	return strings.ToLower(key)
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
	if slices.Contains(config.AddonReservedNames, m.Name) {
		// The other half of the LINKCTRL_ADDON_<NAME>_<X> namespace's ambiguity, and
		// the same answer the reserved *setting* names get: an add-on named `inline`
		// with a setting called `deadline` would read the instance-wide
		// LINKCTRL_ADDON_INLINE_DEADLINE, and no lookup could tell which was meant.
		add("name %q is reserved: this product reads a variable of its own from the "+
			"%s namespace under that name, and a setting of yours would be read from "+
			"the same variable. The reserved names are %s", m.Name,
			config.AddonEnvPrefix, strings.Join(config.AddonReservedNames, ", "))
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
		if _, known := abi.PermissionByName(p); !known {
			add("permissions: %q is not one of this host's permissions; the vocabulary "+
				"is closed and is %s", p, strings.Join(abi.PermissionNames(), ", "))
		}
		if seenPerm[p] {
			add("permissions: %q is declared twice", p)
		}
		seenPerm[p] = true
	}

	seenMigration := make(map[string]bool, len(m.Migrations))
	for _, f := range m.Migrations {
		switch {
		case f.File == "":
			add("migrations: an entry must name the .sql file it describes")
		case strings.ContainsRune(f.File, '/'), strings.ContainsRune(f.File, '\\'),
			f.File == ".", f.File == "..":
			// The same refusal `module` gets, and for the same reason: a manifest
			// naming ../../anything is not a path with a fix.
			add("migrations: %q must be a bare filename inside %s/", f.File, MigrationsDir)
		case !strings.HasSuffix(f.File, ".sql"):
			// `.go` migrations are goose's other form and are not reachable here: the
			// host builds the migration filesystem from this list, so a Go migration
			// could not be compiled into the host anyway. Refused by name so a
			// publisher finds out from the manifest rather than from a migration that
			// silently never ran.
			add("migrations: %q must end in .sql", f.File)
		case !migrationFileRe.MatchString(f.File):
			add("migrations: %q must begin with a version number and an underscore, "+
				"as in 00001_initial.sql — the number is what orders it", f.File)
		case seenMigration[f.File]:
			add("migrations: %q is listed twice", f.File)
		}
		seenMigration[f.File] = true

		switch {
		case len(f.SHA256) != 64:
			add("migrations %q: sha256 %q must be 64 hex characters", f.File, f.SHA256)
		case strings.ToLower(f.SHA256) != f.SHA256:
			add("migrations %q: sha256 %q must be lowercase", f.File, f.SHA256)
		default:
			if _, err := hex.DecodeString(f.SHA256); err != nil {
				add("migrations %q: sha256 %q is not hex: %v", f.File, f.SHA256, err)
			}
		}
	}
	if len(m.Migrations) > 0 && !seenPerm[abi.PermissionStorage] {
		// The host runs this DDL inside the schema `storage.own_schema` is the grant
		// for. An add-on shipping migrations without declaring it has described a
		// capability it did not ask for, and the safe direction to guess in does not
		// exist: creating the schema anyway grants what was not requested, and
		// skipping the migrations leaves a module whose tables silently do not exist.
		add("migrations: %d file(s) are listed and %q is not declared; the host runs "+
			"an add-on's DDL inside the schema that permission grants",
			len(m.Migrations), abi.PermissionStorage)
	}

	seenPrefix := make(map[string]bool, len(m.CookiePrefixes))
	for _, p := range m.CookiePrefixes {
		switch {
		case !cookiePrefixRe.MatchString(p):
			add("cookie_prefixes: %q is not a usable cookie-name prefix; 2 to 64 "+
				"characters, lowercase letters, digits and underscores, starting with "+
				"a letter", p)
		case !strings.HasPrefix(p, m.Name+"_"):
			// Half of the collision rule, and only half: a namespace rather than a
			// registry, so no add-on can be denied its own by one that loaded first.
			// The same shape the Postgres schema (M63) and the route prefix (M64)
			// take, for the same reason — the name is the one thing already unique per
			// instance.
			//
			// What this rule cannot see is the *other* names installed. `oidc` may
			// declare `oidc_x`, which begins with its own name and an underscore and is
			// a prefix of everything add-on `oidc_x` can declare, so the second half is
			// at load: two names standing in a `name + "_"` prefix relation are both
			// refused (nameCollisions, host.go). Neither claim is honoured, and it takes
			// both halves for "two add-ons cannot claim each other's cookies" to be
			// true. It was measured false with only this one.
			add("cookie_prefixes: %q must begin with %q: a cookie namespace is derived "+
				"from the add-on's name, so no add-on can be denied its own and none "+
				"can claim one of this product's", p, m.Name+"_")
		case reachesHostCookie(p):
			add("cookie_prefixes: %q reaches this product's own cookie namespace, which "+
				"no add-on may read or set: the session cookie is the session", p)
		case seenPrefix[p]:
			add("cookie_prefixes: %q is declared twice", p)
		}
		seenPrefix[p] = true
	}

	seenSetting := make(map[string]bool, len(m.Settings))
	for _, s := range m.Settings {
		switch {
		case !nameRe.MatchString(s.Name):
			add("settings: %q is not a usable setting name; same shape as an "+
				"add-on name", s.Name)
		case slices.Contains(config.AddonOverrideNames, s.Name):
			// The reserved names (M65). A setting and an operator's override live in
			// one environment namespace by design, so LINKCTRL_ADDON_OIDC_FAILURE_CLASS
			// would otherwise be two different things at once and no lookup could tell
			// which was meant — the same ambiguity D263 closed for two add-on names,
			// answered the same way: refuse, rather than resolve.
			add("settings: %q is reserved; it is how an operator answers about this "+
				"add-on rather than a value the add-on reads, and the reserved names "+
				"are %s", s.Name, strings.Join(config.AddonOverrideNames, ", "))
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
