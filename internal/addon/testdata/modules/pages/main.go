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
	"encoding/json"

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
