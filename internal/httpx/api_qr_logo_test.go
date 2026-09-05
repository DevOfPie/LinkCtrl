package httpx

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/qr"
)

// The upload surface (M50.5), tested for the two things that live here rather
// than in internal/qr: that the cap is on the request and applies before
// anything is read, and that nothing the sender *names* is read at all.

// multipartBody builds a body the way a browser or a client library would,
// including the two fields this product must ignore.
func multipartBody(t *testing.T, field, filename, declared string, body []byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{
		`form-data; name="` + field + `"; filename="` + filename + `"`,
	}
	if declared != "" {
		h["Content-Type"] = []string{declared}
	}
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

func uploadRequest(t *testing.T, contentType string, body []byte) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/links/"+strings.Repeat("0", 8)+"/qr/codes/abcdefgh/logo", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	return httptest.NewRecorder(), req
}

// TestNothingAboutAnUploadsNameOrDeclaredTypeIsRead is the assertion m50.5.md
// asks for by name, made from both sides.
//
// **The behavioural half**: a part whose filename is a path escape and whose
// declared type is the one format this product refuses is still read as the PNG
// it contains, because neither string is consulted. Nothing downstream derives a
// storage path from user input either — under D134 there is no path at all, the
// row is keyed by a uuid this server generated and a slug this server generated
// — so the risk section's *path escape* has nowhere to land, and this is what
// says so rather than the absence of a bug report.
//
// **The source half**: the handler file is parsed and checked for any use of the
// two accessors that would reintroduce it. That is stronger than the behavioural
// half on its own, which would pass for a handler that read the filename and
// happened not to use it yet.
func TestNothingAboutAnUploadsNameOrDeclaredTypeIsRead(t *testing.T) {
	png := smallPNG(t)
	ct, body := multipartBody(t, "logo", "../../../etc/passwd", "image/svg+xml", png)
	w, r := uploadRequest(t, ct, body)

	got, err := readUploadedFile(w, r, "logo")
	if err != nil {
		t.Fatalf("a PNG with a hostile filename and a lying content type was refused: %v", err)
	}
	if !bytes.Equal(got.File, png) {
		t.Error("the bytes read are not the bytes sent")
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "api_qr.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	// Two accessors, and only one of them can be named unconditionally.
	// `FileName` belongs to `multipart.Part` alone, so any use of it in this
	// file is the defect. `Header` is also `http.ResponseWriter`'s, which every
	// handler here writes to, so it is flagged on the multipart part by name —
	// which is what makes the scan a check rather than a nuisance.
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "FileName":
			t.Errorf("api_qr.go reads .FileName at %s — the filename the sender chose. "+
				"No component of what is stored derives from user input",
				fset.Position(sel.Pos()))
		case "Header":
			if recv, isIdent := sel.X.(*ast.Ident); isIdent && recv.Name == "part" {
				t.Errorf("api_qr.go reads part.Header at %s, which carries the sender's "+
					"declared Content-Type. The decoder is chosen by the content",
					fset.Position(sel.Pos()))
			}
		}
		return true
	})
}

// TestTheBodyCapIsOnTheRequestAndNotOnThePart holds the ordering the milestone
// puts first: `MaxBytesReader` wraps the body before the multipart parser sees
// it, so the envelope is charged too and an oversized upload is refused by the
// read rather than after it.
func TestTheBodyCapIsOnTheRequestAndNotOnThePart(t *testing.T) {
	// One byte of payload under the cap, which the multipart envelope then
	// pushes over it. A cap applied to the part alone would let this through.
	oversized := bytes.Repeat([]byte{0x41}, qr.MaxLogoUploadBytes)
	ct, body := multipartBody(t, "logo", "big.png", "image/png", oversized)
	if len(body) <= qr.MaxLogoUploadBytes {
		t.Fatalf("the fixture is %d bytes and the cap is %d; it is meant to exceed it "+
			"by the envelope", len(body), qr.MaxLogoUploadBytes)
	}
	w, r := uploadRequest(t, ct, body)

	_, err := readUploadedFile(w, r, "logo")
	var errs domain.ValidationErrors
	if !errors.As(err, &errs) || errs[0].Code != "too_large" {
		t.Fatalf("an oversized body returned %v, want a too_large validation error", err)
	}
}

// TestABodyThatIsNotMultipartIsRefusedRatherThanGuessedAt covers the shapes a
// caller actually sends by mistake: JSON, a bare image, and a multipart body
// with the wrong part in it.
func TestABodyThatIsNotMultipartIsRefusedRatherThanGuessedAt(t *testing.T) {
	png := smallPNG(t)

	for name, tc := range map[string]struct {
		contentType string
		body        []byte
		wantCode    string
	}{
		"json":            {"application/json", []byte(`{"logo":"..."}`), "invalid"},
		"raw image":       {"image/png", png, "invalid"},
		"no content type": {"", png, "invalid"},
	} {
		w, r := uploadRequest(t, tc.contentType, tc.body)
		if tc.contentType == "" {
			r.Header.Del("Content-Type")
		}
		_, err := readUploadedFile(w, r, "logo")
		var errs domain.ValidationErrors
		if !errors.As(err, &errs) || errs[0].Code != tc.wantCode {
			t.Errorf("%s body returned %v, want a %s validation error", name, err, tc.wantCode)
		}
	}

	// A multipart body naming some other field. The part is skipped rather than
	// taken as the file, because a client that misnamed its field has not
	// uploaded a logo and should be told so.
	ct, body := multipartBody(t, "image", "logo.png", "image/png", png)
	w, r := uploadRequest(t, ct, body)
	_, err := readUploadedFile(w, r, "logo")
	var errs domain.ValidationErrors
	if !errors.As(err, &errs) || errs[0].Code != "required" {
		t.Errorf("a body with no logo part returned %v, want a required validation error", err)
	}
}

// TestTheFieldsBesideAnUploadAreReadWhereverTheySit is the dashboard's half of
// the reader (M50.5).
//
// A file cannot travel in an urlencoded form, so the panel's two hidden inputs —
// where the save returns to, and which code it is for — travel as parts of the
// same multipart body. **Their position must not matter**: a browser sends parts
// in markup order, and a reader that stopped at the file would make the values
// depend on where a template happens to put them. So they are asserted from both
// sides of it.
//
// The oversized case is asserted too, because the alternative to refusing is
// silent truncation: a return path cut in half is a redirect to somewhere nobody
// asked for, reported as success.
func TestTheFieldsBesideAnUploadAreReadWhereverTheySit(t *testing.T) {
	png := smallPNG(t)

	build := func(t *testing.T, before, after map[string]string) (string, []byte) {
		t.Helper()
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		write := func(fields map[string]string) {
			for name, value := range fields {
				if err := w.WriteField(name, value); err != nil {
					t.Fatal(err)
				}
			}
		}
		write(before)
		part, err := w.CreatePart(map[string][]string{
			"Content-Disposition": {`form-data; name="logo"; filename="a.png"`},
			"Content-Type":        {"image/png"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(png); err != nil {
			t.Fatal(err)
		}
		write(after)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return w.FormDataContentType(), buf.Bytes()
	}

	for name, tc := range map[string]struct{ before, after map[string]string }{
		"before the file": {before: map[string]string{"next": "/links/x/qr", "code": "abcdefgh"}},
		"after the file":  {after: map[string]string{"next": "/links/x/qr", "code": "abcdefgh"}},
		"either side": {
			before: map[string]string{"next": "/links/x/qr"},
			after:  map[string]string{"code": "abcdefgh"},
		},
	} {
		ct, body := build(t, tc.before, tc.after)
		w, r := uploadRequest(t, ct, body)
		got, err := readUploadedFile(w, r, "logo")
		if err != nil {
			t.Fatalf("%s: a valid upload was refused: %v", name, err)
		}
		if !bytes.Equal(got.File, png) {
			t.Errorf("%s: the file part is not the bytes that were sent", name)
		}
		if got.Fields.Get("next") != "/links/x/qr" || got.Fields.Get("code") != "abcdefgh" {
			t.Errorf("%s: the fields read back as %v, want next and code as sent",
				name, got.Fields)
		}
	}

	ct, body := build(t, map[string]string{
		"next": strings.Repeat("x", maxUploadFieldBytes+1),
	}, nil)
	w, r := uploadRequest(t, ct, body)
	_, err := readUploadedFile(w, r, "logo")
	var errs domain.ValidationErrors
	if !errors.As(err, &errs) || errs[0].Code != "too_large" {
		t.Errorf("a text field past its bound returned %v, want a too_large "+
			"validation error rather than a truncated value", err)
	}
}

// smallPNG is a real PNG, produced the way everything else in this product
// produces one.
func smallPNG(t *testing.T) []byte {
	t.Helper()
	out, err := qr.RenderPNG("https://example.test/x", qr.Style{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// --- the warning, at the F214 reopening ---------------------------------------

// The dashboard's half. An image past the storage target is stored at a smaller
// size than it arrived at, and the page it lands on is a fresh request that
// never saw the file — so the pair travels in the redirect and qrNotice is the
// one place it is spent. **The only such pair now**: the size control had a
// `want`/`got` one on the same terms until D182 made the two numbers equal at
// every value, and it went with the snap it reported.

// TestTheResizeWarningNamesBothSizes is the sentence the owner is owed: what was
// uploaded, and what is stored. A "logo stored" with no qualification for an
// image this product silently shrank is the reopening's own complaint.
func TestTheResizeWarningNamesBothSizes(t *testing.T) {
	got := qrNotice(url.Values{"qr": {"logo"}, "from": {"813x813"}, "to": {"512x512"}})
	for _, want := range []string{"813×813", "512×512", "resized"} {
		if !strings.Contains(got, want) {
			t.Errorf("the notice %q does not carry %q", got, want)
		}
	}
	// And the ordinary sentence survives underneath it: the two things M50.6
	// changed about the picture are still what a reader has to be told.
	if !strings.Contains(got, "level H") {
		t.Errorf("the resize warning replaced the stored-logo sentence rather than "+
			"adding to it: %q", got)
	}
}

// TestTheRemovalNoticeNoLongerPromisesH is F223 on the surface that promised it.
//
// The sentence said the code *stays* at level H and gave M50.6's reason. The
// owner overruled the reason, so the sentence is a claim the product stopped
// making — and a notice that describes the previous behaviour is worse than no
// notice, because a reader who checks it is checking against the wrong thing.
// This is also the only place the level is stated to somebody who did not
// upload anything, which is D186's answer to whether the tab says it at all.
func TestTheRemovalNoticeNoLongerPromisesH(t *testing.T) {
	got := qrNotice(url.Values{"qr": {"logo_removed"}})
	if got == "" {
		t.Fatal("removing a logo says nothing at all")
	}
	if strings.Contains(got, "stays at error correction level H") {
		t.Errorf("the notice still promises the code stays at H: %q", got)
	}
	for _, want := range []string{"Error correction goes back", "already printed"} {
		if !strings.Contains(got, want) {
			t.Errorf("the notice %q does not carry %q — a reader is owed what "+
				"changed and that nothing printed stops working", got, want)
		}
	}
}

// TestAnUnresizedUploadSaysNothingExtra is the other side. A sentence printed
// after every upload is a sentence nobody reads by the third one — the reasoning
// that deleted the size control's snap sentence outright at D182, once the size
// asked for and the size drawn stopped being able to differ. This warning
// survives that argument because the two dimensions still can.
func TestAnUnresizedUploadSaysNothingExtra(t *testing.T) {
	for name, q := range map[string]url.Values{
		"no pair at all":  {"qr": {"logo"}},
		"same both ways":  {"qr": {"logo"}, "from": {"300x200"}, "to": {"300x200"}},
		"half a pair":     {"qr": {"logo"}, "from": {"813x813"}},
		"not two numbers": {"qr": {"logo"}, "from": {"lots"}, "to": {"a few"}},
		// Hand-edited out of range, which is what re-deriving is for: a sentence
		// assembled from a query string is a sentence somebody else can write.
		"past the bound": {"qr": {"logo"}, "from": {"99999x99999"}, "to": {"1x1"}},
	} {
		if got := qrNotice(q); strings.Contains(got, "resized") {
			t.Errorf("%s produced a resize warning: %q", name, got)
		}
	}
}

// --- the size-raised notice, at M70 (F230) ------------------------------------

// TestTheSizeRaisedNoticeIsTheOnlyPartOfTheRuleAReaderSees covers the branch that
// turns M49's re-fit arithmetic into words.
//
// The arithmetic is covered at both levels — the service and the API — and the
// dashboard branch that renders it was covered nowhere: no test named `qrNotice`
// or `sizeParam`, and no test drove `CreateQRCode` through the web handler at all.
// The behaviour was driven by hand on a running instance when M49 shipped, which
// is an uncaptured verification rather than an unverified claim.
//
// **It is worth a test because the silence is the design.** A re-fit that kept the
// number says nothing, on the owner's own rule, so a branch that stopped rendering
// would look exactly like the common case. Nothing else the reader ever sees says
// their size was raised.
//
// Here rather than in the browser suite: what is asserted is which sentence the
// pair produces, and that is this function's answer rather than a rendering.
func TestTheSizeRaisedNoticeIsTheOnlyPartOfTheRuleAReaderSees(t *testing.T) {
	const base = "A second code, with a name and an identity of its own."

	t.Run("a raised size names both numbers", func(t *testing.T) {
		got := qrNotice(url.Values{"qr": {"added"}, "from": {"86"}, "to": {"94"}})
		for _, want := range []string{base, "94px", "86px", "raised"} {
			if !strings.Contains(got, want) {
				t.Errorf("the notice %q does not carry %q", got, want)
			}
		}
	})

	// Every shape that is not two sizes in range with the second above the first
	// falls back to the ordinary sentence, and each arrives in a URL anybody can
	// edit — which is why the handler re-derives them rather than echoing them.
	for _, tc := range []struct{ name, from, to string }{
		{"no pair at all", "", ""},
		{"an equal pair, which is a re-fit that kept the number", "94", "94"},
		{"an inverted pair", "94", "86"},
		{"a from outside the range", "1", "94"},
		{"a to outside the range", "86", "999999"},
		{"text where a number goes", "eighty-six", "ninety-four"},
		{"a negative", "-86", "94"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := qrNotice(url.Values{"qr": {"added"}, "from": {tc.from}, "to": {tc.to}})
			if got != base+" It points at the same destination — what it changes is that "+
				"a scan of this one is told apart from a scan of the others." {
				t.Errorf("%s produced the raised-size sentence: %q. Anything that is not "+
					"a real rise says nothing, which is the rule the owner set", tc.name, got)
			}
		})
	}
}
