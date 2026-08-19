package addon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

// This file reads an add-on's migration source and hands the host something to
// apply.
//
// # Why an in-memory filesystem rather than the directory
//
// The same rule the module gets: bytes that are not the bytes the manifest
// describes are not executed. M60 hashes the .wasm before wazero is asked for
// anything, and the argument is that a check made after the artifact has been
// parsed is worth nothing. DDL deserves it more, not less — the host runs it, an
// operator did not write it, and it runs before the listener opens.
//
// Handing goose an os.DirFS over the directory would leave a window between the
// digest and the read, and — worse than the window — it would leave the *set*
// open: goose globs `*.sql` and `*.go` out of whatever it is given, so a file
// nobody listed would be applied. Building the filesystem from the verified bytes
// closes both. It is why [migrationFS] exists rather than a one-line os.DirFS.

// maxMigrationBytes bounds one migration file.
//
// The whole set is read into host memory before anything is applied, so this is a
// bound on the heap at boot, paid for a directory an operator populated. A quarter
// of a megabyte is far longer than any schema this product's own migrations
// needed — the largest is a few kilobytes — and short enough that a directory of
// junk fails rather than swells.
const maxMigrationBytes = 256 << 10

// readMigrations verifies an add-on's migration source against its manifest and
// returns it as a filesystem goose can read, or nil when the add-on ships none.
//
// Three refusals, and each one is a different lie the directory could be telling:
// a file the manifest lists and the directory does not have, a file the directory
// has and the manifest does not list, and a file whose bytes are not the bytes the
// manifest describes. The second is the one that is easy to leave out and is the
// reason the manifest enumerates rather than aggregates — without it, adding DDL
// to an installed add-on takes no edit to any signed artifact.
func readMigrations(dir string, m Manifest) (fs.FS, error) {
	path := filepath.Join(dir, MigrationsDir)
	entries, err := os.ReadDir(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if len(m.Migrations) > 0 {
			return nil, fmt.Errorf("the manifest lists %d migration(s) and there is no %s/ directory",
				len(m.Migrations), MigrationsDir)
		}
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%s/: %w", MigrationsDir, err)
	}

	// What is on disk, as a set. Directories and anything not ending .sql are
	// ignored rather than refused: an editor's swap file or a README beside the
	// migrations is not a reason for an instance to stop, and it is the same answer
	// Open gives a stray file in the add-ons directory.
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		present[e.Name()] = true
	}

	listed := make(map[string]bool, len(m.Migrations))
	files := make(map[string][]byte, len(m.Migrations))
	for _, want := range m.Migrations {
		listed[want.File] = true
		// filepath.Join with a name Validate has refused a separator in, so this
		// cannot leave the migrations directory.
		full := filepath.Join(path, want.File)
		code, err := readBounded(full)
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", MigrationsDir, want.File, err)
		}
		sum := sha256.Sum256(code)
		if got := hex.EncodeToString(sum[:]); got != want.SHA256 {
			return nil, fmt.Errorf("%s/%s hashes to %s, the manifest says %s",
				MigrationsDir, want.File, got, want.SHA256)
		}
		files[want.File] = code
	}

	var unlisted []string
	for name := range present {
		if !listed[name] {
			unlisted = append(unlisted, name)
		}
	}
	if len(unlisted) > 0 {
		sort.Strings(unlisted)
		// Refused rather than skipped. Skipping would mean the host executes a set
		// the manifest describes and *also* that the directory can hold DDL the
		// manifest does not — which is the whole property the digests are for.
		return nil, fmt.Errorf("%s/ holds %v, which the manifest does not list; "+
			"every migration the host runs is named in the manifest with its digest",
			MigrationsDir, unlisted)
	}
	if len(files) == 0 {
		return nil, nil
	}
	return &migrationFS{files: files}, nil
}

// readBounded reads one file, refusing one larger than the bound rather than
// reading it and then complaining.
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: an operator-owned directory is the feature; the filename is validated to be bare
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// One byte past the bound, so a file exactly at it is accepted and a file over
	// it is detectable without reading all of it.
	code, err := io.ReadAll(io.LimitReader(f, maxMigrationBytes+1))
	if err != nil {
		return nil, err
	}
	if len(code) > maxMigrationBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxMigrationBytes)
	}
	return code, nil
}

// migrationFS is the verified migration set, as the flat filesystem goose reads.
//
// Flat on purpose: goose globs its patterns against the root, so a set with no
// directories in it is a set with nothing to walk into. It implements the two
// optional interfaces fs.Glob and fs.ReadFile look for, plus Open, which is what
// goose's own parser calls.
type migrationFS struct {
	files map[string][]byte
}

var (
	_ fs.FS         = (*migrationFS)(nil)
	_ fs.ReadDirFS  = (*migrationFS)(nil)
	_ fs.ReadFileFS = (*migrationFS)(nil)
)

func (m *migrationFS) names() []string {
	out := make([]string, 0, len(m.files))
	for name := range m.files {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func (m *migrationFS) ReadFile(name string) ([]byte, error) {
	code, ok := m.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	// A copy, because goose is handed this and the caller keeps the original. A
	// shared slice is a set of migrations somebody downstream can rewrite.
	out := make([]byte, len(code))
	copy(out, code)
	return out, nil
}

func (m *migrationFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]fs.DirEntry, 0, len(m.files))
	for _, n := range m.names() {
		out = append(out, migrationEntry{name: n, size: int64(len(m.files[n]))})
	}
	return out, nil
}

func (m *migrationFS) Open(name string) (fs.File, error) {
	code, ok := m.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &migrationFile{
		info: migrationInfo{name: name, size: int64(len(code))},
		data: code,
	}, nil
}

// migrationEntry and migrationInfo are the fs metadata a glob needs and nothing
// more. The modification time is the zero time deliberately: these bytes came
// from a manifest's digest rather than from a file whose mtime means anything, and
// a fabricated timestamp is a fact somebody would eventually read.
type migrationEntry struct {
	name string
	size int64
}

func (e migrationEntry) Name() string               { return e.name }
func (e migrationEntry) IsDir() bool                { return false }
func (e migrationEntry) Type() fs.FileMode          { return 0 }
func (e migrationEntry) Info() (fs.FileInfo, error) { return migrationInfo(e), nil }

type migrationInfo struct {
	name string
	size int64
}

func (i migrationInfo) Name() string       { return i.name }
func (i migrationInfo) Size() int64        { return i.size }
func (i migrationInfo) Mode() fs.FileMode  { return 0o444 }
func (i migrationInfo) ModTime() time.Time { return time.Time{} }
func (i migrationInfo) IsDir() bool        { return false }
func (i migrationInfo) Sys() any           { return nil }

// migrationFile is one open migration. Read-only, and it holds an offset rather
// than a reader so Stat can answer without a second type.
type migrationFile struct {
	info   migrationInfo
	data   []byte
	offset int
}

func (f *migrationFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *migrationFile) Close() error               { return nil }

func (f *migrationFile) Read(p []byte) (int, error) {
	if f.offset >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.offset:])
	f.offset += n
	return n, nil
}
