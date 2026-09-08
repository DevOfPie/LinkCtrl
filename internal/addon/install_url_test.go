package addon

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/crc32"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/audit"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// M68.6's tests. What they are about is the *second door* — what is fetched,
// what is checked before anything is parsed, and that what lands is
// indistinguishable from what an upload lands. The permission gate needs an
// identity only internal/auth can mint and is asserted in test/integration, the
// way M67's is.

// --- the central claim ---------------------------------------------------------

// **The bullet, whole: the same module, both ways, and the directory afterwards
// is identical.**
//
// This is the assertion that makes *from the digest check onward the path is
// M67's* a fact rather than a design intention. If the fetch path grew a second
// manifest reader, re-encoded the manifest, wrote the module under a name from the
// archive instead of from the manifest, or staged differently, this fails — and it
// fails by naming the file that differs.
func TestTheSameModuleInstallsBothWaysAndLandsIdenticallyOnDisk(t *testing.T) {
	code := fixture(t, "minimal")
	m := manifestFor("arrival", ClassDegrade, code)
	manifest := marshalManifest(t, m)
	good := []member{{name: ManifestFile, body: manifest}, {name: m.Module, body: code}}

	// A host per install, because one host refuses the second install of a name —
	// which is itself M67's rule holding for the new door, asserted below.
	uploaded, upDir, _, _ := lifecycleHost(t)
	if _, err := uploaded.install(t.Context(), nil, InstallRequest{
		Manifest: manifest, Module: code,
	}); err != nil {
		t.Fatalf("installing by upload: %v", err)
	}

	// **Once per container, against the one upload.** D384 accepts three, and what
	// makes that a widening of the door rather than of the add-on is that all three
	// land the same directory — so the comparison is run three times rather than
	// once, and a zip that arrived byte-identical to a tar is what says the
	// container stopped mattering at the digest check.
	for _, f := range bundleFormats {
		t.Run(f.name, func(t *testing.T) {
			bundle := f.pack(t, good)
			fetched, fetchDir, rec, _ := lifecycleHost(t)
			ts := serveBundle(t, bundle)
			reachable(t, fetched, ts)
			out, err := fetched.installFromURL(t.Context(), nil, URLInstallRequest{
				URL: ts.URL + "/arrival." + f.name, SHA256: digestOf(bundle),
			})
			if err != nil {
				t.Fatalf("installing from a URL, as %s: %v", f.name, err)
			}
			if out.Name != "arrival" || out.SHA256 != m.SHA256 {
				t.Errorf("the answer describes %+v, want arrival at %s", out, m.SHA256)
			}
			if fetched.Len() != 1 {
				t.Errorf("the host runs %d add-ons after a URL install", fetched.Len())
			}

			sameTree(t, filepath.Join(upDir, "arrival"), filepath.Join(fetchDir, "arrival"))

			// The audit record is M67's too, and it says the same things: what an
			// operator reads later must not depend on which door the module came
			// through, and now not on which container either.
			assertAudited(t, rec, audit.ActionAddonInstalled, "arrival", m.SHA256)
		})
	}
}

// sameTree compares two installed add-on directories entry by entry and byte for
// byte.
func sameTree(t *testing.T, a, b string) {
	t.Helper()
	names := func(dir string) []string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	an, bn := names(a), names(b)
	if strings.Join(an, ",") != strings.Join(bn, ",") {
		t.Fatalf("an upload wrote %v and a URL install wrote %v; the two doors must "+
			"produce the same add-on", an, bn)
	}
	for _, name := range an {
		x, err := os.ReadFile(filepath.Join(a, name))
		if err != nil {
			t.Fatal(err)
		}
		y, err := os.ReadFile(filepath.Join(b, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(x, y) {
			t.Errorf("%s differs between an upload and a URL install: %d bytes against %d. "+
				"The manifest is written verbatim on both paths and the module is written "+
				"under the name its manifest declares", name, len(x), len(y))
		}
	}
}

// --- the security claim --------------------------------------------------------

// **m68.6.md's own test, in its own words: a manifest whose `sha256` matches a
// module the operator did not expect.**
//
// The bundle served here is internally perfect. Its manifest declares the digest
// of the module beside it, so every check M67 makes would pass — and it is not the
// bundle the operator named. Nothing but the operator's own digest can tell the
// difference, which is the whole reason that field exists, and it is why a
// checksum fetched from the same place as the module is worth nothing.
func TestAManifestFetchedFromTheURLIsNotTheSourceOfItsOwnModulesDigest(t *testing.T) {
	expected := fixture(t, "minimal")
	em := manifestFor("arrival", ClassDegrade, expected)
	wanted := tarBundle(t, map[string][]byte{
		ManifestFile: marshalManifest(t, em), em.Module: expected,
	})

	// What the origin actually serves: a different module, and a manifest that
	// describes *it* correctly.
	substitute := append(append([]byte{}, expected...), 0x00)
	sm := manifestFor("arrival", ClassDegrade, substitute)
	served := tarBundle(t, map[string][]byte{
		ManifestFile: marshalManifest(t, sm), sm.Module: substitute,
	})
	if digestOf(served) == digestOf(wanted) {
		t.Fatal("the two bundles hash the same, so this test proves nothing")
	}

	h, dir, _, _ := lifecycleHost(t)
	ts := serveBundle(t, served)
	reachable(t, h, ts)
	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/arrival.tar", SHA256: digestOf(wanted),
	})
	assertFieldCode(t, err, CodeDigestMismatch)
	if h.Len() != 0 {
		t.Error("a bundle that is not the one the operator named was installed")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the add-ons directory holds %d entries after a refused fetch; the "+
			"digest is checked before anything is parsed and before anything is written",
			len(entries))
	}
}

// The address policy is M68.5's and it is not relaxed by an operator having typed
// the URL. This is the one test in this file that leaves `allowAddr` alone: the
// server really is on loopback and the dial really is refused.
func TestAUrlInstallCannotReachAnAddressTheHostDoesNotDial(t *testing.T) {
	code := fixture(t, "minimal")
	m := manifestFor("arrival", ClassDegrade, code)
	bundle := tarBundle(t, map[string][]byte{
		ManifestFile: marshalManifest(t, m), m.Module: code,
	})

	h, _, _, _ := lifecycleHost(t)
	ts := serveBundle(t, bundle)
	// The certificate is trusted and the address is not, so what is being measured
	// is the address policy rather than a handshake failing for another reason.
	patchTLS(t, h.installFetcher, ts)

	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/arrival.tar", SHA256: digestOf(bundle),
	})
	assertFieldCode(t, err, "fetch_address_refused")
	if h.Len() != 0 {
		t.Error("an add-on was installed from an address this host does not dial")
	}
}

// Cleartext is refused before anything is dialled, by the same [originOf] an
// add-on's own fetch passes through.
func TestAUrlInstallIsHTTPSOnly(t *testing.T) {
	h, _, _, _ := lifecycleHost(t)
	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: "http://example.com/x.tar", SHA256: strings.Repeat("a", 64),
	})
	assertFieldCode(t, err, CodeURLInvalid)
}

// A digest that is not a digest is its own refusal, and it happens before any
// request is made: the operator mistyped a field, and dialling somebody's server
// to discover that would be a request made for nothing.
func TestADigestThatIsNotADigestIsRefusedBeforeAnythingIsFetched(t *testing.T) {
	h, _, _, _ := lifecycleHost(t)
	var asked bool
	ts := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		asked = true
	}))
	t.Cleanup(ts.Close)
	reachable(t, h, ts)

	for _, digest := range []string{"", "not-a-digest", strings.Repeat("a", 63),
		strings.ToUpper(strings.Repeat("a", 64)) + "b"} {
		_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
			URL: ts.URL + "/x.tar", SHA256: digest,
		})
		assertFieldCode(t, err, CodeDigestInvalid)
	}
	if asked {
		t.Error("the origin was asked for a bundle before the digest field was read; " +
			"a mistyped field is not a reason to make somebody else's server work")
	}

	// Upper case and surrounding space are the two ways a digest arrives from a
	// terminal, and neither is a mistyping. They are normalized rather than
	// refused, and the proof is that the refusal below is about the fetch.
	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/x.tar", SHA256: "  " + strings.ToUpper(strings.Repeat("ab", 32)) + "\n",
	})
	assertFieldCode(t, err, CodeDigestMismatch)
}

// Everything that is not a 200 is its own refusal naming the status, because a
// release page answering 404 is the commonest way this fails and it has nothing to
// do with a digest.
func TestAnOriginThatDoesNotAnswerWithTheBundleSaysSo(t *testing.T) {
	h, _, _, _ := lifecycleHost(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such release", http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)
	reachable(t, h, ts)

	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/gone.tar", SHA256: strings.Repeat("a", 64),
	})
	assertFieldCode(t, err, CodeFetchStatus)
}

// The size cap is the install's own, not the one a discovery document gets, and a
// body past it comes back as a refusal naming that bound.
func TestABundleOverTheInstallCapIsRefused(t *testing.T) {
	h, _, _, _ := lifecycleHost(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// One byte past whatever this fetcher was built with, written without
		// holding the whole thing: the fetcher is patched down to a small cap below
		// so this stays a test about the bound rather than about 32 MiB of RAM.
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(bytes.Repeat([]byte{0}, 4097))
	}))
	t.Cleanup(ts.Close)
	reachable(t, h, ts)
	h.installFetcher.maxBytes = 4096

	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/big.tar", SHA256: strings.Repeat("a", 64),
	})
	assertFieldCode(t, err, "fetch_too_large")
}

// The cap the *product* ships is the install bound rather than the add-on one,
// which is the sentence m68.6.md asked to be said out loud rather than inherited.
func TestTheInstallFetchIsSizedForAModuleAndNotForADocument(t *testing.T) {
	h, _, _, _ := lifecycleHost(t)
	if got := h.installFetcher.maxBytes; got != MaxUploadBytes {
		t.Errorf("a bundle fetch carries at most %d bytes; an install is bounded at %d "+
			"whichever door it comes through", got, MaxUploadBytes)
	}
	if h.installFetcher.maxBytes <= h.fetcher.maxBytes {
		t.Errorf("the install cap (%d) is not larger than an add-on's response cap (%d); "+
			"one was sized for a .wasm and the other for a JSON document",
			h.installFetcher.maxBytes, h.fetcher.maxBytes)
	}
	if h.installFetcher.timeout != InstallFetchTimeout {
		t.Errorf("a bundle fetch is bounded at %s, want %s",
			h.installFetcher.timeout, InstallFetchTimeout)
	}
	if InstallFetchTimeout <= DefaultFetchTimeout {
		t.Errorf("the install timeout (%s) is not longer than an add-on's (%s), and a "+
			"module is three orders of magnitude larger than a discovery document",
			InstallFetchTimeout, DefaultFetchTimeout)
	}
}

// --- the bundle ---------------------------------------------------------------

// Everything a bundle can be that is not an add-on, **asserted against every
// container this door accepts**.
//
// The table is written once and run three times, which is D384's instruction and
// the reason it is an instruction: the member rule is what makes accepting three
// formats affordable, so a rule that held in the tar reader and not in the zip
// reader would be the whole of the cost with none of the defence. A refusal that
// exists in one format and not another fails here, by name, saying which.
//
// The digest is correct in every row — so what is being asserted is the reader
// rather than the digest catching it first — and nothing lands in the add-ons
// directory in any of them.
func TestWhatAnAddonBundleMayHold(t *testing.T) {
	code := fixture(t, "minimal")
	m := manifestFor("arrival", ClassDegrade, code)
	manifest := marshalManifest(t, m)
	manifestMember := member{name: ManifestFile, body: manifest}

	// `says` is set only where the code alone cannot tell the refusals apart: two
	// of these shapes are refused by a clause of their own and would *also* be
	// refused by the count at the end, so without the sentence the test would pass
	// with the clause deleted.
	for _, tc := range []struct {
		name    string
		members []member
		code    string
		says    string
	}{
		{"no manifest in it", []member{{name: m.Module, body: code}}, CodeBundleInvalid, ""},
		{"no module in it", []member{manifestMember}, CodeBundleInvalid, ""},
		{"a third file", []member{
			manifestMember, {name: m.Module, body: code}, {name: "README", body: []byte("hi")},
		}, CodeBundleInvalid, ""},
		{"more entries than it will walk", []member{
			manifestMember, {name: m.Module, body: code},
			{name: "a", body: []byte("a")}, {name: "b", body: []byte("b")},
		}, CodeBundleInvalid, "more than 3 entries"},
		{"a module the manifest does not name", []member{
			manifestMember, {name: "somethingelse.wasm", body: code},
		}, CodeBundleMismatch, ""},
		{"a member under a path", []member{
			manifestMember, {name: "nested/" + m.Module, body: code},
		}, CodeBundleInvalid, ""},
		{"a member escaping the directory", []member{
			manifestMember, {name: "../" + m.Module, body: code},
		}, CodeBundleInvalid, ""},
		{"a directory entry", []member{
			manifestMember, {name: "migrations", kind: memDir}, {name: m.Module, body: code},
		}, CodeBundleInvalid, ""},
		{"a symbolic link", []member{
			manifestMember, {name: m.Module, kind: memSymlink},
		}, CodeBundleInvalid, ""},
		// **Two manifests, and that is the shape the clause is for.** A tar reader
		// walking a stream takes the last entry and a zip reader indexing a central
		// directory may take either, so an archive carrying a benign addon.json and
		// a hostile one is an archive that means different things to different
		// tools — and, with the duplicate clause gone, one that installs whichever
		// this reader happened to keep. Two *modules* would not test it: the count
		// at the end refuses those anyway.
		{"the same name twice", []member{
			manifestMember,
			{name: ManifestFile, body: marshalManifest(t, manifestFor("other", ClassRequired, code))},
			{name: m.Module, body: code},
		}, CodeBundleInvalid, "twice"},
	} {
		for _, f := range bundleFormats {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				h, dir, _, _ := lifecycleHost(t)
				bundle := f.pack(t, tc.members)
				ts := serveBundle(t, bundle)
				reachable(t, h, ts)
				_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
					URL: ts.URL + "/x.bin", SHA256: digestOf(bundle),
				})
				assertFieldCode(t, err, tc.code)
				if tc.says != "" && !strings.Contains(errorText(err), tc.says) {
					t.Errorf("the refusal is %q and does not say %q; this shape is refused "+
						"by a clause of its own and the code alone cannot show that",
						errorText(err), tc.says)
				}
				if entries, _ := os.ReadDir(dir); len(entries) != 0 {
					t.Errorf("the add-ons directory holds %d entries after %s in a %s",
						len(entries), tc.name, f.name)
				}
			})
		}
	}
}

// Bytes that are not any of the three containers are refused before a parser is
// chosen at all, and the refusal says what a bundle is rather than naming a
// format the operator did not use.
func TestBytesThatAreNotAnArchiveAtAll(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"a manifest on its own", []byte(`{"schema_version":1}`)},
		{"an HTML error page a proxy served", []byte("<!doctype html><title>404</title>")},
		{"a wasm module with no container round it", fixture(t, "minimal")},
		{"nothing at all", []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := lifecycleHost(t)
			ts := serveBundle(t, tc.raw)
			reachable(t, h, ts)
			_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
				URL: ts.URL + "/x.tar", SHA256: digestOf(tc.raw),
			})
			assertFieldCode(t, err, CodeBundleInvalid)
		})
	}
}

// **The container comes from the bytes and never from the name**, which is D384's
// first obligation and is a security property rather than a convenience: the URL
// was typed by whoever is trusted least in this transaction, so its extension is a
// claim and the magic number is a fact.
//
// Every pairing of a lying name with a real container, and each one installs.
func TestTheContainerIsDetectedFromContentAndNotFromTheURL(t *testing.T) {
	code := fixture(t, "minimal")
	m := manifestFor("arrival", ClassDegrade, code)
	good := []member{{name: ManifestFile, body: marshalManifest(t, m)}, {name: m.Module, body: code}}

	for _, f := range bundleFormats {
		for _, lie := range []string{"/a.tar", "/a.tar.gz", "/a.zip", "/download", "/a.txt"} {
			t.Run(f.name+lie, func(t *testing.T) {
				h, _, _, _ := lifecycleHost(t)
				bundle := f.pack(t, good)
				ts := serveBundle(t, bundle)
				reachable(t, h, ts)
				if _, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
					URL: ts.URL + lie, SHA256: digestOf(bundle),
				}); err != nil {
					t.Errorf("a %s bundle served at %s was refused: %v — the URL's "+
						"extension is not an input to the reader", f.name, lie, err)
				}
			})
		}
	}
}

// --- what a compressed container costs, and what refuses it --------------------

// **The absolute inflated cap, and it is tested with an archive the ratio would
// let through.** That is the whole difficulty of asserting this bound: the
// obvious bomb — a few kilobytes of gzipped zeros amounting to 33 MiB — is
// refused by the ratio long before the cap is reached, so a test built on one
// would still pass with the cap deleted.
//
// So this bundle is *plausibly* compressed and simply enormous: nearly a
// mebibyte of incompressible padding puts the fetched size high enough that fifty
// times it is past the cap, and the guard below asserts exactly that rather than
// leaving it to arithmetic in a comment. What refuses it can only be the absolute
// figure, which is the shape of the operator this bound is for — somebody whose
// release archive is real and too large.
//
// Written through a stream rather than from a slice: the point is that the *host*
// never holds 33 MiB, and a test that built the bomb in RAM would be conceding
// half of it.
func TestACompressedBundleThatUnpacksPastTheCapIsRefused(t *testing.T) {
	const size = maxBundleInflated + (1 << 20)
	const padding = 900 << 10
	pad := make([]byte, padding)
	if _, err := rand.Read(pad); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: ManifestFile, Mode: 0o644, Size: size, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(pad); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(tw, zeroes{}, size-padding); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	bomb := buf.Bytes()
	if int64(len(bomb)) > MaxUploadBytes {
		t.Fatalf("the bomb is %d bytes on the wire and the fetch cap would refuse it "+
			"first, which would make this a test about the wrong bound", len(bomb))
	}
	if limit := bundleInflatedLimit(int64(len(bomb))); limit != maxBundleInflated {
		t.Fatalf("this bundle is %d bytes fetched, so its bound is %d rather than the "+
			"cap and the ratio is what would refuse it; the padding is meant to make "+
			"the cap the binding figure", len(bomb), limit)
	}

	h, dir, _, _ := lifecycleHost(t)
	ts := serveBundle(t, bomb)
	reachable(t, h, ts)
	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/bomb.tar.gz", SHA256: digestOf(bomb),
	})
	assertFieldCode(t, err, CodeBundleExpands)
	// **The sentence, not only the code.** Two bounds share this code deliberately;
	// what an operator has to be able to tell apart is which of them their archive
	// hit, so the assertion is that this one names the size and *not* the ratio.
	if !strings.Contains(errorText(err), "unpacks to more than "+byteBound(maxBundleInflated)) {
		t.Errorf("the refusal is %q and does not name the absolute cap it hit",
			errorText(err))
	}
	if strings.Contains(errorText(err), "times the bytes") {
		t.Errorf("the refusal is %q, which is the ratio's sentence; this archive "+
			"compresses at a rate a publisher's does and the cap is what refused it",
			errorText(err))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the add-ons directory holds %d entries after a decompression bomb",
			len(entries))
	}
}

// **And the bound is where the reading stops, not where the verdict is passed.**
// This is the assertion the other two cannot make: both of them would pass just
// as happily against a reader that inflated a bomb to 32 MiB and only then
// declined it, having already spent the allocation and the CPU the ratio exists
// to refuse.
//
// It is made without measuring anything, by putting bytes past the end of the
// gzip member that are not a gzip member and are not anything else. A reader
// bounded at [bundleInflatedLimit] never reaches them and refuses for expanding;
// a reader that runs the stream to its end meets them and refuses with
// `bundle_invalid`, because what it found is a corrupt stream. The two refusals
// are different codes, so *where the read stopped* is observable from outside
// this package rather than inferred from a benchmark.
func TestACompressedBundleStopsReadingAtItsBoundRatherThanAtTheCap(t *testing.T) {
	const size = 2 << 20
	bomb := gzipped(t, packTar(t, []member{{name: ManifestFile, body: make([]byte, size)}}))
	bomb = append(bomb, []byte("not a gzip header, and nothing may ever read this far")...)

	limit := bundleInflatedLimit(int64(len(bomb)))
	if limit != maxBundleRatioFloor || limit >= size {
		t.Fatalf("this bundle is %d bytes fetched and its bound is %d; the test needs "+
			"a bound below the %d bytes of payload, so that the trailing bytes sit "+
			"past it", len(bomb), limit, size)
	}

	h, dir, _, _ := lifecycleHost(t)
	ts := serveBundle(t, bomb)
	reachable(t, h, ts)
	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/trailing.tar.gz", SHA256: digestOf(bomb),
	})
	// A `bundle_invalid` here would mean the reader reached the trailing bytes, and
	// it can only reach them by inflating the whole payload first.
	assertFieldCode(t, err, CodeBundleExpands)
	if !strings.Contains(errorText(err), "times the bytes") {
		t.Errorf("the refusal is %q; the bound this bundle passed is the ratio's",
			errorText(err))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the add-ons directory holds %d entries after a decompression bomb",
			len(entries))
	}
}

// **The ratio, which is the bound the absolute cap does not provide.** Two
// mebibytes of zeros is inside every other number at this door — under the fetch
// cap, under the inflated cap — and it still expands by a factor nothing a
// publisher builds comes near. Refused, and the refusal says the ratio, because
// *too big* with no figure in it is a refusal an operator cannot act on.
func TestABundleThatExpandsImplausiblyIsRefusedEvenInsideTheCap(t *testing.T) {
	const size = 2 << 20
	bomb := gzipped(t, packTar(t, []member{{name: ManifestFile, body: make([]byte, size)}}))
	if int64(len(bomb)) > maxBundleInflated || size <= maxBundleRatioFloor {
		t.Fatalf("this bundle is %d bytes fetched and %d inflated; it is meant to pass "+
			"the absolute cap and fail only the ratio", len(bomb), size)
	}
	if bundleInflatedLimit(int64(len(bomb))) >= maxBundleInflated {
		t.Fatalf("this bundle's bound is the absolute cap, so the ratio is not what " +
			"would refuse it and the assertions below would be about the wrong bound")
	}

	h, _, _, _ := lifecycleHost(t)
	ts := serveBundle(t, bomb)
	reachable(t, h, ts)
	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/ratio.tar.gz", SHA256: digestOf(bomb),
	})
	assertFieldCode(t, err, CodeBundleExpands)
	if !strings.Contains(errorText(err), "times the bytes") {
		t.Errorf("the refusal is %q and does not name the ratio it hit; every refusal "+
			"at this door says which bound bit", errorText(err))
	}
	// The other half of the discrimination the test above makes: this archive is
	// nowhere near the absolute cap, so a refusal wording itself as the cap would
	// mean the two bounds had collapsed into one sentence.
	if strings.Contains(errorText(err), "as much as an add-on bundle may be") {
		t.Errorf("the refusal is %q, which is the absolute cap's sentence, and this "+
			"bundle is %d bytes inflated against a cap of %d", errorText(err), size,
			maxBundleInflated)
	}
}

// **The region the ratio actually governs, which neither test above enters.**
//
// Both of them are built out of a couple of kilobytes on the wire, and a couple
// of kilobytes buys [maxBundleRatioFloor] — so what refuses them is the floor
// wearing the ratio's sentence, and `maxBundleRatio` could be lowered to almost
// anything without either of them noticing. The figure only binds between roughly
// 21 KB fetched, where fifty times the fetched size passes the floor, and 640 KB,
// where it reaches the cap. This test is built inside that window and is what
// pins the constant: fifty is written here as a literal, so a change to it makes
// one of the two cases below fail whichever direction it moves in.
//
// Two cases, and they are one byte apart: a stream that inflates to exactly the
// limit is read to its end, and one byte more is refused. That is the same
// boundary the cap's tests assert at the other end of the range, made here where
// the fetched size is what sets it.
//
// Against [inflate] rather than through an install, for
// [TestASmallBundleIsNotRefusedForExpandingSharply]'s reason inverted: five
// megabytes of padding is not a tar whatever the ratio says about it, so an
// end-to-end version could only assert that the accepted case went on to be
// refused for something else, which is a weaker thing to have proved.
func TestTheRatioBindsWhereTheFetchedSizeSetsTheLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		over int
	}{
		{"exactly the limit is read to the end", 0},
		{"one byte past it is refused", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, payload := ratioRegionBundle(t, tc.over)
			limit := int64(ratioUnderTest * len(raw))
			// The window, asserted rather than assumed: below the floor or above the
			// cap this would be a second test about a bound that already has two.
			if limit <= maxBundleRatioFloor || limit >= maxBundleInflated {
				t.Fatalf("%d bytes fetched sets a limit of %d, which is outside the range "+
					"the ratio governs (%d to %d); the construction has drifted and this "+
					"test is about the wrong bound", len(raw), limit, maxBundleRatioFloor,
					maxBundleInflated)
			}
			// And the figure itself, which is the whole reason this file needs a third
			// ratio test: fifty here is a literal, not the constant.
			if got := bundleInflatedLimit(int64(len(raw))); got != limit {
				t.Errorf("a bundle fetched as %d bytes is bounded at %d; %d bytes fetched "+
					"may amount to %d times that and no more", len(raw), got, len(raw),
					ratioUnderTest)
			}

			out, err := inflate(raw)
			if tc.over == 0 {
				if err != nil {
					t.Fatalf("a bundle that inflates to exactly its %d-byte limit was "+
						"refused: %v — the limit is what a bundle may amount to, and a "+
						"bundle that is precisely as large as it may be is inside it",
						limit, err)
				}
				if len(out) != payload {
					t.Errorf("the reader produced %d bytes of a %d-byte stream", len(out), payload)
				}
				return
			}
			if err == nil {
				t.Fatalf("a bundle that inflates to %d bytes, one past its %d-byte limit, "+
					"was accepted", payload, limit)
			}
			if got := bundleRefusal(err); got != CodeBundleExpands {
				t.Errorf("a bundle past its ratio bound is refused as %q; the archive "+
					"parsed and what is wrong with it is a number", got)
			}
			if !strings.Contains(err.Error(), "times the bytes") {
				t.Errorf("the refusal is %q and does not name the ratio; a bundle this "+
					"size is nowhere near the cap and the operator's problem is how fast "+
					"it expands", err)
			}
		})
	}
}

// ratioUnderTest is [maxBundleRatio] written out, and the duplication is the
// point: a test that computes its expectation from the constant it is checking
// asserts that the constant equals itself. This is the figure D385 decided, and
// [TestTheRatioBindsWhereTheFetchedSizeSetsTheLimit] is where the two are held
// against each other.
const ratioUnderTest = 50

// ratioRegionBundle builds a gzip stream that is fetched as enough bytes for the
// ratio to be the binding figure, and that inflates to exactly `over` bytes past
// the limit those fetched bytes set.
//
// **The construction is a fixed point and has to be**, because the limit is a
// multiple of the compressed size and padding the payload to reach it changes the
// compressed size in turn. Each pass sets the padding to what the last pass's
// compressed size asked for; the loop converges in about five because a byte of
// padding costs a thousandth of a byte compressed, so each pass closes the gap by
// a factor of twenty. It ends on an exact fixed point or it fails — an
// approximate one would leave the assertion above about a boundary the bundle is
// merely near.
//
// The incompressible head is derived rather than random for the same reason:
// convergence has to be a property of the test and not of the run it happened in.
func ratioRegionBundle(t *testing.T, over int) (raw []byte, inflated int) {
	t.Helper()
	const head = 100 << 10
	incompressible := make([]byte, head)
	d := sha256.Sum256([]byte("m68.6 ratio region"))
	for i := 0; i < head; i += len(d) {
		d = sha256.Sum256(d[:])
		copy(incompressible[i:], d[:])
	}

	pad := 4 << 20
	for range 16 {
		payload := make([]byte, head+pad)
		copy(payload, incompressible)
		raw = gzipped(t, payload)
		want := ratioUnderTest*len(raw) + over - head
		if want == pad {
			return raw, len(payload)
		}
		pad = want
	}
	t.Fatalf("no padding makes a bundle that inflates to exactly %d past its own "+
		"limit; the compression's response to padding has changed and the loop no "+
		"longer converges", over)
	return nil, 0
}

// **The floor, which is the half of the ratio bound that lets things through.**
// [maxBundleRatioFloor] is the reason a bundle holding a small module is not
// refused for a figure that is really a property of tar: a tar pads every member
// to 512 bytes and ends with 1024 zero bytes, so a small bundle is mostly padding
// and compresses at hundreds of times while amounting to nothing at all.
//
// The guards below are the test: this bundle expands by more than
// [maxBundleRatio] and unpacks to less than the floor, so it is refused the
// moment the floor stops applying and accepted while it holds. Without them the
// test would keep passing against a bundle that had drifted under the ratio and
// would be asserting nothing.
//
// Asserted against [unbundle] rather than through an install, because a bundle
// small enough for the floor to govern cannot carry a module this host could
// compile — the smallest fixture in this repository is 1.8 MB, which is past the
// floor on its own — so an end-to-end version would be a test about the compiler.
func TestASmallBundleIsNotRefusedForExpandingSharply(t *testing.T) {
	code := make([]byte, 600<<10)
	m := manifestFor("padded", ClassDegrade, code)
	manifest := marshalManifest(t, m)
	inner := packTar(t, []member{{name: ManifestFile, body: manifest}, {name: m.Module, body: code}})
	bundle := gzipped(t, inner)

	if int64(len(inner)) > maxBundleRatioFloor {
		t.Fatalf("this bundle unpacks to %d bytes, which is past the %d-byte floor, so "+
			"the ratio applies to it and the floor is not what this test would be "+
			"about", len(inner), maxBundleRatioFloor)
	}
	if int64(len(inner))/int64(len(bundle)) <= maxBundleRatio {
		t.Fatalf("this bundle expands %d times, which the ratio permits anyway; the "+
			"floor is what has to be doing the work here",
			int64(len(inner))/int64(len(bundle)))
	}

	gotManifest, name, module, err := unbundle(bundle)
	if err != nil {
		t.Fatalf("a %d-byte bundle unpacking to %d bytes was refused: %v — below the "+
			"floor the ratio is measuring a tar's padding rather than an archive's "+
			"intent, and a mebibyte is not a bomb", len(bundle), len(inner), err)
	}
	if name != m.Module || !bytes.Equal(gotManifest, manifest) || !bytes.Equal(module, code) {
		t.Errorf("the bundle read back as %q with %d manifest bytes and %d module "+
			"bytes, want %q with %d and %d", name, len(gotManifest), len(module),
			m.Module, len(manifest), len(code))
	}
}

// A zip declares its members' uncompressed sizes in its central directory, so a
// bomb can be refused before a byte is inflated — and the declaration is never
// *trusted*, only used to say no early. The member read bounds what actually
// arrives, which is what a lying declaration meets.
//
// **The sentence is what shows the declaration is what refused it.** This archive
// is a kilobyte on the wire, so its bound is the ratio rather than the cap — the
// same figure the gzip path stops its read at, reached here without inflating
// anything — and both of this reader's refusals say *that zip says it unpacks
// to*, which no refusal reached by unpacking could honestly say.
func TestAZipThatDeclaresMoreThanItMayUnpackToIsRefused(t *testing.T) {
	bomb := zipRaw(t, ManifestFile, 1<<10, uint64(maxBundleInflated)+1)
	if int64(len(bomb)) > 1<<16 {
		t.Fatalf("the archive is %d bytes, and the point of the declaration is that "+
			"refusing it costs nothing", len(bomb))
	}
	h, _, _, _ := lifecycleHost(t)
	ts := serveBundle(t, bomb)
	reachable(t, h, ts)
	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/bomb.zip", SHA256: digestOf(bomb),
	})
	assertFieldCode(t, err, CodeBundleExpands)
	// The declaration's own voice, for the reason above: the code alone cannot show
	// that the cheap check exists, because every other refusal at this door carries
	// it too.
	if !strings.Contains(errorText(err), "says it unpacks to more than") {
		t.Errorf("the refusal is %q and does not say the zip's own declaration is "+
			"what refused it", errorText(err))
	}
}

// **A central directory's sizes are numbers an attacker chose, and they are added
// up.** Two entries declaring most of a `uint64` each sum to something small, and
// a wrapped total is a total that passes every comparison made after it — so the
// running sum is checked by subtracting from what is left rather than by adding
// and looking afterwards.
//
// The archive is empty of content: nothing here is ever read, which is the point
// of a check that lives in the central directory.
func TestAZipWhoseDeclaredSizesWouldOverflowTheirSumIsRefused(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for i, declared := range []uint64{1 << 10, ^uint64(0)} {
		if _, err := w.CreateRaw(&zip.FileHeader{
			Name: []string{ManifestFile, "module.wasm"}[i], Method: zip.Store,
			UncompressedSize64: declared,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	bomb := buf.Bytes()

	h, _, _, _ := lifecycleHost(t)
	ts := serveBundle(t, bomb)
	reachable(t, h, ts)
	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/overflow.zip", SHA256: digestOf(bomb),
	})
	assertFieldCode(t, err, CodeBundleExpands)
	if !strings.Contains(errorText(err), "says it unpacks to more than") {
		t.Errorf("the refusal is %q; a declaration that would overflow the sum is "+
			"refused as a declaration, at the central directory", errorText(err))
	}
}

// **And the same lie told the other way is refused too**, which is what makes the
// check above a bound rather than a hint: a member declaring a kilobyte and
// carrying more than the cap does not get to inflate past its own declaration.
//
// It is `archive/zip` that refuses this one — a read taking the decompressed
// count past `UncompressedSize64` is `ErrFormat` — so it arrives as *these bytes
// are not a zip* rather than as an expansion refusal, and that is the honest
// reading of what happened. The reason the test exists anyway is that the
// central-directory check would be worthless if the declaration were not
// enforced; this is the assertion that it is. The budget in
// [bundleMembers.read] sits underneath both and depends on neither.
func TestAZipMemberThatCarriesMoreThanItDeclaresIsRefused(t *testing.T) {
	bomb := zipRaw(t, ManifestFile, maxBundleInflated+(1<<20), 1<<10)
	if int64(len(bomb)) > MaxUploadBytes {
		t.Fatalf("the bomb is %d bytes on the wire and the fetch cap would refuse it "+
			"first, which would make this a test about the wrong bound", len(bomb))
	}
	h, dir, _, _ := lifecycleHost(t)
	ts := serveBundle(t, bomb)
	reachable(t, h, ts)
	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/liar.zip", SHA256: digestOf(bomb),
	})
	assertFieldCode(t, err, CodeBundleInvalid)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the add-ons directory holds %d entries after a lying zip", len(entries))
	}
}

// **Zip's own defect, and the one the other two formats cannot express.** A
// member stored with a compression method this product does not implement is
// refused by name rather than by a decompressor's error, because the alternative
// is registering one to satisfy an archive nobody has asked to publish.
func TestAZipMemberCompressedWithSomethingElseIsRefused(t *testing.T) {
	const madeUp = 99
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	w.RegisterCompressor(madeUp, func(out io.Writer) (io.WriteCloser, error) {
		return nopCloser{out}, nil
	})
	fw, err := w.CreateHeader(&zip.FileHeader{Name: ManifestFile, Method: madeUp})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(`{"schema_version":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	h, _, _, _ := lifecycleHost(t)
	ts := serveBundle(t, buf.Bytes())
	reachable(t, h, ts)
	_, err = h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/odd.zip", SHA256: digestOf(buf.Bytes()),
	})
	assertFieldCode(t, err, CodeBundleInvalid)
	if !strings.Contains(errorText(err), "stored or deflated") {
		t.Errorf("the refusal is %q and does not say which methods a bundle's entries "+
			"use", errorText(err))
	}
}

// A stored — uncompressed — member is a zip an ordinary tool writes, and it
// installs. The refusal above is about the third method, not about deflate being
// mandatory.
func TestAStoredZipMemberIsAcceptedLikeADeflatedOne(t *testing.T) {
	code := fixture(t, "minimal")
	m := manifestFor("arrival", ClassDegrade, code)
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range []member{
		{name: ManifestFile, body: marshalManifest(t, m)}, {name: m.Module, body: code},
	} {
		fw, err := w.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	h, _, _, _ := lifecycleHost(t)
	ts := serveBundle(t, buf.Bytes())
	reachable(t, h, ts)
	if _, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: ts.URL + "/stored.zip", SHA256: digestOf(buf.Bytes()),
	}); err != nil {
		t.Fatalf("a stored zip was refused: %v", err)
	}
}

// --- it is one more way in, never a replacement --------------------------------

// M67's refusals are M67's, whichever door the arrival came through: the name
// collision, the already-installed check and the migrations refusal are reached
// through [Host.install] and are not re-implemented here. One of each, because
// what is being asserted is that the fetch path *arrives at them*.
func TestAUrlInstallMeetsEveryRefusalAnUploadMeets(t *testing.T) {
	code := fixture(t, "minimal")
	m := manifestFor("arrival", ClassDegrade, code)
	bundle := tarBundle(t, map[string][]byte{
		ManifestFile: marshalManifest(t, m), m.Module: code,
	})

	h, _, _, _ := lifecycleHost(t)
	ts := serveBundle(t, bundle)
	reachable(t, h, ts)
	req := URLInstallRequest{URL: ts.URL + "/arrival.tar", SHA256: digestOf(bundle)}
	if _, err := h.installFromURL(t.Context(), nil, req); err != nil {
		t.Fatalf("installing from a URL: %v", err)
	}
	if _, err := h.installFromURL(t.Context(), nil, req); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("installing the same name twice from a URL answered %v, want a conflict; "+
			"this version has no upgrade-in-place whichever door is used", err)
	}

	// And a bundle carrying a manifest that declares migration files: refused by
	// checkUpload, with the code the surfaces word specially.
	mig := manifestFor("shipsddl", ClassDegrade, code)
	mig.Permissions = []string{"storage.own_schema"}
	mig.Migrations = []MigrationFile{{File: "0001_init.sql", SHA256: digest([]byte("x"))}}
	migBundle := tarBundle(t, map[string][]byte{
		ManifestFile: marshalManifest(t, mig), mig.Module: code,
	})
	migTS := serveBundle(t, migBundle)
	reachable(t, h, migTS)
	_, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: migTS.URL + "/ddl.tar", SHA256: digestOf(migBundle),
	})
	assertFieldCode(t, err, CodeMigrationsUnsupported)
}

// An instance with no add-ons directory answers the same sentinel both ways.
func TestAUrlInstallOnAnInstanceWithNoDirectory(t *testing.T) {
	var h *Host
	if _, err := h.installFromURL(t.Context(), nil, URLInstallRequest{
		URL: "https://example.com/x.tar", SHA256: strings.Repeat("a", 64),
	}); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("a URL install on an instance with no add-ons directory answered %v", err)
	}
}

// --- the vocabulary, from the end nothing held ---------------------------------

// **[URLInstallCodes] was closed in one direction only.**
//
// internal/httpx's TestEveryUrlInstallRefusalWordsWhichBoundBit proves every code
// in that list has a sentence of its own on the page. Nothing proved the reverse:
// that every outcome an install can arrive at is *in* the list. The fetch half of
// it is one entry per word [fetchFailure] and the stages around it can produce,
// hand-copied — so a word added to that switch would reach an operator as the
// page's generic "That did not work", with no failing build anywhere. The add-on
// fetch vocabulary is held from both ends by
// [TestEveryOutcomeTheHostProducesIsInTheVocabulary] and httpx's
// TestEveryFetchOutcomeHasAnOperatorsReading; this is the missing half of the
// same pair for the install door.
//
// It reads the words out of the source that produces them rather than listing
// them, because a list is the thing that went stale. The pattern is audit's
// TestAllActionsIsExhaustive, and the same objection answers it: anything
// enumerating this by hand is one word short the moment somebody adds a branch.
func TestEveryOutcomeAUrlInstallCanProduceHasACodeOfItsOwn(t *testing.T) {
	fset := token.NewFileSet()
	fetchFile := parseGoFile(t, fset, "fetch.go")
	installFile := parseGoFile(t, fset, "install_url.go")

	// The two functions that put a word on a response, and the one that turns a
	// wire error into one.
	fromWire := stringsReturnedBy(t, funcNamed(t, fetchFile, "", "fetchFailure"))
	fromPolicy := stringsReturnedBy(t, funcNamed(t, installFile, "operatorURL", "permits"))

	produced := map[string]bool{}
	ast.Inspect(funcNamed(t, fetchFile, "fetcher", "get"), func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Outcome" {
			return true
		}
		switch v := kv.Value.(type) {
		case *ast.BasicLit:
			produced[unquote(t, v)] = true
		case *ast.CallExpr:
			// `Outcome: fetchFailure(err)` — every word that classifier can answer.
			if id, ok := v.Fun.(*ast.Ident); !ok || id.Name != "fetchFailure" {
				t.Fatalf("%s: get answers with a call this test cannot follow",
					fset.Position(v.Pos()))
			}
			for _, w := range fromWire {
				produced[w] = true
			}
		case *ast.Ident:
			// `Outcome: outcome` — the origin policy's, and an install's policy is
			// [operatorURL] rather than the add-on's configured set.
			for _, w := range fromPolicy {
				produced[w] = true
			}
		case *ast.SelectorExpr:
			// `Outcome: abi.FetchOK`, and nothing else selected from another package
			// is a word this test would be able to read.
			if v.Sel.Name != "FetchOK" {
				t.Fatalf("%s: get answers with %s, which this test cannot follow",
					fset.Position(v.Pos()), v.Sel.Name)
			}
			produced[abi.FetchOK] = true
		default:
			t.Fatalf("%s: get answers with a %T this test cannot follow",
				fset.Position(kv.Pos()), kv.Value)
		}
		return true
	})
	if len(produced) < len(fromWire)+len(fromPolicy) {
		t.Fatalf("read %d outcomes out of fetch.go; this test is not reading what it "+
			"thinks it is", len(produced))
	}

	// The two words an install answers with something other than a `fetch_` code,
	// each because the install door says something better about it than the fetch
	// vocabulary can.
	answeredOtherwise := map[string]string{
		"invalid_request": "a URL this host will not make a request out of is " +
			CodeURLInvalid + ", which names the field the operator has to fix",
		abi.FetchOK: "not a refusal at all",
	}

	for word := range produced {
		code := "fetch_" + word
		if why, exempt := answeredOtherwise[word]; exempt {
			if slices.Contains(URLInstallCodes, code) {
				t.Errorf("%q is in URLInstallCodes and this test holds that %q is "+
					"%s; one of the two is wrong", code, word, why)
			}
			continue
		}
		if !slices.Contains(URLInstallCodes, code) {
			t.Errorf("a URL install can end in the outcome %q and URLInstallCodes has "+
				"no %q, so an operator meeting it reads the page's generic sentence "+
				"instead of what happened", word, code)
		}
	}
	// And the other half of the same closure, which makes the list exactly the
	// declared codes plus the reachable outcomes and nothing else. `fetch_status`
	// is why the declared ones are read out of the source rather than told apart by
	// their prefix: it begins with `fetch_` and is not an outcome at all.
	declared := declaredCodes(t, installFile)
	for name, code := range declared {
		if !slices.Contains(URLInstallCodes, code) {
			t.Errorf("%s is declared as %q and URLInstallCodes omits it, so the page has "+
				"no sentence held against it and an operator meeting it reads the "+
				"generic one", name, code)
		}
	}
	for _, code := range URLInstallCodes {
		if slices.Contains(slices.Collect(maps.Values(declared)), code) {
			continue
		}
		word, ok := strings.CutPrefix(code, "fetch_")
		if !ok || !produced[word] {
			t.Errorf("URLInstallCodes carries %q and nothing in this package produces "+
				"it; it is a sentence nobody will ever read", code)
		}
	}
}

// declaredCodes is every `Code…` constant install_url.go declares, by name, for
// the half of the closure that is about the codes this file raises itself.
func declaredCodes(t *testing.T, file *ast.File) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !strings.HasPrefix(name.Name, "Code") || !ok || lit.Kind != token.STRING {
					continue
				}
				out[name.Name] = unquote(t, lit)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("install_url.go declares no Code constants; this test is not reading " +
			"what it thinks it is")
	}
	return out
}

// parseGoFile parses one file of this package, for the tests that hold a list
// against the code that fills it rather than against another list.
func parseGoFile(t *testing.T, fset *token.FileSet, name string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, name, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// funcNamed is the declaration of `recv.name`, or of `name` when recv is empty.
// Absent is fatal: a test that quietly read nothing is a test that passes.
func funcNamed(t *testing.T, file *ast.File, recv, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name || (fd.Recv == nil) != (recv == "") {
			continue
		}
		if recv == "" || receiverName(fd) == recv {
			return fd
		}
	}
	t.Fatalf("this package no longer declares %s.%s; if it moved, move the anchor, "+
		"because it is where the install vocabulary's words come from", recv, name)
	return nil
}

// receiverName is a method's receiver type, pointer or not.
func receiverName(fd *ast.FuncDecl) string {
	expr := fd.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// stringsReturnedBy is every string literal the function returns, which for the
// two functions above is exactly the vocabulary each of them can answer with.
func stringsReturnedBy(t *testing.T, fd *ast.FuncDecl) []string {
	t.Helper()
	var out []string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, r := range ret.Results {
			if lit, ok := r.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				out = append(out, unquote(t, lit))
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("%s returns no string literal; this test is reading the wrong "+
			"function", fd.Name.Name)
	}
	return out
}

func unquote(t *testing.T, lit *ast.BasicLit) string {
	t.Helper()
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatalf("%s: %v", lit.Value, err)
	}
	return s
}

// --- helpers -------------------------------------------------------------------

// --- packing a bundle, in each of the three containers -------------------------

// memberKind is what an archive entry is, in the vocabulary all three containers
// have in common.
type memberKind int

const (
	memFile memberKind = iota
	memDir
	memSymlink
)

// member is one entry a test asks a packer to write — including the entries an
// add-on bundle may not contain, because a bundle built by something that thought
// it was shipping a tree is exactly what the member rule exists to refuse.
type member struct {
	name string
	body []byte
	kind memberKind
}

// bundleFormats is the three containers [D384] accepts, each as a packer over the
// same members.
//
// **This slice is what lets the member rule be asserted as one rule.** A table of
// hostile shapes is written once and run against all three, so a policy that held
// in the tar reader and not in the zip reader fails a test rather than shipping —
// which is the drift D384 names as the thing to prevent, and the reason accepting
// three formats is affordable at all.
var bundleFormats = []struct {
	name string
	pack func(*testing.T, []member) []byte
}{
	{"tar", packTar},
	{"tar.gz", packTarGz},
	{"zip", packZip},
}

// packTar writes the members into an uncompressed tar, in the order given — order
// matters, because a duplicate name is one of the shapes being asserted.
func packTar(t *testing.T, members []member) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range members {
		h := &tar.Header{
			Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg,
		}
		switch e.kind {
		case memFile:
		case memDir:
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		case memSymlink:
			h.Typeflag, h.Mode, h.Size = tar.TypeSymlink, 0o777, 0
			h.Linkname = "/etc/passwd"
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := w.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// packTarGz is the container this project ships its own releases as, and the one
// D381 declined before the owner overruled it.
func packTarGz(t *testing.T, members []member) []byte {
	t.Helper()
	return gzipped(t, packTar(t, members))
}

// packZip writes the same members as a zip, expressing a directory and a symlink
// the way a zip does — in the external attributes rather than in a type byte,
// which is exactly why the two readers could have drifted.
func packZip(t *testing.T, members []member) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range members {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		body := e.body
		switch e.kind {
		case memDir:
			h.Name += "/"
			h.Method = zip.Store
			h.SetMode(fs.ModeDir | 0o755)
			body = nil
		case memSymlink:
			h.Method = zip.Store
			h.SetMode(fs.ModeSymlink | 0o777)
			body = []byte("/etc/passwd")
		default:
			h.SetMode(0o644)
		}
		fw, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tarBundle is the ordinary two-member bundle as an uncompressed tar: what the
// tests that are about something *other* than the container want, sorted so the
// bytes a test hashes are the same bytes every run.
func tarBundle(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	slices.Sort(names)
	entries := make([]member, 0, len(names))
	for _, name := range names {
		entries = append(entries, member{name: name, body: members[name]})
	}
	return packTar(t, entries)
}

// gzipped is the compression layer on its own, so a test can build a bomb without
// building a bundle.
func gzipped(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zipRaw builds a zip whose central directory says one thing and whose content is
// another, which is the shape no ordinary writer produces and every reader has to
// survive.
//
// `zip.Writer.CreateRaw` is what makes it possible: the sizes and the checksum are
// taken from the header rather than measured, so a declaration can be a lie in
// either direction. Both directions matter — a declaration far larger than the
// cap is what lets a bomb be refused at its central directory, and a declaration
// far smaller than the content is what proves the declaration is never *trusted*.
func zipRaw(t *testing.T, name string, actual int64, declared uint64) []byte {
	t.Helper()
	var payload bytes.Buffer
	crc := crc32.NewIEEE()
	fw, err := flate.NewWriter(&payload, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.MultiWriter(fw, crc), zeroes{}, actual); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	out, err := w.CreateRaw(&zip.FileHeader{
		Name: name, Method: zip.Deflate,
		CompressedSize64:   uint64(payload.Len()),
		UncompressedSize64: declared,
		CRC32:              crc.Sum32(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write(payload.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zeroes is an endless run of the most compressible byte there is.
type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// nopCloser lets a test register a compressor for a method nothing implements,
// which is how an archive carrying one gets built at all.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// errorText is a refusal's own sentence, for the two assertions that are about
// the sentence rather than about the code.
func errorText(err error) string {
	var ve domain.ValidationErrors
	if errors.As(err, &ve) && len(ve) > 0 {
		return ve[0].Message
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// marshalManifest writes a manifest the way a publisher's `addon.json` is
// actually written: indented, with a trailing newline.
//
// **Not `json.Marshal`, and the difference is what makes the byte-for-byte
// comparison mean anything.** A compact encoding of a parsed manifest is
// byte-identical to what `json.Marshal` produced in the first place, so a fetch
// path that re-serialized the manifest instead of writing the fetched bytes would
// pass a comparison built on one — which is what happened when this test was
// sabotaged, and is why the fixture is written this way instead.
func marshalManifest(t *testing.T, m Manifest) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// serveBundle answers every path with the same bytes, over TLS, because the
// scheme is not negotiable.
func serveBundle(t *testing.T, bundle []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(bundle)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// reachable teaches this host's install fetcher to trust the test server's
// authority and to dial loopback.
//
// The relaxation is on `allowAddr` and nowhere else, which is
// [newTestFetcher]'s arrangement and it exists for the same reason: a suite that
// could only exercise this path by relaxing the *policy* would never exercise the
// wiring. So one test above leaves this alone and watches the dial be refused.
func reachable(t *testing.T, h *Host, ts *httptest.Server) {
	t.Helper()
	patchTLS(t, h.installFetcher, ts)
	h.installFetcher.allowAddr = func(ip netip.Addr) error {
		if ip.Unmap().IsLoopback() {
			return nil
		}
		return refuseAddress(ip)
	}
}

// assertFieldCode is the whole of what a surface branches on: the code, never the
// message.
func assertFieldCode(t *testing.T, err error, want string) {
	t.Helper()
	var ve domain.ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("the refusal is %v, want validation errors carrying %q", err, want)
	}
	for _, fe := range ve {
		if fe.Code == want {
			return
		}
	}
	t.Errorf("the refusal carries %v, want a field error coded %q — a URL install "+
		"says which bound bit, not that the upload was refused", ve, want)
}

// TestTheInstallFetchTimeoutMirrorIsTheRealOne ties internal/config's mirrored
// copy to the constant it mirrors.
//
// The mirror exists because internal/config cannot import this package — the
// dependency runs the other way — and config.Validate has to refuse an
// HTTP_REQUEST_TIMEOUT that this bound cannot nest inside (F358). Two numbers,
// one meaning; this is what stops them parting company, and it lives here
// because this is the package that can see both.
func TestTheInstallFetchTimeoutMirrorIsTheRealOne(t *testing.T) {
	if got := config.InstallFetchTimeoutMirror(); got != InstallFetchTimeout {
		t.Fatalf("internal/config mirrors the install fetch timeout as %s and it is %s. "+
			"config.Validate nests HTTP_REQUEST_TIMEOUT against its copy, so a mirror "+
			"that has drifted refuses the wrong configurations and admits the wrong "+
			"ones (F358)", got, InstallFetchTimeout)
	}
}
