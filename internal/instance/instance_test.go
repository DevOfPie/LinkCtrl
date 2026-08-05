package instance

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// operatorOnly names the two methods whose authority is the shell rather than a
// permission.
//
// Everything else in this package takes an `*auth.Identity` and asks
// `Can(PermAdmin)` before it does anything. These two do not, and the reason is
// F140: the operation is *appoint whoever administers this instance*, D98 puts
// `instance.admin` outside `InstanceGrantable` so that no in-product surface can
// confer it, and the operator running `lctl` is trusted on exactly the basis
// `POST /api/v1/auth/setup` already trusts them — filesystem and deploy access.
var operatorOnly = []string{"MovePrincipal", "Principals"}

// TestThePrincipalMoveIsReachableFromTheShellAndNowhereElse.
//
// The absence of a permission check is safe **because nothing routes here**, and
// that is a property of the whole tree rather than of this file. A handler that
// called MovePrincipal would turn a command guarded by shell access into an
// endpoint guarded by nothing, which is a strictly worse version of the gap F140
// was filed about — and it is a plausible edit, because the method looks like
// every other service method and reads as though it were missing a check.
//
// So the scan, in the idiom internal/link and cmd/lctl already use for
// prohibitions: the symbol may appear where it is defined, in tests, and under
// cmd/lctl. Anywhere else fails the build, and whoever wants it there has to
// come here and say why.
//
// Blunt on purpose. It matches the name in a comment as readily as in a call,
// which costs a sentence somebody rewords and buys a rule that cannot be
// defeated by an indirection.
func TestThePrincipalMoveIsReachableFromTheShellAndNowhereElse(t *testing.T) {
	root := filepath.Join("..", "..")
	// The two places the symbol belongs. Relative to root, with the OS separator,
	// so the comparison below is against what filepath.Walk actually produces.
	allowed := []string{
		filepath.Join("internal", "instance"),
		filepath.Join("cmd", "lctl"),
	}

	var scanned int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		for _, dir := range allowed {
			if strings.HasPrefix(rel, dir+string(filepath.Separator)) {
				return nil
			}
		}
		b, readErr := os.ReadFile(path) //nolint:gosec // G304: paths come from the walk
		if readErr != nil {
			return readErr
		}
		scanned++
		for i, line := range strings.Split(string(b), "\n") {
			for _, name := range operatorOnly {
				if strings.Contains(line, name) {
					t.Errorf("%s:%d names %s. That method takes no actor and makes no "+
						"permission check, because the authority for it is filesystem "+
						"access to the box — the same claim /auth/setup rests on. "+
						"Reached from anywhere a request can arrive, it is an "+
						"unauthenticated way to take over the instance.",
						filepath.ToSlash(rel), i+1, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if scanned == 0 {
		t.Fatal("scanned no files; the walk is broken rather than the tree clean")
	}
}
