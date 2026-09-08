package sdk_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The SDK's whole claim is that an add-on in another repository can compile
// against it, and m61.md asks for that proven mechanically rather than asserted.
// Two properties, and neither is provable by reading the source:
//
//   - the package's dependency closure is the standard library and nothing else,
//     so importing it does not drag LinkCtrl's internals — or its 14 direct
//     dependencies — into somebody else's module;
//   - a module outside this repository builds against it, with the module proxy
//     turned off, so nothing about the build depends on this repository being
//     published anywhere.
//
// That property is what lets DevOfPie/LinkCtrl-OIDC compile against the SDK from
// its first commit. The add-on repository actually doing so is M69's evidence and
// cannot be marked done from this tree, which is why what is asserted here is the
// property and not the consumer.

func TestTheSDKDependsOnNothingButTheStandardLibrary(t *testing.T) {
	// For the platform an add-on is actually built for. The native build is a stub
	// and could pass this while the real one failed.
	out, err := runGo(t, ".", "list", "-deps", ".")
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(out) {
		if pkg == "github.com/DevOfPie/LinkCtrl/sdk" {
			continue
		}
		// A standard-library import path's first element has no dot in it, which is
		// the same rule the go command itself uses to tell one from a module path.
		if strings.Contains(strings.SplitN(pkg, "/", 2)[0], ".") {
			t.Errorf("the SDK depends on %s; an add-on's module would inherit it", pkg)
		}
		if strings.Contains(pkg, "LinkCtrl/internal") {
			t.Errorf("the SDK depends on %s, which no consumer outside this repository can import", pkg)
		}
	}
}

func TestAModuleOutsideThisRepositoryBuildsAgainstTheSDKAlone(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	write(t, filepath.Join(dir, "go.mod"), fmt.Sprintf(`module consumer

go %s

require github.com/DevOfPie/LinkCtrl v0.0.0

replace github.com/DevOfPie/LinkCtrl => %s
`, goDirective(t, root), root))

	// A consumer that uses a live function, a refused one, an error value and the
	// version constant — the four things an add-on's first commit touches.
	write(t, filepath.Join(dir, "main.go"), `//go:build wasip1

package main

import (
	"errors"

	"github.com/DevOfPie/LinkCtrl/sdk"
)

func init() {
	_ = sdk.Log(sdk.LevelInfo, "built against ABI "+sdk.ABIVersion)
	if _, err := sdk.RedirectEventRead(); !errors.Is(err, sdk.ErrNotAvailable) {
		panic("a refused function answered something else")
	}
}

func main() {}
`)

	out, err := runGo(t, dir, "build", "-buildmode=c-shared", "-o", filepath.Join(dir, "consumer.wasm"), ".")
	if err != nil {
		t.Fatalf("a module outside this repository would not build against the SDK: %v\n%s", err, out)
	}
	if info, err := os.Stat(filepath.Join(dir, "consumer.wasm")); err != nil || info.Size() == 0 {
		t.Fatalf("the consumer produced no module: %v", err)
	}
}

// goDirective is this repository's own language version, so the consumer module
// declares the same one. Read rather than derived from the toolchain: a newer
// toolchain than the go.mod directive is the ordinary case, and a consumer
// declaring the toolchain's version would be testing a language version this
// repository does not claim to build under.
func goDirective(t *testing.T, root string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("this repository's go.mod has no go directive")
	return ""
}

// runGo runs the go command for wasip1 with the proxy off, so a build that would
// have to fetch anything fails instead of quietly succeeding on a machine with a
// network.
func runGo(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GOOS=wasip1", "GOARCH=wasm",
		"GOPROXY=off",
		"GOFLAGS=-mod=mod",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
