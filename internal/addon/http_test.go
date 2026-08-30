package addon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
)

// The routes limb, against a real module (M64).
//
// internal/httpx tests what reaches a browser; this file tests what reaches a
// *guest* and what a guest is allowed to answer. The two halves are separate
// because they fail separately: an escaping bug is in the page, and a boundary
// bug is here.

// pagesPrefix is the cookie namespace the fixture's manifest declares. Every
// prefix must begin with the add-on's own name and an underscore, so this is what
// `pages` may own.
const pagesPrefix = "pages_"

// pagesHost installs the routes fixture and opens a host over it.
func pagesHost(t *testing.T, tweak func(*Manifest)) (*Host, *logSink) {
	t.Helper()
	code := fixture(t, "pages")
	dir := t.TempDir()
	m := manifestFor("pages", ClassRequired, code)
	m.Permissions = grantable()
	m.CookiePrefixes = []string{pagesPrefix}
	m.Settings = []Setting{
		{Name: "client_id", Type: SettingText, Default: "declared-default"},
		{Name: "client_secret", Type: SettingSecret},
	}
	if tweak != nil {
		tweak(&m)
	}
	// After the tweak, and conditional on the grant: a manifest declaring an
	// origin-marked setting without `network.fetch` is refused (M68.5), and half
	// the callers here narrow the permission list to prove something about a
	// grant the add-on does not hold.
	if slices.Contains(m.Permissions, abi.PermissionNetworkFetch) {
		m.Settings = append(m.Settings, originSetting())
	}
	install(t, dir, m, code)

	sink := &logSink{}
	h, err := Open(context.Background(), Options{
		Dir:    dir,
		Logger: slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Settings: func(string, []string) map[string]config.Secret {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("the pages fixture did not load: %v\n%s", err, sink.String())
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h, sink
}

// get is one GET to a path inside the add-on's own prefix.
func get(t *testing.T, h *Host, path string) (Response, error) {
	t.Helper()
	return h.Route(context.Background(), "pages",
		RequestIn{Method: http.MethodGet, Path: path})
}

// A request crosses, the module answers, and the record arrives intact. The echo
// is what makes "intact" falsifiable: the module marshals back what it was
// handed, so a field the host dropped is a field missing from the answer.
func TestARequestReachesAModuleAndTheAnswerComesBack(t *testing.T) {
	h, _ := pagesHost(t, nil)

	resp, err := h.Route(context.Background(), "pages", RequestIn{
		Method:         http.MethodPost,
		Path:           "/callback",
		Query:          "code=xyz&state=abc",
		ContentType:    "application/x-www-form-urlencoded",
		AcceptLanguage: "en-GB,en;q=0.9",
		Body:           []byte("a=1"),
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status %d, want 200", resp.Status)
	}
	echoed, ok := strings.CutPrefix(resp.Body, "echo ")
	if !ok {
		t.Fatalf("the module did not echo: %q", resp.Body)
	}
	var got Request
	if err := json.Unmarshal([]byte(echoed), &got); err != nil {
		t.Fatalf("the echoed record is not a request: %v", err)
	}
	want := Request{
		Method: http.MethodPost, Path: "/callback", Query: "code=xyz&state=abc",
		ContentType:    "application/x-www-form-urlencoded",
		AcceptLanguage: "en-GB,en;q=0.9", Body: "a=1",
		Cookies: map[string]string{},
	}
	if got.Method != want.Method || got.Path != want.Path || got.Query != want.Query ||
		got.ContentType != want.ContentType || got.AcceptLanguage != want.AcceptLanguage ||
		got.Body != want.Body || got.BodyBase64 {
		t.Errorf("the record that crossed is\n%+v\nwant\n%+v", got, want)
	}
}

// D232, as behaviour rather than as a property of the ABI's field names: the
// session cookie is *sent*, and it does not cross. The host is what filters,
// because the host is what knows the manifest.
func TestTheSessionCookieDoesNotReachAModule(t *testing.T) {
	h, _ := pagesHost(t, nil)

	resp, err := h.Route(context.Background(), "pages", RequestIn{
		Method: http.MethodGet, Path: "/echo",
		Cookies: []*http.Cookie{
			{Name: auth.SessionCookieNameInsecure, Value: "a-real-looking-token"},
			{Name: auth.SessionCookieName, Value: "the-secure-spelling"},
			{Name: "linkctrl_theme", Value: "dark"},
			{Name: pagesPrefix + "state", Value: "mine"},
			{Name: "other_addon_state", Value: "not mine"},
		},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	for _, forbidden := range []string{
		"a-real-looking-token", "the-secure-spelling",
		auth.SessionCookieNameInsecure, auth.SessionCookieName,
		"linkctrl_theme", "other_addon_state",
	} {
		if strings.Contains(resp.Body, forbidden) {
			t.Errorf("%q reached the module: this product's session cookie is the session, "+
				"so a module handed one could act as whoever is signed in", forbidden)
		}
	}
	if !strings.Contains(resp.Body, pagesPrefix+"state") {
		t.Errorf("the add-on's own cookie did not reach it, so the namespace it declared "+
			"buys nothing: %s", resp.Body)
	}
}

// A module may not choose text/html, may not answer a permanent redirect, may
// not write twice, and may not set a cookie outside its namespace. Each refusal
// is reported by the *guest*, because the status a guest received is a fact only
// the guest can report.
func TestTheHostRefusesTheResponsesItMust(t *testing.T) {
	h, sink := pagesHost(t, nil)

	for path, marker := range map[string]string{
		"/html":           "pages: html_refused=ok",
		"/permanent":      "pages: permanent_refused=ok",
		"/twice":          "pages: second_write_refused=ok",
		"/foreign-cookie": "pages: foreign_cookie_refused=ok",
	} {
		if _, err := get(t, h, path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if logs := sink.String(); !strings.Contains(logs, marker) {
			t.Errorf("the module did not report %q for %s\n%s", marker, path, logs)
		}
	}
	if strings.Contains(sink.String(), "MISMATCH") {
		t.Errorf("the host accepted a response it must refuse\n%s", sink.String())
	}
}

// The refused response is refused *whole*: the first write stands and the second
// changes nothing, so a module writing twice cannot leave the host holding a
// half-applied answer.
func TestOnlyTheFirstResponseStands(t *testing.T) {
	h, _ := pagesHost(t, nil)
	resp, err := get(t, h, "/twice")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Body != "first" {
		t.Errorf("the response is %q; the first write is the one that stands", resp.Body)
	}
}

// The vocabulary of media types, and the wrapped default. This is the host-side
// half of the claim internal/httpx asserts from the browser's side.
func TestTheContentTypeVocabularyIsClosed(t *testing.T) {
	for _, tc := range []struct {
		body    string
		want    string
		refused bool
	}{
		{`{"content_type":"text/plain"}`, "text/plain; charset=utf-8", false},
		{`{"content_type":"text/plain; charset=utf-8"}`, "text/plain; charset=utf-8", false},
		{`{"content_type":"application/json"}`, "application/json", false},
		{`{}`, ContentTypeWrapped, false},
		{`{"content_type":"text/html"}`, "", true},
		{`{"content_type":"text/html; charset=utf-8"}`, "", true},
		{`{"content_type":"image/svg+xml"}`, "", true},
		{`{"content_type":"text/plain; charset=iso-8859-1"}`, "", true},
		{`{"content_type":"application/xhtml+xml"}`, "", true},
	} {
		got, err := decodeResponse([]byte(tc.body), nil)
		switch {
		case tc.refused && err == nil:
			t.Errorf("%s was accepted, and the host owns the HTML", tc.body)
		case !tc.refused && err != nil:
			t.Errorf("%s was refused: %v", tc.body, err)
		case !tc.refused && got.ContentType != tc.want:
			t.Errorf("%s normalized to %q, want %q", tc.body, got.ContentType, tc.want)
		}
	}
}

// The status vocabulary, including the two 3xx spellings that would make this
// product answer a permanent redirect for the first time.
func TestTheStatusVocabularyIsClosed(t *testing.T) {
	for _, tc := range []struct {
		body    string
		want    int
		refused bool
	}{
		{`{}`, http.StatusOK, false},
		{`{"status":404}`, http.StatusNotFound, false},
		{`{"status":500}`, http.StatusInternalServerError, false},
		{`{"location":"/somewhere"}`, http.StatusFound, false},
		{`{"status":302,"location":"/somewhere"}`, http.StatusFound, false},
		{`{"status":301,"location":"/somewhere"}`, 0, true},
		{`{"status":308,"location":"/somewhere"}`, 0, true},
		{`{"status":307,"location":"/somewhere"}`, 0, true},
		{`{"status":302}`, 0, true},
		{`{"status":100}`, 0, true},
		{`{"status":600}`, 0, true},
		{`{"location":"/x","body":"and a body"}`, 0, true},
		// A location that reads as a path and behaves as another origin.
		{`{"location":"//evil.test/x"}`, 0, true},
		{`{"location":"javascript:alert(1)"}`, 0, true},
		{"{\"location\":\"/x\\r\\nSet-Cookie: a=b\"}", 0, true},
		// An add-on's flow legitimately leaves this origin, which is what an
		// identity provider is.
		{`{"location":"https://idp.test/authorize"}`, http.StatusFound, false},
	} {
		got, err := decodeResponse([]byte(tc.body), nil)
		switch {
		case tc.refused && err == nil:
			t.Errorf("%s was accepted", tc.body)
		case !tc.refused && err != nil:
			t.Errorf("%s was refused: %v", tc.body, err)
		case !tc.refused && got.Status != tc.want:
			t.Errorf("%s answered %d, want %d", tc.body, got.Status, tc.want)
		}
	}
}

// A field this host does not know is a module expecting behaviour that will not
// happen — the same refusal the manifest parser gives, for the same reason.
func TestAnUnknownResponseFieldIsRefused(t *testing.T) {
	if _, err := decodeResponse([]byte(`{"status":200,"stream":true}`), nil); err == nil {
		t.Error("a response carrying an unknown field was accepted")
	}
	if _, err := decodeResponse([]byte(`{"status":200}{"status":200}`), nil); err == nil {
		t.Error("a second response object in one payload was accepted")
	}
}

// The three failures the routing path distinguishes, each from a real module.
func TestWhatAModuleFailingLooksLike(t *testing.T) {
	h, _ := pagesHost(t, nil)

	if _, err := get(t, h, "/nothing"); err == nil || !isErr(err, ErrNoResponse) {
		t.Errorf("a handler that wrote nothing answered %v, want ErrNoResponse", err)
	}
	if _, err := get(t, h, "/refuse"); err == nil || !isErr(err, ErrGuestFailed) {
		t.Errorf("a handler that returned a refusal answered %v, want ErrGuestFailed", err)
	}
	if _, err := h.Route(context.Background(), "nosuchaddon",
		RequestIn{Method: http.MethodGet, Path: "/"}); !isErr(err, ErrNoRoute) {
		t.Errorf("an add-on nobody installed answered %v, want ErrNoRoute", err)
	}
}

// An add-on that did not declare the routes grant has no prefix, which is one
// answer with a mistyped path: what an instance runs is not something an
// anonymous visitor is owed.
func TestAnAddonWithoutTheGrantHasNoPrefix(t *testing.T) {
	h, _ := pagesHost(t, func(m *Manifest) {
		m.Permissions = []string{"config.read"}
	})
	if _, err := get(t, h, "/echo"); !isErr(err, ErrNoRoute) {
		t.Errorf("an add-on that declared no routes grant answered %v, want ErrNoRoute", err)
	}
	if names := h.RoutedAddons(); len(names) != 0 {
		t.Errorf("RoutedAddons is %v for an add-on that declared no routes grant", names)
	}
}

// A module exporting no handler is a packaging failure, not an instance failure:
// the add-on loads, and its pages answer.
func TestAModuleWithNoHandlerLoadsAndItsPagesFail(t *testing.T) {
	code := fixture(t, "minimal")
	dir := t.TempDir()
	m := manifestFor("minimal", ClassRequired, code)
	m.Permissions = []string{PermissionRoutes}
	install(t, dir, m, code)

	h, sink, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatalf("the add-on did not load: %v", err)
	}
	if _, err := h.Route(context.Background(), "minimal",
		RequestIn{Method: http.MethodGet, Path: "/"}); !isErr(err, ErrNoHandler) {
		t.Errorf("a module with no handler answered %v, want ErrNoHandler", err)
	}
	if !strings.Contains(sink.String(), abi.GuestHTTPHandler) {
		t.Errorf("the log does not name the export an operator has to go and add\n%s", sink.String())
	}
}

// The session read costs its own grant (D258), and an add-on that did not declare
// it is handed nothing — not merely refused the call.
func TestSessionContextCostsItsOwnGrant(t *testing.T) {
	// The identity the host resolved, not a record built by the test: since M65 the
	// record a module sees is derived from this inside internal/addon, so a test
	// that handed one over would be testing its own mapping.
	signedIn := &auth.Identity{
		UserID: uuid.MustParse("0198c9c5-0000-7000-8000-000000000001"),
		Email:  "owner@example.com", Role: "owner",
	}

	full, _ := pagesHost(t, nil)
	resp, err := full.Route(context.Background(), "pages",
		RequestIn{Method: http.MethodGet, Path: "/session", Identity: signedIn})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Body, signedIn.Email) {
		t.Errorf("an add-on holding session.context could not read who is signed in: %q", resp.Body)
	}

	// The same request, the same module, one token fewer.
	narrow, _ := pagesHost(t, func(m *Manifest) {
		m.Permissions = []string{PermissionRoutes, "config.read"}
	})
	resp, err = narrow.Route(context.Background(), "pages",
		RequestIn{Method: http.MethodGet, Path: "/session", Identity: signedIn})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Body, signedIn.Email) || strings.Contains(resp.Body, signedIn.UserID.String()) {
		t.Errorf("an add-on that declared only routes.own_prefix read the identity anyway: %q",
			resp.Body)
	}
	if !strings.Contains(resp.Body, "session unavailable") {
		t.Errorf("the refusal did not reach the module as one: %q", resp.Body)
	}
}

// An operator's configured value outranks the manifest's default, and a secret
// that has one stops being ErrNotFound. This is the config limb of M64 through
// the whole path: the environment, the load, and the guest's own call.
func TestConfiguredSettingsReachAModuleAndOutrankItsDefaults(t *testing.T) {
	code := fixture(t, "pages")
	dir := t.TempDir()
	m := manifestFor("pages", ClassRequired, code)
	m.Permissions = grantable()
	m.Settings = []Setting{
		{Name: "client_id", Type: SettingText, Default: "declared-default"},
		{Name: "client_secret", Type: SettingSecret},
	}
	install(t, dir, m, code)

	t.Setenv(config.AddonSettingVar("pages", "client_id"), "configured-id")
	t.Setenv(config.AddonSettingVar("pages", "client_secret"), "configured-secret")
	// A variable for a setting no manifest declares. It is not read, which is the
	// same scoping config_get itself applies.
	t.Setenv(config.AddonSettingVar("pages", "undeclared"), "ignored")

	h, sink, err := openHostWithLog(t, dir)
	if err != nil {
		t.Fatalf("the add-on did not load: %v", err)
	}
	loaded := h.Addons()[0]
	if got := loaded.ConfiguredSettings(); got != 2 {
		t.Errorf("%d settings were configured, want 2 — the undeclared variable is not one", got)
	}
	// The values are held as config.Secret, so no line the host writes can print
	// one. The boot log is the line that would.
	for _, value := range []string{"configured-id", "configured-secret"} {
		if strings.Contains(sink.String(), value) {
			t.Errorf("%q appears in the log: an add-on's configured values are held as "+
				"config.Secret precisely so that they cannot", value)
		}
	}
	if v, ok := loaded.settings.get("client_id"); !ok || v.Reveal() != "configured-id" {
		t.Errorf("the configured value did not reach the add-on: %d values configured",
			loaded.settings.len())
	}
	if got := config.AddonSettingVar("pages", "client_id"); got != "LINKCTRL_ADDON_PAGES_CLIENT_ID" {
		t.Errorf("the variable an operator sets is %q", got)
	}
}

// Per-request instantiation, asserted from the guest's side: a module cannot
// carry state from one request into the next, so an authentication add-on's
// nonce has to live in its own schema rather than in its memory (D260).
func TestOneInstancePerRequest(t *testing.T) {
	h, sink := pagesHost(t, nil)
	for range 3 {
		if _, err := get(t, h, "/echo"); err != nil {
			t.Fatal(err)
		}
	}
	// The fixture logs from package initialization, which runs once per
	// instantiation. Three requests, three initializations.
	if got := strings.Count(sink.String(), "pages: instance initialized"); got != 4 {
		t.Errorf("the fixture initialized %d times across a load and three requests, want 4; "+
			"an instance is per request, and the load's own instance is the fourth", got)
	}
}

// The cost of that choice, measured rather than inherited (m64.md's second risk).
//
// The ceiling is two orders of magnitude above what this machine does and is
// there to catch a change in the *shape* of the cost — a route that started
// compiling, or one that grew a database round trip — not jitter. The number this
// run measured is logged, which is what D260 cites.
func TestRoutingCostsAnInstantiation(t *testing.T) {
	h, _ := pagesHost(t, nil)

	// One request first, so what is being timed is not a cold instruction cache.
	if _, err := get(t, h, "/echo"); err != nil {
		t.Fatal(err)
	}
	const runs = 10
	start := time.Now()
	for range runs {
		if _, err := get(t, h, "/echo"); err != nil {
			t.Fatal(err)
		}
	}
	per := time.Since(start) / runs

	t.Logf("one add-on request costs %v, instantiation included", per)
	// The dashboard's target is 250 ms for a whole page. A route that cannot
	// answer inside a tenth of it is not a route this product can put a page
	// behind, and it is what would have to be a pool instead.
	if per > 100*time.Millisecond {
		t.Errorf("one add-on request cost %v; the dashboard's target is 250ms for the "+
			"whole page, so this is the choice between an instance per request and a pool",
			per)
	}
}

// The concurrency bound holds under load, and a request still gets an answer.
// What this asserts is that the queue is a queue: sixteen slots, more callers
// than slots, and every one of them served.
func TestConcurrentRequestsAreBoundedAndAllAnswered(t *testing.T) {
	h, _ := pagesHost(t, nil)

	const callers = maxConcurrentRoutes * 2
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = get(t, h, "/echo")
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	// The slots are all back, or the next request would block for ever.
	if len(h.slots) != 0 {
		t.Errorf("%d of %d slots are still held after every caller returned",
			len(h.slots), maxConcurrentRoutes)
	}
}

// A cancelled request does not wait for a slot, which is what makes the bound a
// queue rather than a stall.
func TestABusyHostRefusesRatherThanWaitsForEver(t *testing.T) {
	h, _ := pagesHost(t, nil)
	// Every slot held, so the next caller has to wait — and its context is
	// already done.
	for range maxConcurrentRoutes {
		h.slots <- struct{}{}
	}
	t.Cleanup(func() {
		for range maxConcurrentRoutes {
			<-h.slots
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Route(ctx, "pages",
		RequestIn{Method: http.MethodGet, Path: "/echo"}); !isErr(err, ErrBusy) {
		t.Errorf("a saturated host answered %v, want ErrBusy", err)
	}
}

// The prefix is derived from the name, like the schema and the cookie namespace,
// so two add-ons cannot contend for one and none can be denied its own. Unlike the
// cookie namespace it needed nothing else: the whole name is one path segment,
// matched exactly, with nothing joined onto it — see http.go's prefix section.
func TestThePrefixIsDerivedFromTheName(t *testing.T) {
	h, _ := pagesHost(t, nil)
	l := h.Addons()[0]
	if got := l.PathPrefix(); got != "/addons/pages/" {
		t.Errorf("PathPrefix is %q", got)
	}
	if !strings.HasPrefix(l.PathPrefix(), RoutePrefix) {
		t.Errorf("%q is not under %q", l.PathPrefix(), RoutePrefix)
	}
	if names := h.RoutedAddons(); len(names) != 1 || names[0] != "pages" {
		t.Errorf("RoutedAddons is %v", names)
	}
}

// The two permission constants this package branches on, held to the vocabulary
// the ABI publishes. A second spelling is the drift a closed vocabulary exists
// to stop, and both of these are used *as* the token rather than looked up.
func TestTheRoutePermissionConstantsNameVocabularyEntries(t *testing.T) {
	for _, name := range []string{PermissionRoutes, PermissionSessionContext} {
		p, ok := abi.PermissionByName(name)
		if !ok {
			t.Errorf("%q is not in the vocabulary: %v", name, abi.PermissionNames())
			continue
		}
		if !p.Grantable {
			t.Errorf("%q is not grantable, and M64 implemented what it costs", name)
		}
		if p.BackedBy != "M64" {
			t.Errorf("%q is backed by %q, want M64", name, p.BackedBy)
		}
	}
	if PermissionRoutes != "routes.own_prefix" || PermissionSessionContext != "session.context" {
		t.Errorf("the constants are %q and %q", PermissionRoutes, PermissionSessionContext)
	}
}

// A body that is not UTF-8 crosses as base64, and says so. Without the flag a
// guest could not tell an encoded body from one that happens to look encoded.
func TestANonUTF8BodyCrossesAsBase64AndSaysSo(t *testing.T) {
	text, encoded := EncodeRequestBody([]byte("plain text"))
	if encoded || text != "plain text" {
		t.Errorf("UTF-8 text was encoded: %q %v", text, encoded)
	}
	binary, encoded := EncodeRequestBody([]byte{0xff, 0xfe, 0x00})
	if !encoded {
		t.Error("a body that is not UTF-8 crossed as text")
	}
	if binary != "//4A" {
		t.Errorf("the encoded body is %q", binary)
	}
}

// isErr is errors.Is over the wrapped forms Route returns.
func isErr(err error, target error) bool { return errors.Is(err, target) }

// A request whose record does not fit one ABI value is refused before anything is
// instantiated, and refused as the client's error.
//
// The bound is on the *record*, which is what makes the three cases below
// different sizes: a plain body of the bound itself is over it once the envelope
// is added, a body that is not UTF-8 is base64 first so its ceiling is three
// quarters of that, and a body of control characters is six bytes each inside the
// JSON encoding. What must not happen is any of them reaching the module, because
// a module handed a record it cannot answer about produces a 502 for a body
// somebody else chose the size of.
func TestARequestTooLargeToCrossIsRefusedBeforeTheModuleSeesIt(t *testing.T) {
	h, sink := pagesHost(t, nil)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"a plain body the size of the bound", bytes.Repeat([]byte("a"), maxStringIn)},
		{"base64 pushes a binary body over", bytes.Repeat([]byte{0xff}, maxStringIn*3/4)},
		{"json escaping pushes control characters over", bytes.Repeat([]byte{0x01}, maxStringIn/4)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h.Route(context.Background(), "pages", RequestIn{
				Method: http.MethodPost, Path: "/hostile", Body: tc.body,
			})
			if !isErr(err, ErrRequestTooLarge) {
				t.Fatalf("a body of %d bytes answered %v with status %d; the record does not "+
					"fit one value and the visitor has to be told so",
					len(tc.body), err, resp.Status)
			}
			// The module is what a 502 would blame, so the module must not have run.
			if strings.Contains(sink.String(), "pages: handling POST /hostile") {
				t.Error("the module was instantiated for a request it could never be handed")
			}
		})
	}

	// And the bound is a ceiling rather than a wall: a record that fits crosses.
	resp, err := h.Route(context.Background(), "pages", RequestIn{
		Method: http.MethodPost, Path: "/hostile",
		Body: bytes.Repeat([]byte("a"), maxStringIn/2),
	})
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("a body of %d bytes answered %v with status %d, so the bound above is "+
			"refusing everything", maxStringIn/2, err, resp.Status)
	}
}

// --- F289: what an add-on occupies in a browser -----------------------------

// jarStore is a browser's cookie store, as far as an add-on's cookies are
// concerned: names to values, with the host's own session cookie already in it.
//
// It exists so that "how many cookies does an add-on occupy" is a number a test
// can read, rather than a property inferred from a response. Applying a jar
// follows the rules a browser follows — a negative Max-Age deletes, anything
// else stores — and nothing here knows what a jar means, which is the point: the
// count has to hold against an opaque value.
type jarStore map[string]string

func newJarStore() jarStore {
	return jarStore{"linkctrl_session": "the operator's session"}
}

func (s jarStore) apply(jar []JarCookie) {
	for _, c := range jar {
		if c.MaxAge < 0 {
			delete(s, c.Name)
			continue
		}
		s[c.Name] = c.Value
	}
}

func (s jarStore) sent() []*http.Cookie {
	out := make([]*http.Cookie, 0, len(s))
	for name, value := range s {
		out = append(out, &http.Cookie{Name: name, Value: value})
	}
	return out
}

// bytes is the whole weight of the store, because F289's other limb was header
// size rather than count: 1200 Set-Cookie headers, 107,000 bytes, in one answer.
func (s jarStore) bytes() int {
	total := 0
	for name, value := range s {
		total += len(name) + len(value)
	}
	return total
}

// visit is one request with whatever the browser is holding, and the browser
// applying whatever came back.
func visit(t *testing.T, h *Host, store jarStore, path, query string) Response {
	t.Helper()
	resp, err := h.Route(context.Background(), "pages", RequestIn{
		Method: http.MethodGet, Path: path, Query: query, Cookies: store.sent(),
	})
	if err != nil {
		t.Fatalf("GET %s?%s: %v", path, query, err)
	}
	store.apply(resp.Jar)
	return resp
}

// The fix, stated as the thing it fixes. Two hundred visits, each setting a
// cookie under a name the module chose and never used before, with a real
// browser store carrying the answers forward — which is the shape that reached
// 180 cookies and evicted `linkctrl_session` in Chromium at n=180.
//
// What is asserted is occupancy rather than refusal: the module is never told
// no, every name it sets is still readable, and the number of slots it holds in
// the store does not move.
func TestVisitsDoNotGrowWhatAnAddonOccupies(t *testing.T) {
	h, _ := pagesHost(t, nil)
	store := newJarStore()

	for i := range 200 {
		visit(t, h, store, "/cookie-named", "n"+strconv.Itoa(i))
		if got := len(store); got > 3 {
			t.Fatalf("after %d visits the browser holds %d cookies (%v); an add-on's "+
				"occupancy is at most one jar per lifetime class and does not grow with "+
				"visits, which is what stopped it evicting the session cookie",
				i+1, got, slices.Sorted(maps.Keys(store)))
		}
	}
	if _, ok := store["linkctrl_session"]; !ok {
		t.Fatal("the session cookie is gone from the store, which is F289 exactly")
	}
	if got := store.bytes(); got > 2*maxCookieJar {
		t.Errorf("the add-on's cookies weigh %d bytes; two jars bound it at %d",
			got, 2*maxCookieJar)
	}
	// And the module can still read what it wrote, most recently first: the jar is
	// storage rather than a muzzle. The oldest names are gone — that is the
	// eviction maxCookieJar forces, and it is the add-on losing its own values
	// rather than the browser losing somebody else's.
	resp := visit(t, h, store, "/echo", "")
	if !strings.Contains(resp.Body, `"pages_n199":"v"`) {
		t.Errorf("the module cannot read back the cookie it just set: %s", resp.Body)
	}
}

// A module that asks for the flood is told no at the call it made, rather than
// having its answer quietly changed — the rule the whole response record
// follows.
//
// The fixture sets two hundred cookies and not the twelve hundred M64.9 drove
// through the host's own handler, and the difference is what makes this test
// measure the new bound rather than an old one: twelve hundred cookies is a
// record larger than the ABI's 64 KiB single value, which `readString` refused
// before M64 was reopened and would refuse whatever this milestone did. Two
// hundred fits the record and does not fit a jar. The host's own reason is read
// out of the log, because the guest is told `ErrInvalid` and nothing more — by
// design, so that a module looping on a bad record cannot decide how much an
// instance logs.
func TestACookieFloodIsRefusedAtTheCallTheModuleMade(t *testing.T) {
	h, sink := pagesHost(t, nil)
	resp := visit(t, h, newJarStore(), "/cookie-flood", "")
	if !strings.Contains(sink.String(), "pages: cookie_flood_refused=ok") {
		t.Errorf("the module was not refused 200 cookies: %s", sink.String())
	}
	if !strings.Contains(sink.String(), "the cookie jar is") {
		t.Errorf("the refusal was not the jar's bound, so this test is measuring "+
			"some other refusal: %s", sink.String())
	}
	if len(resp.Jar) != 0 {
		t.Errorf("a refused response still wrote %d cookies", len(resp.Jar))
	}
	if !strings.Contains(resp.Body, "flood refused") {
		t.Errorf("body %q", resp.Body)
	}
}

// The same refusal, isolated from every other bound on the boundary: a record
// small enough to cross, carrying cookies too large to pack.
//
// It is a separate test because the fixture cannot show which bound refused it —
// the guest gets one status for every refusal — and a test that cannot tell two
// bounds apart is a test that would pass if the new one were deleted.
func TestCookiesThatFitTheRecordAndNotTheJarAreRefused(t *testing.T) {
	set := make([]Cookie, 200)
	for i := range set {
		set[i] = Cookie{Name: pagesPrefix + "n" + strconv.Itoa(i), Value: "v", MaxAge: 600}
	}
	raw, err := json.Marshal(Response{Body: "x", SetCookie: set})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxStringIn {
		t.Fatalf("the record is %d bytes, over the ABI's %d, so this test is "+
			"measuring the record bound and not the jar's", len(raw), maxStringIn)
	}
	_, err = decodeResponse(raw, []string{pagesPrefix})
	if err == nil {
		t.Fatal("200 cookies packed into a jar a browser would store")
	}
	if !strings.Contains(err.Error(), "cookie jar") {
		t.Errorf("refused for some other reason: %v", err)
	}

	// And the bound is on the jar rather than on the count: the same number of
	// cookies, small enough to pack, is accepted.
	for i := range set {
		set[i] = Cookie{Name: pagesPrefix + strconv.Itoa(i), MaxAge: 600}
	}
	raw, _ = json.Marshal(Response{Body: "x", SetCookie: set[:20]})
	if _, err := decodeResponse(raw, []string{pagesPrefix}); err != nil {
		t.Errorf("twenty small cookies were refused: %v", err)
	}
}

// The two lifetime classes, which is why there are two jars and not one. A
// session cookie packed beside a year-long one would outlive the browser being
// closed, and a module asking for a session cookie asked for the opposite.
func TestALifetimeSurvivesBeingPackedIntoAJar(t *testing.T) {
	h, _ := pagesHost(t, nil)
	store := newJarStore()
	resp := visit(t, h, store, "/cookie-two", "")

	byName := map[string]JarCookie{}
	for _, c := range resp.Jar {
		byName[c.Name] = c
	}
	if len(byName) != 2 {
		t.Fatalf("two cookies of two lifetimes packed into %d jars: %+v", len(resp.Jar), resp.Jar)
	}
	if got := byName[jarName("pages", false)]; got.MaxAge != 0 {
		t.Errorf("the session jar carries Max-Age=%d, so a session cookie would "+
			"survive the browser closing", got.MaxAge)
	}
	if got := byName[jarName("pages", true)]; got.MaxAge != 600 {
		t.Errorf("the kept jar carries Max-Age=%d and the longest thing in it lives "+
			"600 seconds", got.MaxAge)
	}
	// Both readable, whichever jar they landed in.
	echo := visit(t, h, store, "/echo", "")
	for _, want := range []string{`"pages_session":"s1"`, `"pages_kept":"k1"`} {
		if !strings.Contains(echo.Body, want) {
			t.Errorf("the module cannot read back %s: %s", want, echo.Body)
		}
	}
}

// A deletion in the ABI's vocabulary — a negative max_age — reaches the browser
// as a deletion of the jar, because the jar is then empty. A cookie a browser
// keeps in order to hand back nothing is a slot spent on nothing.
func TestADeletionEmptiesTheJarRatherThanRewritingIt(t *testing.T) {
	h, _ := pagesHost(t, nil)
	store := newJarStore()
	visit(t, h, store, "/cookie", "")
	if _, ok := store[jarName("pages", true)]; !ok {
		t.Fatalf("the jar was not written: %v", slices.Sorted(maps.Keys(store)))
	}
	visit(t, h, store, "/cookie-clear", "")
	if _, ok := store[jarName("pages", true)]; ok {
		t.Errorf("the jar survived the deletion of the only thing in it: %v",
			slices.Sorted(maps.Keys(store)))
	}
	echo := visit(t, h, store, "/echo", "")
	if strings.Contains(echo.Body, "pages_state") {
		t.Errorf("a deleted cookie is still readable: %s", echo.Body)
	}
}

// An entry outliving its own max_age inside a jar held open by a longer-lived
// one is the drift two jars alone would not have stopped. The host drops it on
// the way in, so the module reads what it asked for rather than what the browser
// happened to keep.
func TestAnEntryDiesOnItsOwnScheduleInsideALongerLivedJar(t *testing.T) {
	now := time.Now()
	value, err := packJar([]jarEntry{
		{Name: "pages_short", Value: "gone", Exp: now.Add(-time.Second).Unix()},
		{Name: "pages_long", Value: "here", Exp: now.Add(time.Hour).Unix()},
	})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := pagesHost(t, nil)
	resp, err := h.Route(context.Background(), "pages", RequestIn{
		Method: http.MethodGet, Path: "/echo",
		Cookies: []*http.Cookie{{Name: jarName("pages", true), Value: value}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Body, "pages_short") {
		t.Errorf("an expired entry was handed to the module: %s", resp.Body)
	}
	if !strings.Contains(resp.Body, `"pages_long":"here"`) {
		t.Errorf("a live entry beside it was dropped too: %s", resp.Body)
	}
}

// What an add-on can still do to itself, and the only threshold in the fix: fill
// its own jar. The oldest values go, the operator is told which add-on did it,
// and nothing outside the add-on's own namespace is touched.
func TestAFullJarDropsItsOldestValuesAndSaysWhichAddon(t *testing.T) {
	h, sink := pagesHost(t, nil)
	store := newJarStore()
	for i := range 80 {
		visit(t, h, store, "/cookie-named", strings.Repeat("x", 40)+strconv.Itoa(i))
	}
	if !strings.Contains(sink.String(), "cookie jar is full") ||
		!strings.Contains(sink.String(), "addon=pages") {
		t.Errorf("nothing in the log says which add-on overfilled its jar: %s", sink.String())
	}
	if _, ok := store["linkctrl_session"]; !ok {
		t.Error("the session cookie went with the add-on's own values")
	}
}

// A lifetime the arithmetic underneath could not represent, refused at the call
// the module made rather than turned into something else.
//
// applyToJar turns max_age into an absolute expiry with
// `now.Add(time.Duration(max_age) * time.Second)`, and a Duration is int64
// nanoseconds. Measured before the bound existed: `max_age=10000000000` gave an
// expiry 8446744074 seconds *before* now, and `max_age=1<<62` gave one exactly
// equal to now. The module got 200 either way, keepLive dropped the entry on the
// very next read, and a cookie the module had been told it set did not exist. It
// was also a regression — http.SetCookie used to write the value and let the
// browser clamp it, so the cookie worked.
//
// This is decodeResponse and not the fixture on purpose, for the reason
// TestCookiesThatFitTheRecordAndNotTheJarAreRefused gives: a guest is told
// ErrInvalid for every refusal on this record and cannot say which bound fired,
// so a test driven through the module could not tell this bound from any other
// and would still pass with it deleted.
func TestALifetimeTooLongToRepresentIsRefusedRatherThanWrapped(t *testing.T) {
	for _, maxAge := range []int{maxCookieAge + 1, 10_000_000_000, 1 << 62} {
		raw, err := json.Marshal(Response{Body: "x", SetCookie: []Cookie{
			{Name: pagesPrefix + "long", Value: "v", MaxAge: maxAge},
		}})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := decodeResponse(raw, []string{pagesPrefix})
		if err == nil {
			_, kept := applyToJar(nil, nil, resp.SetCookie[0], time.Now())
			t.Errorf("max_age=%d was accepted and became the expiry %v, which is what "+
				"the ABI's \"ErrInvalid rather than a silently corrected response\" "+
				"forbids", maxAge, kept)
			continue
		}
		if !strings.Contains(err.Error(), "max_age") {
			t.Errorf("max_age=%d was refused for some other reason, so this test is "+
				"measuring some other bound: %v", maxAge, err)
		}
	}

	// The other half, and the one that says the bound is a bound rather than a
	// wall: everything under it keeps exactly the lifetime it asked for, up to and
	// including the longest a browser would honour.
	now := time.Now()
	for _, maxAge := range []int{1, 3600, maxCookieAge} {
		raw, err := json.Marshal(Response{Body: "x", SetCookie: []Cookie{
			{Name: pagesPrefix + "long", Value: "v", MaxAge: maxAge},
		}})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := decodeResponse(raw, []string{pagesPrefix})
		if err != nil {
			t.Fatalf("max_age=%d was refused: %v", maxAge, err)
		}
		_, kept := applyToJar(nil, nil, resp.SetCookie[0], now)
		if len(kept) != 1 {
			t.Fatalf("max_age=%d packed into %d kept entries", maxAge, len(kept))
		}
		if got := kept[0].Exp - now.Unix(); got != int64(maxAge) {
			t.Errorf("max_age=%d became an expiry %d seconds away", maxAge, got)
		}
	}
}

// The other end of the same arithmetic, and it is not the module's end: an
// expiry that arrives from a jar the *visitor* edited.
//
// checkCookie cannot reach this one — no module wrote it — so the answer is the
// host holding its own attribute to something a browser will take, rather than a
// refusal. A jar is not refused for being odd, because a jar under a visitor's
// hand is the ordinary case unpackJar was written silent for.
func TestAForgedExpiryDoesNotBecomeTheJarsLifetime(t *testing.T) {
	now := time.Now()
	forged, err := packJar([]jarEntry{{Name: pagesPrefix + "x", Value: "v", Exp: 1 << 62}})
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := jarCookies("pages",
		[]*http.Cookie{{Name: jarName("pages", true), Value: forged}},
		[]string{pagesPrefix},
		[]Cookie{{Name: pagesPrefix + "y", Value: "v", MaxAge: 60}}, now)

	var kept JarCookie
	for _, c := range jar {
		if c.Name == jarName("pages", true) {
			kept = c
		}
	}
	if kept.Name == "" {
		t.Fatalf("no kept jar was written: %+v", jar)
	}
	if kept.MaxAge <= 0 || kept.MaxAge > maxCookieAge {
		t.Errorf("the host wrote Max-Age=%d for its own cookie from an expiry the "+
			"visitor chose; the bound is %d", kept.MaxAge, maxCookieAge)
	}
}

// The jar's own lifetime is written from what is *in* it, and eviction happens
// first.
//
// packWithEviction drops the oldest entries until the jar fits, and jarMaxAge
// then makes the cookie live as long as the longest-lived thing it holds. Given
// the list from before the eviction it wrote a lifetime for a value the browser
// was not being handed — reachable without anything hostile: set one value at
// the 400-day bound at the start of a flow, then write short-lived ones on top
// until the jar fills, and the 400-day entry is the oldest and therefore the
// first to go while its lifetime stayed on the cookie.
//
// Nothing was disclosed by that and nothing crossed an add-on's boundary. What
// was wrong was jarMaxAge's own sentence — "as long as the longest-lived thing
// in it, and no longer" — which the line calling it made false.
func TestAnEvictedLifetimeDoesNotOutliveTheJar(t *testing.T) {
	now := time.Now()
	// Oldest first, which is the order eviction takes them in.
	entries := []jarEntry{{
		Name: pagesPrefix + "forever", Value: "v",
		Exp: now.Add(time.Duration(maxCookieAge) * time.Second).Unix(),
	}}
	for i := range 9 {
		entries = append(entries, jarEntry{
			Name:  pagesPrefix + "fill" + strconv.Itoa(i),
			Value: strings.Repeat("x", 180),
			Exp:   now.Add(time.Hour).Unix(),
		})
	}
	sent, err := packJar(entries)
	if err != nil {
		t.Fatalf("the fixture jar does not fit, so the eviction under test never "+
			"runs and this test asserts nothing: %v", err)
	}

	jar, dropped := jarCookies("pages",
		[]*http.Cookie{{Name: jarName("pages", true), Value: sent}},
		[]string{pagesPrefix},
		[]Cookie{{Name: pagesPrefix + "last", Value: strings.Repeat("y", 600),
			MaxAge: 3600}}, now)
	if dropped == 0 {
		t.Fatal("nothing was evicted, so this is measuring a case it was not written for")
	}

	var kept JarCookie
	for _, c := range jar {
		if c.Name == jarName("pages", true) {
			kept = c
		}
	}
	if kept.Value == "" {
		t.Fatalf("no kept jar was written: %+v", jar)
	}
	for _, e := range unpackJar(kept.Value) {
		if e.Name == pagesPrefix+"forever" {
			t.Fatalf("the 400-day entry survived, so the jar's lifetime is not being " +
				"read off an evicted one and this test is measuring nothing")
		}
	}
	if kept.MaxAge > 3600 {
		t.Errorf("the jar was written Max-Age=%d while the longest-lived entry it "+
			"carries has %d seconds to run; the browser keeps it in order to hand "+
			"back nothing", kept.MaxAge, 3600)
	}
}

// One name, two cookies, and the host's own is the one that counts.
//
// The jar is written at `/addons/<name>/`, and a visitor can set a cookie of the
// same name at `/` — which the writer, scoped as it is, then has no way to
// delete. Reading the jars by assigning per match made that last-wins, so the
// planted value shadowed the real one on every later visit and the add-on's
// state was void for good. RFC 6265 §5.4 has a user agent send the more
// specifically scoped cookie first, so first-wins is what makes the host's own
// jar the one that survives.
//
// What the planted jar still adds is names the real one does not hold, and the
// assertion below says so rather than pretending otherwise: a value under a
// declared prefix already reaches the module straight off the cookie header, by
// design, and a visitor's own browser was always theirs to write. Nothing here
// reaches another add-on or another visitor.
func TestAPlantedJarDoesNotShadowTheHostsOwn(t *testing.T) {
	now := time.Now()
	hosts, err := packJar([]jarEntry{
		{Name: pagesPrefix + "state", Value: "real", Exp: now.Add(time.Hour).Unix()},
	})
	if err != nil {
		t.Fatal(err)
	}
	planted, err := packJar([]jarEntry{
		{Name: pagesPrefix + "state", Value: "planted", Exp: now.Add(time.Hour).Unix()},
		{Name: pagesPrefix + "extra", Value: "planted", Exp: now.Add(time.Hour).Unix()},
	})
	if err != nil {
		t.Fatal(err)
	}
	name := jarName("pages", true)
	_, kept := jarsFrom("pages", []*http.Cookie{
		{Name: name, Value: hosts},   // Path=/addons/pages/, so it is sent first
		{Name: name, Value: planted}, // Path=/, planted by the visitor
	}, []string{pagesPrefix}, now)

	byName := map[string]string{}
	for _, e := range kept {
		byName[e.Name] = e.Value
	}
	if byName[pagesPrefix+"state"] != "real" {
		t.Errorf("a jar planted at a broader path shadowed the host's own: %v", byName)
	}
	if byName[pagesPrefix+"extra"] != "planted" {
		t.Errorf("the jars were not merged, so this test is asserting the old "+
			"assign-per-match and not the merge: %v", byName)
	}
}

// --- F290: what an add-on holds in memory -----------------------------------

// The bound, measured from both sides. A module allocating what a page handler
// plausibly needs still works; one allocating more than the host allows is
// stopped by the runtime, per request, with the host still serving.
//
// Before this bound existed the same fixture at 512 MiB took the host from 78 MB
// resident to 1604 MB (F290), because wazero's default is 65536 pages — 4 GiB
// per instance — and nothing else was counting.
func TestGuestMemoryIsBoundedPerInstance(t *testing.T) {
	h, _ := pagesHost(t, nil)

	if resp, err := get(t, h, "/grow"); err != nil || resp.Body == "" {
		t.Fatalf("a module allocating nothing failed: %v", err)
	}
	resp, err := h.Route(context.Background(), "pages",
		RequestIn{Method: http.MethodGet, Path: "/grow", Query: "4"})
	if err != nil {
		t.Fatalf("a module allocating 4 MiB was refused, which is less room than a "+
			"page handler needs: %v", err)
	}
	if resp.Body != "grew 4194304 bytes" {
		t.Errorf("body %q", resp.Body)
	}

	resp, err = h.Route(context.Background(), "pages",
		RequestIn{Method: http.MethodGet, Path: "/grow", Query: "64"})
	if !errors.Is(err, ErrGuestFailed) {
		t.Fatalf("a module allocating 64 MiB was allowed to: err=%v body=%q; "+
			"maxGuestMemoryPages is %d pages, which is %d MiB",
			err, resp.Body, maxGuestMemoryPages, maxGuestMemoryPages/16)
	}
	// And the host is still a host: the failure was the request's, not the
	// process's.
	if _, err := get(t, h, "/cookie"); err != nil {
		t.Errorf("the host did not survive a module hitting the bound: %v", err)
	}
}

// The arithmetic the documentation states, held against the documentation —
// each constant on its own, not only their product, and nowhere the numbers are
// stated that this table has not been told about.
//
// It is here because the sentence F290 falsified was not wrong about a
// measurement: 2.4 MB per instance was accurate, and it was wrong because
// nothing tied the sentence to a bound.
//
// **The first version of this test made the same mistake one level up.** It
// multiplied the two constants and looked for "128 MiB" in three files, and
// m64.md claimed from that that neither constant could move without the
// operator-facing sentence moving with it. False for a compensating pair —
// thirty-two instances of 4 MiB is still 128 MiB, and the product would not have
// moved — while four documents state the concurrency and the per-instance bound
// *separately* and would each have gone quietly wrong. So each part is asserted
// where it is stated, in the sentence that states it.
//
// **The second version made a smaller version of it again**, and that is why
// there are two tests here rather than one. It listed eight sentences in six
// files and said of itself that its claim was every file outside build-notes
// that states one of these numbers. It was not: Plan.md stated all three,
// docs/operations.md stated the concurrency, and four of the six files it did
// list stated one of the numbers a *second* time in a sentence no anchor
// covered. A claim about every file, kept true by whoever last thought to read
// every file, is the same shape as a sentence kept true by whoever last thought
// to check it — which is the shape this milestone was reopened to fix. So the
// claim is checked: [TestEveryDocumentedNumberIsTied] finds every occurrence of
// these numbers in the documentation and fails on one this table does not
// account for, either as a sentence tied to the constant or as an occurrence
// that is about something else and says what.
//
// Anchoring the sentence rather than the number is the point and not an
// accident: what an operator reads is a sentence, and rewording one of these
// while the constants stand still is exactly the edit that must not happen
// without somebody looking here. A reworded sentence fails this test, and fixing
// it is one line — which is the cost of the tie, stated rather than hidden.
func TestTheGuestMemoryCeilingIsTheOneDocumented(t *testing.T) {
	fill, _ := documentedNumbers(t)
	for _, doc := range documentedNumberSites {
		flat := flattenDocument(t, doc.path)
		for _, s := range doc.sentences {
			want := strings.ToLower(fill.Replace(s))
			if !strings.Contains(flat, want) {
				t.Errorf("%s does not say %q, so an operator is sizing a host against a "+
					"sentence this build no longer holds to: %d in flight x %d pages",
					doc.path, want, maxConcurrentRoutes, maxGuestMemoryPages)
			}
		}
		// The untied list rots the same way an anchor does, and it rots silently:
		// a phrase nothing matches stops excusing anything and starts hiding that
		// nobody has re-read the sentence it was written for.
		for _, s := range doc.untied {
			if !strings.Contains(flat, strings.ToLower(s)) {
				t.Errorf("%s no longer says %q, which this file lists as an occurrence of "+
					"one of these numbers that is about something else; re-read the "+
					"sentence and either tie it or restate why it is untied", doc.path, s)
			}
		}
	}
}

// Every occurrence of one of the three numbers, in every document that could
// state one, against the table above.
//
// This is the half that makes *every file* a checked claim rather than a claim
// about how carefully somebody swept. A new sentence quoting one of these
// numbers fails here until whoever wrote it decides which it is: a statement of
// the bound, which gets an anchor, or a use of the same word about something
// else, which gets a line in `untied` saying what it is about. Both are one line
// and both are visible.
//
// What it scans is the product's own documentation — every Markdown file in the
// tree, plus sdk/doc.go, which is a publisher's manual that happens to be a Go
// comment. Four exclusions, and they are here rather than left to be discovered,
// because an unwritten exception is what this test exists over:
//
//   - **docs/build-notes** is the record. Its entries quote what a number was
//     when the entry was written, and an append-only file whose past has to be
//     rewritten when a constant moves is not append-only.
//   - **Go source under internal/** states these numbers beside the constants
//     that produce them, so the edit that moves one has the sentence about it
//     already open on the screen. That is not the failure this test is for,
//     which is a sentence in a different file from the constant it describes.
//   - **.claude/ and build output** are not the product's documentation. The
//     first is this harness's own command files; the rest is gitignored and is
//     not in a clone.
//   - **A number spelled in digits.** What it looks for is how these documents
//     write these numbers: the concurrency as a word, the memory in MiB. A
//     sentence saying "16 add-on requests" would pass this sweep, and the
//     spelling table in [documentedNumbers] is what keeps that from being a
//     silent option — a constant with no word fails before this test runs.
func TestEveryDocumentedNumberIsTied(t *testing.T) {
	fill, find := documentedNumbers(t)
	accounted := map[string][]string{}
	for _, doc := range documentedNumberSites {
		for _, s := range doc.sentences {
			accounted[doc.path] = append(accounted[doc.path], strings.ToLower(fill.Replace(s)))
		}
		for _, s := range doc.untied {
			accounted[doc.path] = append(accounted[doc.path], strings.ToLower(s))
		}
	}
	total := 0
	for _, path := range documentationFiles(t) {
		flat := flattenDocument(t, path)
		for _, at := range find.FindAllStringIndex(flat, -1) {
			total++
			if spannedBy(flat, at, accounted[path]) {
				continue
			}
			t.Errorf("%s states %q and no line in documentedNumberSites covers it:\n  …%s…\n"+
				"Either anchor that sentence to the constant, or add it to this file's "+
				"untied list saying what the number is about there.",
				path, flat[at[0]:at[1]], excerptAround(flat, at))
		}
	}
	// A walk that reached nothing would pass every assertion above by reaching
	// none of them, which is the one failure this test cannot report as one.
	if total < len(accounted) {
		t.Fatalf("the sweep found %d occurrences across %d documents, fewer than the "+
			"%d files this file anchors sentences in; the walk from %q is not reaching "+
			"the documentation", total, len(accounted), len(accounted), repoRoot(t))
	}
}

// documentedNumberSites is every sentence outside docs/build-notes that states
// the concurrency bound, the per-instance memory bound, or their product — and,
// per file, every other occurrence of one of those numbers, with what it is
// about instead.
//
// Seven of the nine files give an operator a number to size a host by; two —
// docs/addon-abi.md and sdk/doc.go — give a publisher the per-instance bound and
// neither of the others. Counted at M69.9 rather than trusted: it read six and
// eight against a table that had grown.
var documentedNumberSites = []struct {
	path      string
	sentences []string
	untied    []string
}{
	{path: "docs/SECURITY.md",
		sentences: []string{
			"the number of add-on invocations in flight is bounded at {n} across the instance",
			// M66.5: the ceiling gained a second term when the redirect path started
			// keeping instances, so the sentence names what is held at rest as well as
			// what is in flight.
			"each of those {n} instances is bounded at {mem} of guest memory, " +
				"and {idle} more may be kept warm between invocations, " +
				"a ceiling of {ceiling}",
			// M66: the same budget, now stated as shared by three consumers rather
			// than by add-on page requests alone.
			"The {n} are shared by every reason an add-on runs",
			"a module wanting more than its {mem} is stopped by the runtime",
			"the instance is held to {mem} whatever the module's toolchain wrote",
			// M66.5, in the redirect row: what the pool adds to the bound the pages row
			// states, said where the reuse boundary is argued.
			"the ceiling above is what the two add to the {n} in flight",
		},
		untied: []string{
			// A domain-verification challenge, and a count of Unicode format
			// characters. Neither has anything to do with add-on memory.
			"sixteen bytes from crypto/rand",
			"the sixteen it over-counted",
			// History, and it stays true when the constant moves: what this
			// describes is the tree before F290, where the concurrency was sixteen
			// and the memory bound did not exist.
			"sixteen unbounded instances priced an amount the module chose",
		}},
	{path: "docs/deployment.md",
		sentences: []string{
			"{n} add-on invocations run at once and each is capped at {mem} of guest " +
				"memory, with {idle} more instances kept warm between invocations, " +
				"so {ceiling} is the ceiling",
			"{n} across every reason an add-on runs",
			// M66.5: the host-side image, which is not inside the ceiling because the
			// ceiling states guest memory, so the sizing row quotes the ceiling twice.
			"the resident worst case is {ceiling} twice over",
		}},
	{path: "docs/configuration.md",
		sentences: []string{
			"{n} add-on invocations are served at once",
			// M66's reopening: what instantiating one of them costs when all of
			// them are busy, which is the measurement the second redirect bound is
			// argued from.
			"measured here with all {n} instance slots busy",
			"each instance is bounded at {mem} of memory",
			"{n} in flight plus {idle} kept warm, each of {mem}, is a ceiling of {ceiling}",
			// M66.5's two pool variables, in the table an operator reads them from. The
			// first row says what the pool is *not*, which is the concurrency bound, and
			// the second prices what it holds against the bound in flight.
			"not how many run at once — that is {n}, it is fixed in the build",
			"up to this many instances of {mem}, on top of the {n} that may be in flight",
		}},
	{path: "docs/operations.md",
		sentences: []string{
			// The 503 row of the table an add-on's own failures are read from.
			"{n} add-on invocations are already in flight across the instance",
			"the {n} are shared with the redirect path",
			// The rate-limited row, where the redirect path's own skip is read.
			"all {n} instance slots were busy",
			// M69.9 added the other direction to the same 503 row: a page holding
			// its slot for the route deadline is what starves the redirect path,
			// and it was stated only in slo.md until then.
			"so {n} concurrent sign-ins through an add-on hold all {n} for seconds",
		}},
	{path: "CHANGELOG.md",
		sentences: []string{
			"{n} add-on invocations run at once across the instance",
			"with all {n} instance slots held throughout",
			"each instance is capped at {mem} of memory, and {idle} instances are kept " +
				"warm between invocations, so add-ons add at most {ceiling}",
			"skipped because all {n} instance slots were busy",
			// M66.5, in the paragraph that adds the two pool variables: the sentence
			// says what the pool is *not*, which is this bound.
			"neither variable changes how many add-on invocations run at once, " +
				"which is still {n} and still fixed in the build",
			// M68.5's route deadline, where the slot budget is what the bound gives
			// back: a route handler that will not return holds one of these until
			// the deadline elapses, and before the deadline existed, until the
			// request timeout did.
			"a module that will not return holds one of the {n} instance slots for " +
				"as long as the visitor waits",
		},
		untied: []string{
			// How many dashboard pages scrolled sideways at 360px, in 0.4.0.
			"at 360px, sixteen of the",
			// How many ABI functions do anything, which reached sixteen at M68.5 and
			// collides with this file's concurrency bound only by arithmetic. Tied by
			// internal/addon/abi's own sweep, which is where the number comes from.
			"sixteen functions work; the rest are declared and refuse",
		}},
	{path: "Plan.md",
		sentences: []string{
			"{n} add-on invocations run at once across the instance, each bounded at " +
				"{mem} of guest memory",
			"{idle} kept warm is what LINKCTRL_ADDON_POOL_SIZE bounds",
			"the three bounds add into the {ceiling} ceiling",
			"an out-of-band observation draw on the same {n}",
		},
		untied: []string{
			// An anchor in planning.md, about how many milestones a phase holds.
			"the-size-target-a-phase-stays-under-sixteen-milestones",
			// How many functions the ABI has, which is a different count entirely and
			// is tied by internal/addon/abi's own sweep. It collides here only because
			// this file's concurrency bound happens to be spelled with the same word,
			// and it arrived at M66 when the ABI reached sixteen functions.
			"one of its seventeen functions still refuses",
			// The live count in the same row, which reached sixteen at M68.5 and
			// collides for the same reason. Tied by internal/addon/abi.
			"so sixteen are live",
		}},
	{path: "docs/SECURITY.md",
		untied: []string{
			// The ABI's live count, which reached sixteen at M68.5. Tied by
			// internal/addon/abi's anchored sweep, not by this file's bound.
			"sixteen of those functions do anything today",
		}},
	{path: "docs/addon-abi.md",
		sentences: []string{
			"a module gets {mem} of linear memory — {pages} pages",
			"{mem} is what your instance gets either way",
		}},
	{path: "sdk/doc.go",
		sentences: []string{
			"a bounded amount of memory — {mem} of linear memory",
			"the instance gets {mem} either way",
		}},
	{path: "docs/slo.md",
		sentences: []string{
			// M66's add-on run, where the concurrency bound is the thing being
			// measured: it is what makes 83% of that run indistinguishable from a run
			// with no add-on, so it is tied rather than excused.
			"{n} instance slots exist across the whole host and an inline invocation " +
				"takes one without waiting",
			"while {n} are held by modules being killed",
			// The reopening's own reading of the same run: the instantiation bound
			// was never approached with every slot held, which is what makes it a
			// measurement of instantiation under load rather than of an idle machine.
			"with all {n} instance slots continuously held by modules being killed",
			// M66.5's run, where the slot budget is the arithmetic rather than the
			// subject: the same sixteen carry twenty-four times the invocations once
			// startup is out of the occupancy.
			"{n} instance slots at 11.05 ms of occupancy carry ~1,448 invocations a second",
			"the same {n} slots carry ~35,000 a second",
			// M67's re-run, reading the same budget to say why a skip count of
			// thirty-eight is jitter rather than the budget starting to bind.
			"of a run whose {n} instance slots have headroom for ~35,000 " +
				"invocations a second",
			"{n} in flight plus {idle} kept warm, each held to {mem}",
			// M68.5's written discharge, where the slot budget is the one line from
			// that milestone to this document's subject. Three sentences rather than
			// one, because the rejection of that milestone's first attempt was partly
			// that it stated only the half that helps: the deadline gives a slot back
			// from a spinning module, and a *fetching* route handler holds one for a
			// network round trip where a computing one held it for milliseconds.
			"those {n} slots are shared with the redirect path",
			"{n} concurrent sign-in round trips on an add-on's pages are {n} slots " +
				"held for seconds",
			"{n} is fixed in the build",
		},
		untied: []string{
			// Cache-hit runs in the performance record.
			"what all sixteen cached runs in this document have done",
			"sixteen cached runs now read 100%",
		}},
}

// documentedNumbers renders the three numbers the way the documents spell them,
// and a pattern that finds any of them in flattened prose.
func documentedNumbers(t *testing.T) (*strings.Replacer, *regexp.Regexp) {
	t.Helper()
	// The documents spell the concurrency as a word, so the tie needs the word.
	// A constant with no spelling here fails loudly rather than quietly asserting
	// nothing: whoever changes it writes the word, which is the same act as
	// going to change the sentences above.
	inWords := map[int]string{
		4: "four", 8: "eight", 12: "twelve", 16: "sixteen",
		24: "twenty-four", 32: "thirty-two", 48: "forty-eight", 64: "sixty-four",
	}
	n, ok := inWords[maxConcurrentRoutes]
	if !ok {
		t.Fatalf("maxConcurrentRoutes is %d, and every document below spells this "+
			"number as a word; add the spelling here, then to each of them",
			maxConcurrentRoutes)
	}
	// The pool's default, spelled the same way, and it is a *fourth* number since
	// M66.5: an instance is now kept after the invocation that made it, so the
	// ceiling is what may be in flight plus what may be held at rest.
	idle, ok := inWords[DefaultPoolSize]
	if !ok {
		t.Fatalf("DefaultPoolSize is %d, and the documents spell it as a word; add the "+
			"spelling here, then to each of them", DefaultPoolSize)
	}
	// 16 pages of 64 KiB to the MiB, which is the only arithmetic in this file
	// that is not the documentation's own.
	mem := strconv.Itoa(maxGuestMemoryPages/16) + " MiB"
	ceiling := strconv.Itoa((maxConcurrentRoutes+DefaultPoolSize)*maxGuestMemoryPages/16) + " MiB"
	pages := strconv.Itoa(maxGuestMemoryPages)
	fill := strings.NewReplacer("{n}", n, "{mem}", mem, "{ceiling}", ceiling,
		"{pages}", pages, "{idle}", idle)
	// Word boundaries, so that the ceiling is not read as the per-instance bound
	// inside its own digits: "128 MiB" holds "8 MiB" as characters and not as a
	// number, and \b is the difference.
	find := regexp.MustCompile(`\b(` + strings.Join([]string{
		regexp.QuoteMeta(strings.ToLower(n)),
		regexp.QuoteMeta(strings.ToLower(mem)),
		regexp.QuoteMeta(strings.ToLower(ceiling)),
		regexp.QuoteMeta(strings.ToLower(pages + " pages")),
	}, "|") + `)\b`)
	// **The pool default is tied by [documentedNumberSites] and is deliberately not
	// in this pattern.** The sweep works because the two bounds are spelled in ways
	// prose does not otherwise use — "sixteen" about anything else is rare enough to
	// list, and "8 MiB" is a quantity. "eight" is an ordinary English word: adding it
	// would flag some thirty occurrences across these files and README, cli.md and
	// usage.md, every one of them about something else, and a sweep whose output is
	// mostly exclusions is a sweep nobody reads. So a *new* sentence stating the pool
	// default is not caught here, while every sentence this file already anchors
	// still fails the moment the constant moves — which is the half the ceiling
	// depends on.
	return fill, find
}

// documentationFiles is every file the sweep reads, relative to the repo root.
func documentationFiles(t *testing.T) []string {
	t.Helper()
	// Named rather than derived: the sweep is a claim about what it reads, so what
	// it does not read is written down. See [TestEveryDocumentedNumberIsTied].
	skipDir := map[string]bool{
		".git": true, ".claude": true, "node_modules": true, "bin": true,
		"dist": true, "tmp": true, "build-notes": true,
	}
	root := repoRoot(t)
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, ".md") || rel == "sdk/doc.go" {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for documentation: %v", root, err)
	}
	return out
}

func flattenDocument(t *testing.T, path string) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return flattenProse(string(text))
}

// spannedBy reports whether any of these phrases contains the whole of at.
func spannedBy(flat string, at []int, phrases []string) bool {
	for _, p := range phrases {
		for from := 0; from <= len(flat)-len(p); {
			i := strings.Index(flat[from:], p)
			if i < 0 {
				break
			}
			i += from
			if i <= at[0] && at[1] <= i+len(p) {
				return true
			}
			from = i + 1
		}
	}
	return false
}

// excerptAround gives the reader of a failure enough of the sentence to find it.
func excerptAround(flat string, at []int) string {
	return flat[max(0, at[0]-90):min(len(flat), at[1]+90)]
}

// flattenProse makes a Markdown paragraph comparable to a sentence written in
// one line: case, emphasis, code ticks and where the author happened to wrap are
// none of them the claim. The numbers and the words between them are.
func flattenProse(text string) string {
	// "// " goes too, so a Go doc comment flattens the same way a paragraph of
	// Markdown does and a sentence wrapped across two comment lines is one
	// sentence here.
	replaced := strings.NewReplacer("*", "", "`", "", "// ", "").
		Replace(strings.ToLower(text))
	return strings.Join(strings.Fields(replaced), " ")
}
