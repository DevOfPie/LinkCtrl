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

// widestUpload is the most expensive file the caps admit: the largest square the
// side bound allows, at the bit depth `image/png` decodes to eight bytes a pixel
// rather than four.
//
// Flat colour, so it is a few kilobytes on the wire — which is the point. The
// worst decode this product can be made to perform does not need a large upload,
// and a figure derived from the body cap would miss it entirely.
func widestUpload(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewNRGBA64(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = 0xcc
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pixelBytes is the buffer a decoded image actually holds, read off the concrete
// type rather than computed from its bounds.
//
// That distinction is the whole reason this helper exists: a figure this test
// multiplied out itself would agree with logo.go's multiplication whether either
// was right or not, which is exactly how a decode bound half the real one
// survived a test written to hold it.
func pixelBytes(t *testing.T, m image.Image) int {
	t.Helper()
	switch v := m.(type) {
	case *image.NRGBA64:
		return len(v.Pix)
	case *image.RGBA64:
		return len(v.Pix)
	case *image.NRGBA:
		return len(v.Pix)
	case *image.RGBA:
		return len(v.Pix)
	case *image.Gray16:
		return len(v.Pix)
	case *image.Gray:
		return len(v.Pix)
	case *image.Paletted:
		return len(v.Pix)
	case *image.CMYK:
		return len(v.Pix)
	case *image.YCbCr:
		return len(v.Y) + len(v.Cb) + len(v.Cr)
	default:
		t.Fatalf("a decoder returned %T, which this test cannot measure; a new "+
			"concrete image type means MaxDecodedLogoBytesPerPixel has to be "+
			"re-derived, not this switch extended", m)
		return 0
	}
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

// TestOnlyTheSideBoundRefuses is the F214 reopening's central claim, and it is
// the negative half: the area figure stopped turning uploads away.
//
// The shipped milestone had two header bounds and an 813×813 upload passed one
// and failed the other, which is a refusal with no verdict in it. Now the side
// is the only thing a header is refused for, and everything the side admits is
// decoded — so a square at the side bound, which used to be the area bound's own
// example, is accepted.
func TestOnlyTheSideBoundRefuses(t *testing.T) {
	strip := declaredPNG(MaxLogoDimension*2, 1)
	if _, err := NormalizeLogo(strip); !errors.Is(err, ErrLogoTooLarge) {
		t.Errorf("a %dx1 strip returned %v; the side bound is what refuses it",
			MaxLogoDimension*2, err)
	}

	// Past the area figure by four times over, and inside the side bound.
	square := solidPNG(t, MaxLogoDimension, MaxLogoDimension)
	if MaxLogoDimension*MaxLogoDimension <= MaxLogoPixels {
		t.Fatalf("a %[1]dx%[1]d image is inside the storage target; this fixture is "+
			"supposed to be the one that has to be resized", MaxLogoDimension)
	}
	out, err := NormalizeLogo(square)
	if err != nil {
		t.Fatalf("a %[1]dx%[1]d image was refused: %v. Since F214 an image inside the "+
			"side bound is resized to fit rather than turned away", MaxLogoDimension, err)
	}
	if !out.Resampled() {
		t.Errorf("a %[1]dx%[1]d image stored at %dx%d and reported no resample",
			MaxLogoDimension, out.Width, out.Height)
	}
	if out.Width*out.Height > MaxLogoPixels {
		t.Errorf("it stored %dx%d, which is %d pixels; the storage target is %d",
			out.Width, out.Height, out.Width*out.Height, MaxLogoPixels)
	}
}

// TestTheOwnersUploadIsStoredRatherThanRefused is F214(a) as the owner met it.
//
// 813×813 is 660,969 pixels — inside the 1024-a-side bound and four times past
// what a row holds — and it is here as a literal because it is the measurement
// that was reported, not a number derived from the constants. What it must
// produce is a stored image, at the largest square the target admits.
func TestTheOwnersUploadIsStoredRatherThanRefused(t *testing.T) {
	out, err := NormalizeLogo(solidPNG(t, 813, 813))
	if err != nil {
		t.Fatalf("813x813 was refused: %v", err)
	}
	if out.SourceWidth != 813 || out.SourceHeight != 813 {
		t.Errorf("it reported the upload as %dx%d, and it was 813x813; the warning is "+
			"built from that pair", out.SourceWidth, out.SourceHeight)
	}
	if out.Width != 512 || out.Height != 512 {
		t.Errorf("813x813 stored at %dx%d; 512x512 is exactly %d pixels, which is the "+
			"target, and rounding down to 511 would give away a pixel for nothing",
			out.Width, out.Height, MaxLogoPixels)
	}
	// And the bytes really are that size, rather than the struct saying so.
	cfg, err := DecodeLogoConfig(out.PNG)
	if err != nil {
		t.Fatalf("the stored PNG does not decode: %v", err)
	}
	if cfg.Width != out.Width || cfg.Height != out.Height {
		t.Errorf("the stored PNG is %dx%d and the Logo says %dx%d",
			cfg.Width, cfg.Height, out.Width, out.Height)
	}
}

// TestAnImageInsideTheTargetIsNotTouched is the other side of it. Resampling a
// picture that already fits would throw away detail for nothing, and Resampled
// would then be true for every upload, which is the same as being true for none.
func TestAnImageInsideTheTargetIsNotTouched(t *testing.T) {
	out, err := NormalizeLogo(solidPNG(t, 300, 200))
	if err != nil {
		t.Fatalf("300x200 was refused: %v", err)
	}
	if out.Resampled() || out.Width != 300 || out.Height != 200 {
		t.Errorf("300x200 was stored as %dx%d (resampled=%v); it fits both bounds",
			out.Width, out.Height, out.Resampled())
	}
}

// TestFitStoredLogoKeepsTheShape holds the arithmetic to what it claims: inside
// both bounds after the fit, never larger than the source, and the aspect ratio
// preserved to within the pixel the integer rounding can cost.
func TestFitStoredLogoKeepsTheShape(t *testing.T) {
	for _, in := range []struct{ w, h int }{
		{813, 813}, {1024, 1024}, {1024, 512}, {900, 300}, {300, 900},
		{1, 1}, {1024, 1}, {512, 512}, {1024, 256}, {1023, 1023},
	} {
		w, h := FitStoredLogo(in.w, in.h)
		switch {
		case w < 1 || h < 1:
			t.Errorf("%dx%d fitted to %dx%d, which is not an image", in.w, in.h, w, h)
		case w > in.w || h > in.h:
			t.Errorf("%dx%d fitted to the larger %dx%d; fitting never enlarges",
				in.w, in.h, w, h)
		case w*h > MaxLogoPixels || w > MaxLogoDimension || h > MaxLogoDimension:
			t.Errorf("%dx%d fitted to %dx%d, which is %d pixels and past a bound",
				in.w, in.h, w, h, w*h)
		}
		// The ratio, compared as a cross product so there is no float in the
		// assertion. One pixel of slack on each side is what rounding to nearest
		// and then walking a side back can cost.
		if d := in.w*h - in.h*w; d > (in.h+in.w) || d < -(in.h+in.w) {
			t.Errorf("%dx%d fitted to %dx%d, which is a different shape", in.w, in.h, w, h)
		}
	}
}

// TestARefusalNamesTheMeasurementAndOneBound is F214(a)'s other half, and it is
// about the sentence rather than about the error.
//
// The shipped message named two caps and no measurement, so the reader could not
// tell which one bit or by how much. What replaces it has to carry the file's own
// dimensions and exactly one limit — naming both again would be the same defect
// in different words.
func TestARefusalNamesTheMeasurementAndOneBound(t *testing.T) {
	_, err := NormalizeLogo(declaredPNG(2000, 300))
	var bound *LogoBoundError
	if !errors.As(err, &bound) {
		t.Fatalf("a 2000x300 upload returned %v, want a *LogoBoundError", err)
	}
	if bound.Width != 2000 || bound.Height != 300 {
		t.Errorf("the refusal measured %dx%d, and the file is 2000x300",
			bound.Width, bound.Height)
	}
	if bound.Bound != "side" || bound.Limit != MaxLogoDimension {
		t.Errorf("the refusal blames %q at %d; the side bound at %d is what 2000 crosses",
			bound.Bound, bound.Limit, MaxLogoDimension)
	}
	msg := bound.Error()
	for _, want := range []string{"2000", "300", "wide"} {
		if !bytes.Contains([]byte(msg), []byte(want)) {
			t.Errorf("the sentence %q does not carry %q", msg, want)
		}
	}
	// And it does not restate the target as though it were a second refusal.
	if bytes.Contains([]byte(msg), []byte("262144")) ||
		bytes.Contains([]byte(msg), []byte("262,144")) {
		t.Errorf("the sentence %q names the storage target, which refuses nothing; "+
			"that is the two-caps-no-verdict shape F214 was raised about", msg)
	}
	// The sentinel still answers, or every caller that switched on it breaks.
	if !errors.Is(err, ErrLogoTooLarge) {
		t.Error("a LogoBoundError no longer answers errors.Is for ErrLogoTooLarge")
	}
}

// TestTheCapsAdmitWhatTheyAreSizedFor is the other side of the pair: an image at
// the storage target and inside the side bound is accepted, so the bounds are
// not simply refusing everything.
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
		t.Errorf("MaxLogoStoredBytes (%d) is not above the stored buffer it bounds (%d)",
			MaxLogoStoredBytes, MaxLogoPixels*4)
	}
	// The derivation in logo.go is 1,050,132 for the tallest image the area cap
	// admits. If the caps move, that line moves with them.
	tallest := MaxLogoDimension * (1 + 4*(MaxLogoPixels/MaxLogoDimension))
	if want := 1_049_600; tallest != want {
		t.Errorf("the worst filtered stream is now %d bytes, and logo.go's table says %d; "+
			"the derivation is stale", tallest, want)
	}
	// The decode bound is derived from the side bound rather than declared
	// beside it, which is what makes "one refusal, one number" true. A second
	// header cap reintroduced anywhere would show up as this drifting.
	if MaxDecodedLogoPixels != MaxLogoDimension*MaxLogoDimension {
		t.Errorf("MaxDecodedLogoPixels is %d and the side bound implies %d; the decode "+
			"bound has stopped being what the side bound admits",
			MaxDecodedLogoPixels, MaxLogoDimension*MaxLogoDimension)
	}
	// And logo.go's image-buffer table is the sum of four terms it names —
	// summed here out of buffers the standard library allocated and a size
	// FitStoredLogo chose, rather than out of this test repeating logo.go's
	// arithmetic back at it. The upload is first because NormalizeLogo holds it
	// live across the decode, and a table that omitted it was understating the
	// peak by a megabyte. The decode is measured against a real one by
	// TestTheDecodeBoundIsMeasuredNotAssumed.
	//
	// The encoder's own working state is deliberately not here: logo.go states it
	// beside the table as a bound rather than inside it, because both of its terms
	// are properties of a Go release and pinning them would break on a release
	// that changed neither this package nor its caps.
	storedW, storedH := FitStoredLogo(MaxLogoDimension, MaxLogoDimension)
	peak := MaxLogoUploadBytes +
		MaxDecodedLogoBytes +
		len(image.NewNRGBA(image.Rect(0, 0, MaxLogoDimension, MaxLogoDimension)).Pix) +
		len(image.NewNRGBA(image.Rect(0, 0, storedW, storedH)).Pix)
	if want := 14_680_064; peak != want {
		t.Errorf("the image buffers an upload holds are now %d bytes and logo.go's "+
			"table says %d; the derivation is stale", peak, want)
	}
	// The storage target has to sit below the decode bound, or nothing is ever
	// resized and the whole reopening is dead code.
	if MaxLogoPixels >= MaxDecodedLogoPixels {
		t.Error("the storage target is not below the decode bound; no upload can " +
			"reach the resize path")
	}
}

// TestTheDecodeBoundIsMeasuredNotAssumed is the reason MaxDecodedLogoBytes is a
// number anybody should believe.
//
// The figure this file carried until now was MaxDecodedLogoPixels × 4, and it
// was wrong by exactly half: `image/png` hands back eight bytes a pixel for a
// bit-depth-16 file. Four is what this package *normalizes* to. The check that
// was meant to hold the figure re-derived the same multiplication, so it agreed
// with logo.go and with nothing else — a test that reproduces the code's
// arithmetic is not a check on the code's arithmetic.
//
// So this one measures. It puts the widest file the caps admit through the same
// three steps a request does and reads the buffer off what came back.
func TestTheDecodeBoundIsMeasuredNotAssumed(t *testing.T) {
	upload := widestUpload(t, MaxLogoDimension)

	// Step 1. It passes the body cap, and not narrowly — the worst decode this
	// product can be made to perform costs a few kilobytes on the wire, which is
	// why the body cap is not a bound on it.
	if len(upload) > MaxLogoUploadBytes {
		t.Fatalf("the fixture is %d bytes and the body cap is %d; a worst case that "+
			"cannot reach the decoder is not the worst case", len(upload), MaxLogoUploadBytes)
	}

	// Step 2. It passes the only header refusal there is.
	cfg, err := DecodeLogoConfig(upload)
	if err != nil {
		t.Fatalf("the largest square the side bound admits was refused: %v", err)
	}
	if cfg.Width*cfg.Height != MaxDecodedLogoPixels {
		t.Fatalf("the header reads %dx%d = %d pixels and the side bound implies %d",
			cfg.Width, cfg.Height, cfg.Width*cfg.Height, MaxDecodedLogoPixels)
	}

	// Step 3, and this is what step 2 admitted.
	src, err := png.Decode(bytes.NewReader(upload))
	if err != nil {
		t.Fatalf("the fixture did not decode: %v", err)
	}
	got := pixelBytes(t, src)
	if got != MaxDecodedLogoBytes {
		t.Errorf("the widest file the caps admit decodes to %d bytes as a %T, and "+
			"MaxDecodedLogoBytes says %d; logo.go's allocation story is out by a "+
			"factor of %.2f", got, src, MaxDecodedLogoBytes,
			float64(got)/float64(MaxDecodedLogoBytes))
	}
	if per := got / MaxDecodedLogoPixels; per != MaxDecodedLogoBytesPerPixel {
		t.Errorf("a decoded pixel measured %d bytes and MaxDecodedLogoBytesPerPixel "+
			"is %d", per, MaxDecodedLogoBytesPerPixel)
	}

	// And it is accepted rather than refused, which is what makes the allocation
	// one this product will really perform rather than one it declines.
	out, err := NormalizeLogo(upload)
	if err != nil {
		t.Fatalf("the widest file the caps admit was refused by NormalizeLogo: %v", err)
	}
	if !out.Resampled() || out.Width*out.Height > MaxLogoPixels {
		t.Errorf("a %dx%d upload stored as %dx%d; the widest one has to be resized "+
			"down to the storage target", out.SourceWidth, out.SourceHeight,
			out.Width, out.Height)
	}
}

// TestNoDecoderOutputIsWiderThanTheStatedPixel is the other half: eight is the
// widest, not merely the widest the fixture above happens to reach.
//
// Every concrete type the two decoders return, measured. `image/png` yields
// Gray, Gray16, NRGBA, NRGBA64, RGBA, RGBA64 and Paletted; `image/jpeg` yields
// Gray, YCbCr and CMYK. A future one this switch does not know about fails in
// pixelBytes rather than passing quietly.
func TestNoDecoderOutputIsWiderThanTheStatedPixel(t *testing.T) {
	const side = 16
	r := image.Rect(0, 0, side, side)
	for _, m := range []image.Image{
		image.NewGray(r),
		image.NewGray16(r),
		image.NewNRGBA(r),
		image.NewNRGBA64(r),
		image.NewRGBA(r),
		image.NewRGBA64(r),
		image.NewPaletted(r, color.Palette{color.Black, color.White}),
		image.NewCMYK(r),
		image.NewYCbCr(r, image.YCbCrSubsampleRatio444),
	} {
		if per := pixelBytes(t, m) / (side * side); per > MaxDecodedLogoBytesPerPixel {
			t.Errorf("%T is %d bytes a pixel and MaxDecodedLogoBytesPerPixel is %d; "+
				"the decode bound is understated for anything decoding to one",
				m, per, MaxDecodedLogoBytesPerPixel)
		}
	}
}
