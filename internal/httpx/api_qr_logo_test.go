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
