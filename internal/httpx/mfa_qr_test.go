package httpx

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// The enrolment code is drawn once and read by two consumers that are not the
// same kind of thing, and the class attribute is the whole of the difference.
//
// `/account/mfa` inlines the drawing into a page whose stylesheet defines
// `max-w-full h-auto`, and without them a 456px code takes a 360px viewport
// 174px sideways (F184). `POST /api/v1/auth/mfa/enrol` returns the same bytes as
// the `qr_svg` string in a JSON body, to a client that has never seen that
// stylesheet — where the same attribute is markup naming a rule the reader
// cannot satisfy, and is the thing link.Service.RenderQRBySlug already refuses
// to put on the file somebody downloads.
//
// Both halves, because one function serves both and a default is how the page's
// class reached the API body in the first place.

// TestTheEnrolmentCodeCarriesOnlyTheClassItIsAskedFor is the function's half.
func TestTheEnrolmentCodeCarriesOnlyTheClassItIsAskedFor(t *testing.T) {
	const uri = "otpauth://totp/linkctrl.test:owner@example.com" +
		"?secret=JBSWY3DPEHPK3PXP&issuer=linkctrl.test&algorithm=SHA1&digits=6&period=30"

	inline, err := mfaEnrolmentQR(uri, qr.FluidClass)
	if err != nil {
		t.Fatal(err)
	}
	if want := `class="` + qr.FluidClass + `"`; !strings.Contains(string(inline), want) {
		t.Errorf("the inlined enrolment code carries no %s, so /account/mfa is back "+
			"to the one page in the dashboard that scrolls sideways.\n\n%.140s…",
			want, inline)
	}

	body, err := mfaEnrolmentQR(uri, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "class=") {
		t.Errorf("the enrolment code drawn for the API body carries a class "+
			"attribute:\n\n%.140s…\n\nqr_svg is read by clients that do not have "+
			"this product's stylesheet, and a class list means nothing to them.",
			body)
	}
	// The picture is the same either way: the class bounds the element and the
	// geometry lives inside the viewBox, which is what makes the two consumers
	// able to share one drawing at all.
	if got := strings.Replace(string(inline), ` class="`+qr.FluidClass+`"`, "", 1); got != string(body) {
		t.Error("the two enrolment codes differ by more than the class attribute; " +
			"they are meant to be one picture served two ways")
	}
}

// TestEachEnrolmentCallSitePassesTheClassItsConsumerNeeds is the wiring's half,
// and it is a source scan for the reason the qr thumbnail's guard is one: what
// went wrong was not the drawing, it was which caller asked for what.
//
// Reading it out of the source rather than out of a served response because the
// handlers need an MFA service with a cipher, an audit writer and a notifier to
// answer at all — the end-to-end assertion that `qr_svg` has no class is in the
// contract test, which already boots that. This is what stops the *page*
// quietly losing its class, which no response in this package's tests renders.
func TestEachEnrolmentCallSitePassesTheClassItsConsumerNeeds(t *testing.T) {
	// file → the expression every mfaEnrolmentQR call in it must pass as its
	// class. The web file inlines into a page; the API file writes a JSON body.
	for file, want := range map[string]string{
		"web_mfa.go": "qr.FluidClass",
		"api_mfa.go": `""`,
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		var calls int
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "mfaEnrolmentQR" || len(call.Args) != 2 {
				return true
			}
			calls++
			var got strings.Builder
			if perr := printer.Fprint(&got, fset, call.Args[1]); perr != nil {
				t.Fatalf("%s: render the class argument: %v", file, perr)
			}
			if got.String() != want {
				t.Errorf("%s:%d passes %s as the enrolment code's class, want %s. "+
					"The page needs the bound that stops it scrolling sideways and the "+
					"API body must carry nothing that only resolves in this product's "+
					"stylesheet; one function serves both, which is how they diverged.",
					file, fset.Position(call.Pos()).Line, got.String(), want)
			}
			return true
		})
		if calls == 0 {
			t.Errorf("%s calls mfaEnrolmentQR nowhere, so this guard checks nothing. "+
				"If the call moved, move the assertion with it.", file)
		}
	}
}
