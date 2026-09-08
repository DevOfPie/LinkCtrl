//go:build wasip1

// Command identity is M65's fixture: a module that asserts a fake identity.
//
// It exists because the milestone's claims are about a boundary whose failure is
// an account takeover, and a boundary tested only from the host's side is a
// boundary tested against the host's own idea of what a module would send. This
// is a real consumer of the published SDK, compiled the way an add-on is
// compiled, and it asserts things a careless or hostile authentication add-on
// would assert: a subject nobody linked, a subject belonging to somebody else, a
// claim with no subject at all, two mints for one request, and a mint made while
// somebody is already signed in.
//
// **It never sees a token, and that is the assertion this file can make that no
// host-side test can.** MintedSession is decoded here, in the guest, and printed
// into the response body; a host test then reads the page. If a credential were
// ever added to that record, this fixture would print it and the test that reads
// the page would fail on it — which is a stronger claim than a host-side check on
// a Go struct, because it is made from the only place that matters.
//
// The exported name is the literal `linkctrl_http_handle`, for the reason the
// pages fixture writes it out: a //go:wasmexport directive cannot take a
// constant.
package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unsafe"

	"github.com/DevOfPie/LinkCtrl/sdk"
)

// wasmSessionMint is the raw import, declared here rather than reached through
// [sdk.SessionMint].
//
// The SDK's wrapper starts with a buffer larger than the record and grows on a
// short answer, so nothing that goes through it can ever offer the host a buffer
// too small — which is exactly the case the out-parameter convention is about,
// and therefore the case a fixture has to be able to reach. A publisher writing
// against the ABI directly rather than against the SDK writes this same line, so
// the fixture is not doing anything a module could not.
//
//go:wasmimport linkctrl session_mint
//go:noescape
func wasmSessionMint(claimPtr unsafe.Pointer, claimLen uint32,
	sessionPtr unsafe.Pointer, sessionLen uint32) int32

// claim and minted are the SessionClaim and MintedSession records, guest-side.
// Written out here rather than shared, because that is a publisher's position:
// the ABI's records are a documented JSON shape, not a Go type another repository
// can import.
type claim struct {
	Subject       string   `json:"subject"`
	Issuer        string   `json:"issuer"`
	Email         string   `json:"email,omitempty"`
	EmailVerified bool     `json:"email_verified,omitempty"`
	DisplayName   string   `json:"display_name,omitempty"`
	Groups        []string `json:"groups,omitempty"`
}

type minted struct {
	ExpiresAt            string `json:"expires_at"`
	SecondFactorRequired bool   `json:"second_factor_required"`
}

type request struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query"`
}

type response struct {
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Location    string `json:"location,omitempty"`
	Body        string `json:"body,omitempty"`
}

// issuer is fixed rather than taken from the query, because it is half of the key
// the host looks a link up by and a test that could vary both halves would be
// varying the thing it is asserting about.
const issuer = "https://idp.test"

//go:wasmexport linkctrl_http_handle
func handle() int32 {
	raw, err := sdk.HTTPRequestRead()
	if err != nil {
		_ = sdk.Log(sdk.LevelError, "identity: request unreadable: "+err.Error())
		return -1
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = sdk.Log(sdk.LevelError, "identity: request is not JSON: "+err.Error())
		return -1
	}
	// The subject the flow "authenticated". A query parameter, so one module can
	// stand in for an add-on that met a linked identity, an unlinked one, and one
	// belonging to a locked account, without three fixtures.
	subject := req.Query

	switch req.Path {
	case "/callback":
		return answer(mint(claim{
			Subject: subject, Issuer: issuer,
			// Deliberately an address that is **not** the account's. The host is
			// documented as reading neither this nor display_name, and a fixture that
			// sent the right address could not tell a host that ignores it from one
			// that matches on it. A host-side test asserts the account it signed in.
			Email:         "someone-else@elsewhere.test",
			EmailVerified: true,
			DisplayName:   "Asserted Name",
		}))

	case "/callback-twice":
		first := mint(claim{Subject: subject, Issuer: issuer})
		second := mint(claim{Subject: subject, Issuer: issuer})
		return answer("first: " + first + "; second: " + second)

	case "/callback-no-subject":
		// A claim the host cannot resolve anybody from. Refused as the module's own
		// fault rather than as an unknown identity, which is a distinction its author
		// needs: one is a bug in the module and the other is a person who has not
		// connected a provider.
		return answer(mint(claim{Issuer: issuer}))

	case "/callback-no-issuer":
		return answer(mint(claim{Subject: subject}))

	case "/callback-tiny-buffer":
		// The out-parameter convention, driven from the guest with a buffer that
		// cannot hold the answer. Published rule: nothing was written, retry with a
		// buffer that size. On this function that has to mean **nothing was minted**
		// either — otherwise the retry meets the one-mint guard and is refused while
		// the host is already setting a cookie. So both calls are made and both are
		// printed: the first must answer a size, and the second must mint.
		encoded := encode(claim{Subject: subject, Issuer: issuer})
		var tiny [1]byte
		n := wasmSessionMint(rawPtr(encoded), uint32(len(encoded)), unsafe.Pointer(&tiny[0]), 1)
		if n < 0 {
			return answer("tiny: refused " + strconv.Itoa(int(n)) + "; retry: not attempted")
		}
		return answer("tiny: size=" + strconv.Itoa(int(n)) + "; retry: " +
			mint(claim{Subject: subject, Issuer: issuer}))

	case "/callback-tiny-only":
		// The same short call and no retry after it, so a host-side test can assert
		// that the minter was never reached. With the retry in the same request both
		// orderings reach it exactly once and the count proves nothing.
		short := encode(claim{Subject: subject, Issuer: issuer})
		var one [1]byte
		return answer("tiny: size=" + strconv.Itoa(int(
			wasmSessionMint(rawPtr(short), uint32(len(short)), unsafe.Pointer(&one[0]), 1))))

	case "/connect":
		// The linking half, which is the only way anything this module does writes
		// the mapping the callback above is believed on.
		err := sdk.IdentityLink(encode(claim{Subject: subject, Issuer: issuer}))
		return answer("link: " + errText(err))

	case "/connect-no-subject":
		return answer("link: " + errText(sdk.IdentityLink(encode(claim{Issuer: issuer}))))

	default:
		return answer("identity fixture: no such path " + req.Path)
	}
}

// mint asserts one identity and reports, as text, everything the module learned.
//
// Everything, deliberately: the whole decoded record is printed, so a field added
// to MintedSession without this fixture changing still reaches the page and a
// host-side test asserting what a browser may see would fail on it.
func mint(c claim) string {
	raw, err := sdk.SessionMint(encode(c))
	if err != nil {
		return "refused " + errText(err)
	}
	var m minted
	if uerr := json.Unmarshal(raw, &m); uerr != nil {
		return "minted but unreadable: " + uerr.Error()
	}
	// The raw record as well as the decoded fields. A credential added to this
	// record would appear here even though the struct above has no field for it,
	// which is the point of printing both.
	return "minted expires_at=" + m.ExpiresAt +
		" second_factor_required=" + boolText(m.SecondFactorRequired) +
		" record=" + strings.TrimSpace(string(raw))
}

// rawPtr is bytesPtr, which the SDK keeps unexported. Same two lines and the
// same reason: a nil pointer with a zero length is what the ABI calls empty.
func rawPtr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(unsafe.SliceData(b))
}

func encode(c claim) []byte {
	b, err := json.Marshal(c)
	if err != nil {
		_ = sdk.Log(sdk.LevelError, "identity: could not encode a claim: "+err.Error())
		return nil
	}
	return b
}

func answer(body string) int32 {
	_ = sdk.Log(sdk.LevelInfo, "identity: "+body)
	if err := sdk.HTTPResponseWrite(mustEncode(response{ContentType: "text/plain", Body: body})); err != nil {
		_ = sdk.Log(sdk.LevelError, "identity: the response was refused: "+err.Error())
		return -1
	}
	return 0
}

func mustEncode(r response) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	return b
}

// errText names the status rather than printing the SDK's sentence.
//
// A short token, because a host-side test asserting on prose would be asserting
// on the SDK generator's wording — which is generated from the ABI's own status
// documentation and is free to change without any behaviour changing. The token
// is what the module actually branched on.
func errText(err error) string {
	switch {
	case err == nil:
		return "<nil>"
	case errors.Is(err, sdk.ErrInvalid):
		return "ErrInvalid"
	case errors.Is(err, sdk.ErrDenied):
		return "ErrDenied"
	case errors.Is(err, sdk.ErrNotFound):
		return "ErrNotFound"
	case errors.Is(err, sdk.ErrNotAvailable):
		return "ErrNotAvailable"
	case errors.Is(err, sdk.ErrInternal):
		return "ErrInternal"
	default:
		return "unknown: " + err.Error()
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func main() {}
