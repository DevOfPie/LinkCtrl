package qr

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"runtime"
	"testing"
)

// The first file this product accepts (M50.5), tested for the things that are
// new here rather than for the things `image/png` already guarantees.
//
// Four claims, and each is a bullet of the milestone: the caps bind before the
// allocation they bound, the decoder is chosen by the bytes, an SVG is refused,
// and what is stored is bytes this product produced. The fifth — that the stored
// artefact fits the number D134 is owed — is the one that makes the arithmetic
// in logo.go a measurement rather than a claim.

// --- fixtures ---------------------------------------------------------------

// solidPNG is an ordinary upload: a small opaque raster somebody would actually
// have.
func solidPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func solidJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: 0x20, B: uint8(y), A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// chunk frames one PNG chunk the way the format does: length, type, payload,
// CRC over the type and the payload.
func chunk(kind string, payload []byte) []byte {
	out := make([]byte, 0, 12+len(payload))
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)))
	out = append(out, kind...)
	out = append(out, payload...)
	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte(kind))
	_, _ = crc.Write(payload)
	return binary.BigEndian.AppendUint32(out, crc.Sum32())
}

// declaredPNG builds a PNG that *says* it is w×h and carries almost no bytes.
//
// This is the decompression bomb the milestone's ordering exists for: a few
// hundred bytes on the wire, a quarter of a gigabyte if anything decodes it.
// The IDAT payload is deliberate rubbish — the point is reached before it is
// read, and if it ever is not, `image/png` has already allocated the buffer.
func declaredPNG(w, h int) []byte {
	ihdr := make([]byte, 0, 13)
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // 8-bit, truecolour with alpha

	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, chunk("IHDR", ihdr)...)
	out = append(out, chunk("IDAT", []byte{0x78, 0x9c, 0x00})...)
	return append(out, chunk("IEND", nil)...)
}

// --- the ordering -----------------------------------------------------------

// TestABombIsRefusedBeforeAnythingIsDecoded is the milestone's most important
// claim, and the assertion is about memory rather than about the error.
//
// An error alone would be satisfied by a cap checked *after* `png.Decode`
// returned — which is the ordering the earlier draft of m50.5.md had, and which
// is no protection at all: `image/png` allocates the full pixel buffer from the
// header before it reads a byte of image data. So the test measures what the
// refusal cost. 8000×8000 at four bytes a pixel is 256,000,000 bytes; the
// allowance below is 8 MiB, three orders of magnitude under it and far above
// the few hundred bytes reading a header can want.
func TestABombIsRefusedBeforeAnythingIsDecoded(t *testing.T) {
	const side = 8000
	bomb := declaredPNG(side, side)
	if len(bomb) > MaxLogoUploadBytes {
		t.Fatalf("the bomb is %d bytes and would be refused by the body cap first; "+
			"this test is meant to pass step 1 and fail step 2", len(bomb))
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := NormalizeLogo(bomb)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrLogoTooLarge) {
		t.Fatalf("a %[1]dx%[1]d declaration returned %v, want ErrLogoTooLarge", side, err)
	}
	const allowance = 8 << 20
	if spent := after.TotalAlloc - before.TotalAlloc; spent > allowance {
		t.Errorf("refusing a %[1]dx%[1]d bomb allocated %d bytes; a header check wants a "+
			"few hundred, and the pixel buffer it must not have made is %d",
			side, spent, side*side*4)
	}
}

// TestBothCapsRefuseSomethingTheOtherAdmits holds the pair to the reason
// logo.go gives for there being two of them. A cap that refuses nothing the
// other does not is a cap that could be deleted, and this is what says neither
// can be.
func TestBothCapsRefuseSomethingTheOtherAdmits(t *testing.T) {
	strip := declaredPNG(MaxLogoDimension*2, 1)
	if w, h := MaxLogoDimension*2, 1; w*h > MaxLogoPixels {
		t.Fatalf("the strip is %dx%d and already fails the pixel cap; it is supposed to "+
			"pass it and fail the side cap", w, h)
	}
	if _, err := NormalizeLogo(strip); !errors.Is(err, ErrLogoTooLarge) {
		t.Errorf("a %dx1 strip returned %v; the side cap is what refuses it",
			MaxLogoDimension*2, err)
	}

	square := declaredPNG(MaxLogoDimension, MaxLogoDimension)
	if MaxLogoDimension*MaxLogoDimension <= MaxLogoPixels {
		t.Fatalf("a %[1]dx%[1]d image is inside the pixel cap; it is supposed to pass the "+
			"side cap and fail the area one", MaxLogoDimension)
	}
	if _, err := NormalizeLogo(square); !errors.Is(err, ErrLogoTooLarge) {
		t.Errorf("a %[1]dx%[1]d image returned %v; the pixel cap is what refuses it",
			MaxLogoDimension, err)
	}
}

// TestTheCapsAdmitWhatTheyAreSizedFor is the other side of the pair: an image at
// the area cap and inside the side cap is accepted, so the caps are not simply
// refusing everything.
func TestTheCapsAdmitWhatTheyAreSizedFor(t *testing.T) {
	for _, size := range []struct{ w, h int }{
		{512, 512}, {MaxLogoDimension, MaxLogoPixels / MaxLogoDimension}, {64, 64}, {1, 1},
	} {
		if _, err := NormalizeLogo(solidPNG(t, size.w, size.h)); err != nil {
			t.Errorf("%dx%d was refused: %v", size.w, size.h, err)
		}
	}
}

// --- the format ---------------------------------------------------------------

// TestTheDecoderIsChosenByTheBytes checks the property from the only side this
// package can see it from: nothing but the content reaches here at all, so a
// JPEG is decoded as a JPEG whatever anyone calls it, and a file wearing a PNG
// signature over JPEG data is refused rather than guessed at.
//
// The other half — that the handler reads neither the filename nor the declared
// type — is asserted in internal/httpx, where those two things exist.
func TestTheDecoderIsChosenByTheBytes(t *testing.T) {
	jpg := solidJPEG(t, 32, 32)
	out, err := NormalizeLogo(jpg)
	if err != nil {
		t.Fatalf("a JPEG carrying no declaration at all was refused: %v", err)
	}
	if !bytes.HasPrefix(out.PNG, pngMagic) {
		t.Error("a JPEG upload did not come back as a PNG")
	}

	lying := append(append([]byte{}, pngMagic...), jpg[len(pngMagic):]...)
	if _, err := NormalizeLogo(lying); !errors.Is(err, ErrLogoUndecodable) {
		t.Errorf("a JPEG wearing a PNG signature returned %v; sniffing picks the decoder "+
			"and the decoder then has to agree", err)
	}
}

// TestAnSVGUploadIsRefusedByName pins the refusal the milestone singles out.
//
// By name rather than as an unrecognised format, because an SVG is the upload
// somebody will actually attempt and "unrecognised" would read as a bug in this
// product rather than as a decision about documents that carry script.
func TestAnSVGUploadIsRefusedByName(t *testing.T) {
	for name, body := range map[string]string{
		"bare":            `<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`,
		"declared":        `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`,
		"leading space":   "\n  <svg xmlns=\"http://www.w3.org/2000/svg\"/>",
		"byte order mark": "\xef\xbb\xbf<svg/>",
		"uppercase":       `<SVG xmlns="http://www.w3.org/2000/svg"/>`,
		"with script":     `<svg onload="alert(1)"><script>x()</script></svg>`,
	} {
		if _, err := NormalizeLogo([]byte(body)); !errors.Is(err, ErrLogoSVG) {
			t.Errorf("%s SVG returned %v, want ErrLogoSVG", name, err)
		}
	}

	// Anything else this product does not decode is still refused, just under
	// the general answer.
	for name, body := range map[string]string{
		"gif":   "GIF89a\x01\x00\x01\x00\x00\x00\x00;",
		"webp":  "RIFF\x00\x00\x00\x00WEBPVP8 ",
		"bmp":   "BM\x00\x00\x00\x00",
		"elf":   "\x7fELF\x02\x01\x01",
		"plain": "just some text",
	} {
		if _, err := NormalizeLogo([]byte(body)); !errors.Is(err, ErrLogoFormat) {
			t.Errorf("%s returned %v, want ErrLogoFormat", name, err)
		}
	}

	if _, err := NormalizeLogo(nil); !errors.Is(err, ErrLogoEmpty) {
		t.Error("an empty upload was not refused as empty")
	}
}

// --- what is stored -----------------------------------------------------------

// TestWhatIsStoredIsBytesThisProductProduced is the polyglot and metadata claim,
// checked by putting something recognisable in the upload and looking for it in
// the output.
//
// A PNG text chunk stands in for every kind of passenger a real upload carries —
// EXIF, an ICC profile, XMP, a JPEG comment, or the second file a polyglot is
// hiding. None of them survives being decoded to pixels and encoded again, and
// that is the whole of why re-encoding is cheaper than sanitising.
func TestWhatIsStoredIsBytesThisProductProduced(t *testing.T) {
	const passenger = "PAYLOAD-THAT-MUST-NOT-SURVIVE"

	// A PNG with a tEXt chunk spliced in ahead of the image data, which is where
	// the format allows one and where an exporter would put it.
	base := solidPNG(t, 48, 48)
	at := bytes.Index(base, []byte("IDAT"))
	if at < 4 {
		t.Fatal("could not find the IDAT chunk to splice ahead of")
	}
	text := chunk("tEXt", append([]byte("Comment\x00"), passenger...))
	upload := append(append(append([]byte{}, base[:at-4]...), text...), base[at-4:]...)
	if !bytes.Contains(upload, []byte(passenger)) {
		t.Fatal("the fixture does not contain the passenger; the splice is wrong")
	}

	out, err := NormalizeLogo(upload)
	if err != nil {
		t.Fatalf("a PNG with a text chunk was refused: %v", err)
	}
	if bytes.Contains(out.PNG, []byte(passenger)) {
		t.Error("the stored bytes still carry the upload's text chunk; what is stored is " +
			"supposed to be bytes this product encoded, not the ones it received")
	}
	if bytes.Equal(out.PNG, upload) {
		t.Error("the stored bytes are the received bytes")
	}
	if out.Width != 48 || out.Height != 48 {
		t.Errorf("stored logo is %dx%d, want 48x48", out.Width, out.Height)
	}

	// The pixels are the part that must survive, or re-encoding would be
	// destroying the thing it is protecting.
	stored, err := png.Decode(bytes.NewReader(out.PNG))
	if err != nil {
		t.Fatalf("the stored bytes are not a decodable PNG: %v", err)
	}
	original, err := png.Decode(bytes.NewReader(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []image.Point{{X: 0, Y: 0}, {X: 17, Y: 31}, {X: 47, Y: 47}} {
		if got, want := stored.At(at.X, at.Y), original.At(at.X, at.Y); got != want {
			t.Errorf("pixel %v is %v after re-encoding, was %v", at, got, want)
		}
	}
}

// TestTheWorstCaseLogoFitsTheStatedBound is what makes logo.go's arithmetic a
// measurement, which is what D134 asked this milestone for.
//
// **The fixture is a paletted PNG on purpose, and that is the finding worth
// keeping.** The stored bound is not implied by the upload cap: incompressible
// pixels are incompressible on the way in too, so a truecolour upload near the
// worst case would not fit through [MaxLogoUploadBytes] in the first place. One
// byte of palette index per pixel does fit — about 263 KB on the wire — and
// expands to four bytes a pixel once it is normalized. So the worst stored row
// is reachable, and it is reachable through a file a quarter of its size.
//
// The shape is the tallest the caps allow at the area cap, because PNG spends a
// filter byte per scanline and logo.go's derivation says so.
func TestTheWorstCaseLogoFitsTheStatedBound(t *testing.T) {
	const (
		width  = MaxLogoDimension
		height = MaxLogoPixels / MaxLogoDimension
	)
	// A deterministic seed, so a failure is reproducible rather than a story
	// about one unlucky run.
	rng := rand.New(rand.NewPCG(0x10905, 0x5010))

	palette := make(color.Palette, 256)
	for i := range palette {
		palette[i] = color.NRGBA{
			R: uint8(rng.UintN(256)), G: uint8(rng.UintN(256)),
			B: uint8(rng.UintN(256)),
			// Deliberately never opaque: an all-opaque image lets the encoder
			// drop the alpha channel and write three bytes a pixel, which would
			// measure a cheaper picture than the bound is written for.
			A: uint8(rng.UintN(255)),
		}
	}
	img := image.NewPaletted(image.Rect(0, 0, width, height), palette)
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.UintN(256))
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	upload := buf.Bytes()
	if len(upload) > MaxLogoUploadBytes {
		t.Fatalf("the fixture is %d bytes and the body cap is %d; the worst stored case "+
			"has to be reachable through the upload cap or it is not the worst case",
			len(upload), MaxLogoUploadBytes)
	}

	out, err := NormalizeLogo(upload)
	if err != nil {
		t.Fatalf("the worst case the caps admit was refused: %v", err)
	}
	if len(out.PNG) > MaxLogoStoredBytes {
		t.Errorf("the worst case stores %d bytes and MaxLogoStoredBytes is %d; the number "+
			"D134 is owed is wrong", len(out.PNG), MaxLogoStoredBytes)
	}
	// And the bound is not passing vacuously: logo.go derives 1,050,132, so a
	// figure far below it would mean the fixture stopped being the worst case
	// and the constant stopped being measured by anything.
	if len(out.PNG) < 1_000_000 {
		t.Errorf("the worst case stored only %d bytes; the fixture is no longer "+
			"incompressible and this test has stopped measuring the bound", len(out.PNG))
	}
	if len(out.PNG) <= len(upload) {
		t.Errorf("the stored form (%d) is no larger than the upload (%d); the point of "+
			"this fixture is that the body cap does not bound the row", len(out.PNG), len(upload))
	}
}

// TestTheCapsAgreeWithEachOther holds the four constants to the relationships
// logo.go's derivation assumes between them. Each one is an arithmetic claim
// that would go quietly wrong if somebody tuned a single number.
func TestTheCapsAgreeWithEachOther(t *testing.T) {
	// The stored bound has to sit above the buffer it is derived from, or
	// ErrLogoStoreTooLarge would fire for ordinary uploads rather than for the
	// case it exists to catch.
	if MaxLogoStoredBytes <= MaxLogoPixels*4 {
		t.Errorf("MaxLogoStoredBytes (%d) is not above the decoded buffer it bounds (%d)",
			MaxLogoStoredBytes, MaxLogoPixels*4)
	}
	// The derivation in logo.go is 1,050,132 for the tallest image the area cap
	// admits. If the caps move, that line moves with them.
	tallest := MaxLogoDimension * (1 + 4*(MaxLogoPixels/MaxLogoDimension))
	if want := 1_049_600; tallest != want {
		t.Errorf("the worst filtered stream is now %d bytes, and logo.go's table says %d; "+
			"the derivation is stale", tallest, want)
	}
	// And the area cap has to be the binding one at the square, or the two caps
	// stop being the pair the comment describes.
	if MaxLogoDimension*MaxLogoDimension <= MaxLogoPixels {
		t.Error("the area cap admits a square at the side cap; the two caps no longer " +
			"refuse different things")
	}
}
