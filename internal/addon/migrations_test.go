package addon

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
)

// installMigrations writes an add-on's migration files and returns the manifest
// entries that honestly describe them.
func installMigrations(t *testing.T, dir string, files map[string]string) []MigrationFile {
	t.Helper()
	path := filepath.Join(dir, MigrationsDir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	var out []MigrationFile
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(path, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		out = append(out, MigrationFile{File: name, SHA256: hex.EncodeToString(sum[:])})
	}
	slices.SortFunc(out, func(a, b MigrationFile) int { return strings.Compare(a.File, b.File) })
	return out
}

// --- what the manifest will accept ------------------------------------------

// The manifest is what makes the DDL the add-on author's rather than whatever is
// on disk (D247), and every one of these is a way that claim could have been
// weakened by a manifest this host accepted.
func TestMigrationDeclarationRules(t *testing.T) {
	good := MigrationFile{File: "00001_initial.sql", SHA256: strings.Repeat("cd", 32)}

	tests := []struct {
		name string
		edit func(*Manifest)
		want string
	}{
		{"a migration with no filename", func(m *Manifest) {
			m.Migrations = []MigrationFile{{SHA256: good.SHA256}}
		}, "must name the .sql file"},
		{"a migration naming a path out of the directory", func(m *Manifest) {
			m.Migrations = []MigrationFile{{File: "../../etc/passwd.sql", SHA256: good.SHA256}}
		}, "must be a bare filename"},
		{"a migration that is not .sql", func(m *Manifest) {
			m.Migrations = []MigrationFile{{File: "00001_initial.go", SHA256: good.SHA256}}
		}, "must end in .sql"},
		{"a migration with no version number", func(m *Manifest) {
			m.Migrations = []MigrationFile{{File: "initial.sql", SHA256: good.SHA256}}
		}, "must begin with a version number"},
		{"the same migration twice", func(m *Manifest) {
			m.Migrations = []MigrationFile{good, good}
		}, "is listed twice"},
		{"a digest that is not 64 characters", func(m *Manifest) {
			m.Migrations = []MigrationFile{{File: good.File, SHA256: "abcd"}}
		}, "must be 64 hex characters"},
		{"a digest in uppercase", func(m *Manifest) {
			m.Migrations = []MigrationFile{{File: good.File, SHA256: strings.Repeat("CD", 32)}}
		}, "must be lowercase"},
		{"a digest that is not hex", func(m *Manifest) {
			m.Migrations = []MigrationFile{{File: good.File, SHA256: strings.Repeat("zz", 32)}}
		}, "is not hex"},
		{"migrations without the permission whose schema they run in", func(m *Manifest) {
			m.Migrations = []MigrationFile{good}
			m.Permissions = []string{"redirect.observe"}
		}, abi.PermissionStorage},
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

func TestAManifestWithHonestMigrationsValidates(t *testing.T) {
	m := valid()
	m.Migrations = []MigrationFile{
		{File: "00001_initial.sql", SHA256: strings.Repeat("cd", 32)},
		{File: "00002_index.sql", SHA256: strings.Repeat("ef", 32)},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("a manifest declaring two migrations and storage.own_schema did not validate: %v", err)
	}
}

// --- what reaches the database ----------------------------------------------

func TestVerifiedMigrationsBecomeTheFilesystemGooseReads(t *testing.T) {
	dir := t.TempDir()
	m := valid()
	m.Migrations = installMigrations(t, dir, map[string]string{
		"00001_initial.sql": "-- +goose Up\nCREATE TABLE notes (id int);\n",
		"00002_index.sql":   "-- +goose Up\nCREATE INDEX ON notes (id);\n",
	})

	fsys, err := readMigrations(dir, m)
	if err != nil {
		t.Fatalf("honest migrations were refused: %v", err)
	}
	if fsys == nil {
		t.Fatal("two declared migrations produced no filesystem")
	}
	// The pattern goose itself globs. A filesystem that answered nothing here would
	// mean a migration set the host silently never applied.
	found, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if want := []string{"00001_initial.sql", "00002_index.sql"}; !slices.Equal(found, want) {
		t.Errorf("glob found %v, want %v", found, want)
	}
	body, err := fs.ReadFile(fsys, "00001_initial.sql")
	if err != nil || !strings.Contains(string(body), "CREATE TABLE notes") {
		t.Errorf("read back %q, err %v", body, err)
	}
	// goose's parser calls Open rather than ReadFile, so a filesystem that only
	// implemented one of the two would fail at migration time and not here.
	f, err := fsys.Open("00002_index.sql")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	info, err := f.Stat()
	if err != nil || info.Name() != "00002_index.sql" {
		t.Errorf("stat gave %v, err %v", info, err)
	}
	// Only what the manifest listed. The set being closed is what stops DDL being
	// added to an installed add-on without editing the artifact that describes it.
	if _, err := fsys.Open("00003_surprise.sql"); err == nil {
		t.Error("a file the manifest never listed opened")
	}
}

func TestNoMigrationsAtAllIsNotAFailure(t *testing.T) {
	dir := t.TempDir()
	m := valid()
	m.Migrations = nil

	fsys, err := readMigrations(dir, m)
	if err != nil {
		t.Fatalf("an add-on that ships no DDL was refused: %v", err)
	}
	if fsys != nil {
		t.Error("an add-on that ships no DDL produced a migration filesystem")
	}
}

// The three lies a directory can tell, each one refused by name. The middle one is
// the one that is easy to leave out, and the whole reason the manifest enumerates
// its migrations rather than summarising them with one digest.
func TestMigrationSourceIsRefusedWhenItDisagreesWithTheManifest(t *testing.T) {
	t.Run("a listed migration that is not there", func(t *testing.T) {
		dir := t.TempDir()
		m := valid()
		m.Migrations = installMigrations(t, dir, map[string]string{
			"00001_initial.sql": "-- +goose Up\nSELECT 1;\n",
		})
		if err := os.Remove(filepath.Join(dir, MigrationsDir, "00001_initial.sql")); err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, dir, m, "00001_initial.sql")
	})

	t.Run("a migration on disk the manifest does not list", func(t *testing.T) {
		dir := t.TempDir()
		m := valid()
		m.Migrations = installMigrations(t, dir, map[string]string{
			"00001_initial.sql": "-- +goose Up\nSELECT 1;\n",
		})
		if err := os.WriteFile(filepath.Join(dir, MigrationsDir, "00002_added.sql"),
			[]byte("-- +goose Up\nCREATE TABLE surprise (x int);\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, dir, m, "00002_added.sql")
	})

	t.Run("a migration whose bytes are not the bytes the manifest describes", func(t *testing.T) {
		dir := t.TempDir()
		m := valid()
		m.Migrations = installMigrations(t, dir, map[string]string{
			"00001_initial.sql": "-- +goose Up\nSELECT 1;\n",
		})
		if err := os.WriteFile(filepath.Join(dir, MigrationsDir, "00001_initial.sql"),
			[]byte("-- +goose Up\nSELECT 2;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, dir, m, "hashes to")
	})

	t.Run("a manifest listing migrations with no directory at all", func(t *testing.T) {
		dir := t.TempDir()
		m := valid()
		m.Migrations = []MigrationFile{{File: "00001_initial.sql", SHA256: strings.Repeat("cd", 32)}}
		mustRefuse(t, dir, m, "there is no "+MigrationsDir+"/ directory")
	})

	t.Run("a migration larger than the bound", func(t *testing.T) {
		dir := t.TempDir()
		m := valid()
		m.Migrations = installMigrations(t, dir, map[string]string{
			"00001_initial.sql": "-- +goose Up\n" + strings.Repeat("-", maxMigrationBytes),
		})
		mustRefuse(t, dir, m, "larger than")
	})
}

func mustRefuse(t *testing.T, dir string, m Manifest, want string) {
	t.Helper()
	_, err := readMigrations(dir, m)
	if err == nil {
		t.Fatal("the migration source was accepted")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not mention %q:\n%v", want, err)
	}
}

// A file that is not a migration is ignored rather than refused, which is the
// answer Open already gives a stray file in the add-ons directory. Refusing would
// mean an editor's leftovers stop an instance.
func TestANonSQLFileBesideTheMigrationsIsIgnored(t *testing.T) {
	dir := t.TempDir()
	m := valid()
	m.Migrations = installMigrations(t, dir, map[string]string{
		"00001_initial.sql": "-- +goose Up\nSELECT 1;\n",
	})
	if err := os.WriteFile(filepath.Join(dir, MigrationsDir, "README.md"),
		[]byte("what these do\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readMigrations(dir, m); err != nil {
		t.Fatalf("a README beside the migrations was refused: %v", err)
	}
}
