package addon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
		RequestIn{Method: http.MethodGet, Path: path}, SessionContext{})
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
	}, SessionContext{})
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
	}, SessionContext{})
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
		RequestIn{Method: http.MethodGet, Path: "/"}, SessionContext{}); !isErr(err, ErrNoRoute) {
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
		RequestIn{Method: http.MethodGet, Path: "/"}, SessionContext{}); !isErr(err, ErrNoHandler) {
		t.Errorf("a module with no handler answered %v, want ErrNoHandler", err)
	}
	if !strings.Contains(sink.String(), abi.GuestHTTPHandler) {
		t.Errorf("the log does not name the export an operator has to go and add\n%s", sink.String())
	}
}

// The session read costs its own grant (D258), and an add-on that did not declare
// it is handed nothing — not merely refused the call.
func TestSessionContextCostsItsOwnGrant(t *testing.T) {
	signedIn := SessionContext{
		SignedIn: true, UserID: "0198c9c5-0000-7000-8000-000000000001",
		Email: "owner@example.com", Role: "owner",
	}

	full, _ := pagesHost(t, nil)
	resp, err := full.Route(context.Background(), "pages",
		RequestIn{Method: http.MethodGet, Path: "/session"}, signedIn)
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
		RequestIn{Method: http.MethodGet, Path: "/session"}, signedIn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Body, signedIn.Email) || strings.Contains(resp.Body, signedIn.UserID) {
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
	if v, ok := loaded.settings["client_id"]; !ok || v.Reveal() != "configured-id" {
		t.Errorf("the configured value did not reach the add-on: %v", loaded.settings)
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
		RequestIn{Method: http.MethodGet, Path: "/echo"}, SessionContext{}); !isErr(err, ErrBusy) {
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
			}, SessionContext{})
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
	}, SessionContext{})
	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("a body of %d bytes answered %v with status %d, so the bound above is "+
			"refusing everything", maxStringIn/2, err, resp.Status)
	}
}
