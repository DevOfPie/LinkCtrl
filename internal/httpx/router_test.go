package httpx

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// maximalDeps returns a Deps with every optional dependency present, so that
// one registration pass registers every route this router can ever register.
//
// Filled by reflection rather than written out. A literal would be a third list
// to keep in sync and its failure would be silent: a dependency added to Deps
// and forgotten here leaves its routes unregistered, so they are never checked
// for a mount and never checked against the reserved list, with every guard
// still green.
//
// Nothing is ever called on these values. Registration takes method values and
// stores them, so the zero value of each service is enough; a handler that
// would dereference a nil field is never reached, because no request is served.
func maximalDeps() Deps {
	var d Deps
	fillPointers(reflect.ValueOf(&d).Elem(), map[reflect.Type]bool{})
	// The one registration gated on configuration rather than on a dependency.
	d.Config.DocsEnabled = true
	// The one dependency that is an interface rather than a pointer, so
	// fillPointers cannot make one (M64). Set by hand, and the check below was
	// widened to interfaces in the same change: an interface field left nil takes
	// its routes out of both guards below without either of them failing, which is
	// precisely the silence patternFloor and TestMaximalDepsFillsEveryDependency
	// exist to break.
	d.Web.Addons = nopAddonRouter{}
	// The second interface field, for the same reason and with the same
	// consequence if it is forgotten (M67). Since M68 it also gates the Add-on
	// manager's pages, which is why the same value is set on Web: one interface,
	// two surfaces, and a nil on either takes its half out of both guards below.
	d.AddonAdmin = nopAddonLifecycle{}
	d.Web.AddonAdmin = nopAddonLifecycle{}
	return d
}

// fillPointers allocates every nil pointer field of a struct, recursing into
// the ones whose type this package declares. Deps.Web is why the recursion
// exists: six dashboard pages are gated on fields of *Web rather than of Deps.
//
// It stops at the package boundary, and at a type it has already filled, so it
// cannot walk into a service's object graph — which may be cyclic and which
// nothing here inspects.
func fillPointers(v reflect.Value, seen map[reflect.Type]bool) {
	pkg := reflect.TypeOf(Deps{}).PkgPath()
	for i := range v.NumField() {
		f := v.Field(i)
		if !f.CanSet() || f.Kind() != reflect.Pointer {
			continue
		}
		if f.IsNil() {
			f.Set(reflect.New(f.Type().Elem()))
		}
		el := f.Type().Elem()
		if el.Kind() != reflect.Struct || el.PkgPath() != pkg || seen[el] {
			continue
		}
		seen[el] = true
		fillPointers(f.Elem(), seen)
	}
}

// patternFloor is a floor on how many routes a maximal registration pass
// produces. It exists because both tests below iterate what was registered: a
// pass that registered nothing would report success at speed, and the failure
// mode being guarded against here — a dependency filled by nobody — removes
// routes rather than adding them.
const patternFloor = 120

const (
	aliasPattern     = "/{alias}"
	aliasDeepPattern = "/{alias}/{rest...}"
)

// TestMaximalDepsFillsEveryDependency is what stops the two guards below from
// going quiet. Both iterate what a registration pass produced, so a dependency
// maximalDeps left nil removes routes from the check rather than failing it —
// the check would pass, faster, having stopped looking at them.
//
// It walks Deps and *Web with its own traversal rather than reusing
// fillPointers, so a fillPointers that stopped recursing is caught instead of
// being agreed with.
func TestMaximalDepsFillsEveryDependency(t *testing.T) {
	d := maximalDeps()
	if d.Web == nil {
		t.Fatal("Deps.Web is nil; no dashboard route is registered at all")
	}
	for _, v := range []reflect.Value{reflect.ValueOf(d), reflect.ValueOf(*d.Web)} {
		for i := range v.NumField() {
			f := v.Field(i)
			// Interface as well as pointer since M64: Web.Addons is an interface, and
			// a nil one is exactly as invisible to the two guards below as a nil
			// pointer was.
			//
			// Deps.Authenticator is the one interface field exempt, and it is exempt
			// because it gates no *pattern*: NewRouter reads it to build the session
			// middleware, so registerAppRoutes — which is what both guards below run
			// — registers exactly the same set with it nil. Named rather than
			// skipped silently, so an interface that does gate a route cannot join it
			// by looking similar.
			if v.Type().Field(i).Name == "Authenticator" {
				continue
			}
			if (f.Kind() == reflect.Pointer || f.Kind() == reflect.Interface) && f.IsNil() {
				t.Errorf("%s.%s is nil after maximalDeps: the routes gated on it are never "+
					"registered, so nothing checks that they are mounted or reserved",
					v.Type().Name(), v.Type().Field(i).Name)
			}
		}
	}
}

// TestEveryRegisteredAppRouteIsMountedOnTheRoot walks the patterns the
// application mux was actually handed and asserts the root tree routes each one
// into the application tree.
//
// This is the guard F85 needed and did not have. The reserved-list guard could
// only ever catch "registered but not reserved", because the list it read was
// the list the router mounted from; nothing looked in the other direction, and
// eleven of M42's and M43's routes shipped registered, reserved, linked from
// the nav and documented, answering the redirect handler's "Link not found" on
// every deployment shape.
//
// It is belt to appMux.mounts's braces: mounting is now derived from
// registration, so the gap is structurally closed, and this test is what says
// so if the derivation itself is wrong.
func TestEveryRegisteredAppRouteIsMountedOnTheRoot(t *testing.T) {
	app := newAppMux()
	registerAppRoutes(maximalDeps(), app)
	if len(app.patterns) < patternFloor {
		t.Fatalf("a maximal registration pass produced only %d patterns, want at least %d; "+
			"maximalDeps has stopped filling something and this test is now checking almost nothing",
			len(app.patterns), patternFloor)
	}

	// The root tree as NewRouter builds it. The two fixed prefixes and the
	// catch-all are written out rather than derived, because landing on the
	// catch-all is exactly the failure this test looks for and it has to be
	// nameable.
	sink := http.NotFoundHandler()
	root := http.NewServeMux()
	for _, p := range app.mounts() {
		root.Handle(p, sink)
	}
	root.Handle(APIPrefix+"/", sink)
	root.Handle("/static/", sink)
	root.Handle(aliasPattern, sink)
	root.Handle(aliasDeepPattern, sink)

	for _, pattern := range app.patterns {
		t.Run(pattern, func(t *testing.T) {
			method := http.MethodGet
			if i := strings.IndexByte(pattern, ' '); i >= 0 {
				method = pattern[:i]
			}
			req := httptest.NewRequest(method, concretePath(patternPath(pattern)), nil)
			_, matched := root.Handler(req)
			switch matched {
			case aliasPattern, aliasDeepPattern:
				t.Errorf("%q is registered on the application mux, but the root mux covers it "+
					"with nothing better than %q: the request reaches the redirect handler and "+
					"answers \"Link not found\"", pattern, matched)
			case "":
				t.Errorf("%q is registered on the application mux, but the root mux matches it "+
					"with no pattern at all: a bare 404", pattern)
			}
		})
	}
}

// concretePath turns a pattern's path into a request path a mux can match:
// every wildcard segment becomes an ordinary one, and the root exact-match
// pattern becomes "/".
func concretePath(path string) string {
	if path == "/{$}" {
		return "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "x"
		}
	}
	return strings.Join(segs, "/")
}

// TestMountsAreNoWiderThanTheRoutesTheyServe is the other half. A derivation
// that mounted "/" would pass the test above and hand the entire alias
// namespace to the dashboard, so the mounts are also asserted to be exactly the
// two shapes the rule produces.
func TestMountsAreNoWiderThanTheRoutesTheyServe(t *testing.T) {
	app := newAppMux()
	registerAppRoutes(maximalDeps(), app)

	for _, m := range app.mounts() {
		if m == "/{$}" {
			continue
		}
		seg := strings.Trim(m, "/")
		if seg == "" || strings.ContainsAny(seg, "/{}") {
			t.Errorf("mount %q is not a single top-level segment; the alias catch-all "+
				"lives at the root and a wider mount would swallow it", m)
		}
	}
}
