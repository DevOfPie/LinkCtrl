package qr

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
)

// The first file this product accepts (M50.5).
//
// **Nothing in this package decoded anything before now.** [Code.PNG] encodes,
// and every byte it writes was computed from integers and from colours already
// parsed as `#rrggbb`. A decoder is the opposite shape: the input is chosen by
// somebody else, and the failure modes are the ones this repository has no
// experience of. Every choice below is *do less* — two formats, standard-library
// decoders, a cap enforced before an allocation rather than after it, and an
// output this product produced rather than one it passed through.
//
// **The ordering is the claim.** A byte cap on the request body is not a bound
// on what decoding costs: a decompression bomb is a small file that declares an
// enormous image. So the sequence is
//
//  1. the request body stops at [MaxLogoUploadBytes], enforced by
//     `http.MaxBytesReader` at the handler — internal/httpx/api_qr.go;
//  2. [DecodeLogoConfig] reads the header only and yields the dimensions, and
//     [MaxLogoDimension] is enforced *there*;
//  3. only then is the image decoded;
//  4. and what decoded is resampled down to [MaxLogoPixels] if it is over it.
//
// [NormalizeLogo] performs 2 to 4 in that order and is the only way into the
// decoder, which is what keeps the ordering from being a comment.
//
// **Step 4 is the F214 reopening, and it moved a cap from a refusal to a
// target.** The shipped milestone enforced *two* header bounds — a side and an
// area — and an 813×813 upload passed the first and failed the second, which is
// a sentence no message could make legible: two numbers, no verdict, and no
// action the reader could take. So the area bound stopped refusing anything. It
// is now the size a stored logo is fitted *to*, the side bound is the only thing
// a header is refused for, and what a refusal names is therefore always exactly
// one number and the measurement that crossed it.
//
// **No SVG.** An SVG is a document: it carries script, external references and
// its own entity resolver, so accepting one means sanitising markup this product
// would then serve. PNG and JPEG decode to pixels, which is a smaller thing to
// be wrong about. The refusal names SVG specifically rather than answering
// "unrecognised format", because that is the upload somebody will actually try.
//
// **The decoder is chosen by sniffing the bytes.** Never the filename, never the
// part's declared `Content-Type`, neither of which reaches this package at all —
// see TestTheDecoderIsChosenByTheBytes and, on the handler side,
// TestNothingAboutAnUploadsNameOrDeclaredTypeIsRead.

// The caps, as numbers, with the allocation each one bounds.
//
// This is the standard [MaxSize] set for M49's rasteriser — *a cap is a number
// with the maximum allocation it implies* — and D134 is why it is not optional
// here. The owner chose a `bytea` column for the stored logo against an
// assumption nobody could check at the time: that the caps this milestone sets
// keep a stored image small enough for a column to be uncontroversial. Those
// numbers are below, and so is the worst-case row they imply. Without the
// arithmetic written down, D134 would be true by assertion.
//
// **1. The request body.** [MaxLogoUploadBytes] is 1 MiB and bounds the whole
// multipart body, envelope included, so it is also the largest buffer the
// handler can hold: the part is read under the same `MaxBytesReader`.
//
// **2. The declared image, and this is the only header bound left.**
// [MaxLogoDimension] is 1024 pixels a side, checked against the *header* before
// any pixel buffer exists. One bound rather than two, since the F214 reopening:
// a side cap of 1024 already implies an area of at most 1024 × 1024, so
// [MaxDecodedLogoPixels] is **1,048,576** and is derived from the side rather
// than declared beside it. Nothing can pass the side cap and fail an area cap,
// which is the shape that made the shipped refusal unanswerable.
//
// **3. The decoded image, and a pixel is eight bytes wide rather than four.**
// This package normalizes to [image.NRGBA], which is four — but that is what it
// *produces*, not what the decoder *hands it*. `image/png` decodes a
// bit-depth-16 file to [image.NRGBA64] or [image.RGBA64], **eight bytes a
// pixel**, and such a file is not exotic or large: a 1024×1024 16-bit RGBA PNG
// of one flat colour is about ten kilobytes on the wire, so it passes the body
// cap by two orders of magnitude and the side cap exactly. Every earlier
// statement of this figure — this file's, SECURITY.md's, D135's — computed at
// four and was therefore half of what an upload can actually cost.
//
// The alternative was to refuse bit depth 16 here and keep four true.
// **Rejected**: it adds a refusal to the one milestone whose purpose is to stop
// refusing what it can adapt, and it would refuse a valid PNG for a property its
// author cannot see in any viewer. So the arithmetic moved instead — see
// [MaxDecodedLogoBytesPerPixel], and the D180 entry in decisions.md for the
// trade.
//
// The largest decode this package can be made to perform is therefore
// 1,048,576 × 8 = **8,388,608 bytes** ([MaxDecodedLogoBytes]) — four times what
// the shipped area cap admitted, because the reopening's whole point is that an
// image over the *storage* target is decoded and shrunk rather than refused
// unread. [resampleNRGBA] converts that source once more into its own NRGBA
// buffer, and the destination is bounded by step 4, so the **image buffers**
// this package holds at once are
//
//	the upload, live across the decode  MaxLogoUploadBytes = 1,048,576
//	decoded source        1,048,576 × 8                    = 8,388,608
//	resampler's own copy  1,048,576 × 4                    = 4,194,304
//	resampled destination   262,144 × 4                    = 1,048,576
//	                                                        ----------
//	                                                        14,680,064
//
// **14 MiB exactly, against 4 MiB before** — the shipped pipeline refused
// anything past 262,144 pixels from the header, so its terms were the same
// upload buffer, a 2,097,152-byte decode and a 1,048,576-byte NRGBA copy with no
// resample at all, summing to 4,194,304. The upload is a term because
// [NormalizeLogo] holds it for the whole call: `png.Decode` reads a reader over
// that same slice, so it is live alongside everything the decode allocates. A
// figure called *the peak* that leaves out the upload is not one.
//
// **What the table excludes is the encoder, and the standard library bounds it
// rather than this file.** Two terms, neither a function of the upload's size:
//
//   - the `bytes.Buffer` the PNG is written into grows by doubling, so at its
//     last growth the old array and the new one are both live — under 3,145,728
//     bytes for a worst-case output of about a megabyte;
//   - flate's window and hash tables and the encoder's per-scanline buffers are
//     a fixed cost of one `png.Encode`, measured at about 850,000 bytes on
//     go1.26 and the same for a 1×1 image as for the largest one.
//
// Under 4 MiB together, so **under 18 MiB for one upload in flight**. Stated
// beside the table rather than summed into it because both terms are properties
// of a Go release, which is the same reason [MaxLogoStoredBytes] carries slack
// over its derivation; TestTheCapsAgreeWithEachOther pins the 14,680,064 this
// file's own caps do bound. The handler's read buffer doubles the same way and
// does not add to the peak — it reaches its own largest size before anything is
// decoded, and what it hands over is the first row of the table.
//
// That is the price of downscaling instead of refusing, and it is why uploads
// have a rate limit bucket of their own rather than sharing the write bucket.
//
// **4. The stored artefact, which is the number D134 is owed.** [MaxLogoPixels]
// is 262,144 and is a *target*, not a refusal: anything above it is resampled
// down to fit, keeping its aspect ratio, and the caller is told what it was and
// what it became. The output is a
// PNG this product encoded from that NRGBA, and the worst case is an image whose
// pixels do not compress at all, so deflate falls back to stored blocks. It is
// also the *tallest* such image the caps allow rather than the squarest, because
// PNG spends a filter byte per scanline: at the area cap, 1024×256 costs 1024
// filter bytes where 512×512 costs 512.
//
//	filtered scanlines    1024 × (1 + 256×4)               = 1,049,600
//	deflate stored blocks ceil(1,049,600 / 65,535) × 5     =        85
//	zlib header and Adler-32                               =         6
//	IDAT chunk framing    ceil(1,049,691 / 32,768) × 12    =       396
//	PNG signature, IHDR and IEND                           =        45
//	                                                         ---------
//	                                                         1,050,132
//
// [MaxLogoStoredBytes] is **1,060,000** — that bound with room over it, because
// two of its terms (the stored-block size deflate falls back to, and the 32 KiB
// buffer Go's encoder flushes IDAT chunks at) are properties of the standard
// library rather than of the PNG format. And it is **enforced rather than
// argued**: [NormalizeLogo] refuses an encoding above it instead of trusting the
// arithmetic, so a Go release that frames its output differently produces a
// failed upload rather than a row past the bound.
// TestTheWorstCaseLogoFitsTheStatedBound builds that exact image out of
// incompressible pixels and pins both halves — that the real figure is close to
// the derivation, and that it is under the constant.
//
// **So the worst case a `qr_codes` row can carry is 1,060,000 bytes**, and a
// link at domain.MaxQRCodesPerLink — twenty — is bounded at 21,200,000 bytes,
// about 20 MiB. That is the sizing question D134 accepted, stated as a number:
// it is in the row, in every backup and in every `pg_dump`. Typical logos are
// two orders of magnitude below it; the ceiling is what an adversary can reach.
const (
	MaxLogoUploadBytes = 1 << 20 // 1,048,576
	MaxLogoDimension   = 1024
	MaxLogoPixels      = 262_144
	MaxLogoStoredBytes = 1_060_000

	// MaxDecodedLogoPixels is the decode bound the F214 reopening had to state,
	// and it is *derived* rather than chosen: the side cap admits nothing larger
	// than a square at that side, so this is what MaxLogoDimension already
	// implies. Written down because the allocation above is written down.
	MaxDecodedLogoPixels = MaxLogoDimension * MaxLogoDimension // 1,048,576

	// MaxDecodedLogoBytesPerPixel is how wide a pixel the decoders hand back,
	// which is not how wide one this package writes is. `image/png` returns
	// *image.NRGBA64 or *image.RGBA64 for a bit-depth-16 file and *image.Gray16
	// for a 16-bit greyscale one; `image/jpeg` returns *image.YCbCr or
	// *image.CMYK, both narrower. Eight is the widest either produces.
	//
	// It is a constant rather than a `4` inlined into the arithmetic because a
	// `4` inlined into the arithmetic is exactly how the shipped figure came to
	// be half of the real one.
	MaxDecodedLogoBytesPerPixel = 8

	// MaxDecodedLogoBytes is the pixel buffer those two bound together, and it
	// is the number the allocation story above turns on.
	//
	// **It is measured, not asserted.** TestTheDecodeBoundIsMeasuredNotAssumed
	// builds the widest file the caps admit, puts it through the real decoder,
	// and compares this constant against the buffer that came back;
	// TestTheCapsAgreeWithEachOther then sums the peak table out of buffers the
	// standard library allocated rather than out of this file's multiplications.
	// A test that re-derives the code's own arithmetic checks nothing, which is
	// the failure that let the four-byte figure ship.
	MaxDecodedLogoBytes = MaxDecodedLogoPixels * MaxDecodedLogoBytesPerPixel // 8,388,608
)

// The refusals, as sentinels rather than as messages.
//
// internal/qr holds no opinion about HTTP status codes and does not import
// internal/domain, so the wording a caller sees is written in internal/link
// beside the other validation errors — the shape [ErrTooLarge] already
// established for M49's rasteriser.
var (
	// ErrLogoEmpty is an upload with no bytes in it.
	ErrLogoEmpty = errors.New("logo: no bytes")
	// ErrLogoFormat is an upload that is neither PNG nor JPEG.
	ErrLogoFormat = errors.New("logo: not a PNG or a JPEG")
	// ErrLogoSVG is the one refused format worth naming, because it is the one
	// somebody will try.
	ErrLogoSVG = errors.New("logo: SVG is a document, not an image")
	// ErrLogoTooLarge is an image this package will not decode. Raised from the
	// header, before a pixel buffer exists. Every one of them carries a
	// [LogoBoundError] with the measurement and the single bound it crossed —
	// see that type for why "a sentinel plus a sentence" was not enough.
	ErrLogoTooLarge = errors.New("logo: too large to decode")
	// ErrLogoUndecodable is a file whose header sniffed as PNG or JPEG and whose
	// body then did not decode.
	ErrLogoUndecodable = errors.New("logo: does not decode")
	// ErrLogoStoreTooLarge is a re-encoding above MaxLogoStoredBytes. Unreachable
	// through the caps above by the arithmetic in this file's comment, and
	// checked anyway — an arithmetic bound nothing enforces is the shape D134
	// asked this milestone not to leave behind.
	ErrLogoStoreTooLarge = errors.New("logo: re-encodes above the stored bound")
)

// LogoBoundError is a refusal that names what was measured and the one bound it
// crossed.
//
// **A sentinel and a sentence were not enough, and F214 is the proof.** The
// shipped refusal read *"a logo is at most 1024 pixels on a side and 262,144
// pixels in total"* for an 813×813 upload — two bounds, neither of them
// obviously the one that bit, and no mention of what the file actually
// measured. The caller could not tell which number to fix. Carrying the
// measurement and the limit as *fields* is what lets internal/link write a
// sentence with a verdict in it instead of a restatement of the rules.
//
// It answers [errors.Is] for [ErrLogoTooLarge], so every existing caller that
// matched the sentinel goes on matching it.
type LogoBoundError struct {
	// Width and Height are what the file declared, in pixels.
	Width, Height int
	// Bound names which limit was crossed: "side" or "pixels". Two values
	// because two callers enforce a bound — this file refuses an oversized
	// *upload* by its side, and composite.go refuses an oversized *stored* image
	// by its area (the storage target, which nothing but a hand-written row can
	// exceed).
	Bound string
	// Limit is that bound's value.
	Limit int
}

func (e *LogoBoundError) Error() string {
	if e.Bound == "side" {
		side, which := e.Width, "wide"
		if e.Height > e.Width {
			side, which = e.Height, "tall"
		}
		return fmt.Sprintf("%s: %dx%d, and %d pixels %s is past the %d a side this "+
			"product decodes", ErrLogoTooLarge, e.Width, e.Height, side, which, e.Limit)
	}
	return fmt.Sprintf("%s: %dx%d is %d pixels, and %d is the most this product holds",
		ErrLogoTooLarge, e.Width, e.Height, e.Width*e.Height, e.Limit)
}

// Is reports that this is an ErrLogoTooLarge, so the sentinel goes on being the
// thing callers switch on.
func (e *LogoBoundError) Is(target error) bool { return target == ErrLogoTooLarge }

// Logo is a stored logo: bytes this product produced, and the size they draw at.
type Logo struct {
	// PNG is what goes in the column. Never the received bytes.
	PNG []byte
	// Width and Height are the stored dimensions, carried so a caller can
	// report them without decoding the output again.
	Width, Height int
	// SourceWidth and SourceHeight are what was uploaded. They differ from the
	// pair above exactly when the image was resampled to fit MaxLogoPixels, and
	// they exist so the caller can say so — a product that silently shrinks
	// somebody's artwork and reports success has told them nothing.
	SourceWidth, SourceHeight int
}

// Resampled reports whether the stored image is a shrunk copy of the upload.
func (l Logo) Resampled() bool {
	return l.SourceWidth != l.Width || l.SourceHeight != l.Height
}

// logoFormat is what sniffing the leading bytes concluded.
type logoFormat int

const (
	formatUnknown logoFormat = iota
	formatPNG
	formatJPEG
	formatSVG
)

// pngMagic and jpegMagic are the signatures sniffLogo matches.
//
// Written out here rather than delegated to [image.DecodeConfig] deliberately.
// That function dispatches through a *process-global* registry that any package
// anywhere in the binary can add to with a blank import, so what it accepts is a
// property of the whole program rather than of this file — a later
// `_ "image/gif"` three packages away would silently widen what this product
// takes. Two constants and a prefix comparison cannot be widened by an import.
var (
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
	jpegMagic = []byte("\xff\xd8\xff")
)

// sniffLogo decides the decoder from the bytes and from nothing else.
func sniffLogo(b []byte) logoFormat {
	switch {
	case bytes.HasPrefix(b, pngMagic):
		return formatPNG
	case bytes.HasPrefix(b, jpegMagic):
		return formatJPEG
	case looksLikeSVG(b):
		return formatSVG
	default:
		return formatUnknown
	}
}

// looksLikeSVG recognises the upload this milestone refuses by name.
//
// Deliberately loose, and it does not have to be anything else: it decides
// which *refusal* is reported, never whether one happens. Anything it misses
// falls through to ErrLogoFormat, which refuses the file just as completely.
func looksLikeSVG(b []byte) bool {
	// A UTF-8 BOM and leading whitespace both appear ahead of the root element
	// in files editors produce, and neither changes what the document is.
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	b = bytes.TrimLeft(b, " \t\r\n")
	if len(b) > 512 {
		b = b[:512]
	}
	lower := bytes.ToLower(b)
	return bytes.HasPrefix(lower, []byte("<?xml")) ||
		bytes.HasPrefix(lower, []byte("<svg")) ||
		bytes.HasPrefix(lower, []byte("<!doctype svg"))
}

// LogoConfig is what step 2 learns without decoding anything.
type LogoConfig struct {
	Width, Height int
	// Format is "png" or "jpeg", as sniffed. Reported so a caller can say which
	// decoder ran without re-sniffing.
	Format string
}

// DecodeLogoConfig reads the header only.
//
// **This is the step that bounds the allocation, and it is separated from
// [NormalizeLogo] so it can be called on its own by a test that proves the
// bound is enforced before any pixel buffer exists.** `DecodeConfig` on both
// standard-library decoders parses the header and returns; neither allocates an
// image, which is what makes checking the dimensions here different in kind from
// checking them after `Decode`.
func DecodeLogoConfig(upload []byte) (LogoConfig, error) {
	if len(upload) == 0 {
		return LogoConfig{}, ErrLogoEmpty
	}
	var (
		cfg  image.Config
		err  error
		name string
	)
	switch sniffLogo(upload) {
	case formatPNG:
		name = "png"
		cfg, err = png.DecodeConfig(bytes.NewReader(upload))
	case formatJPEG:
		name = "jpeg"
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(upload))
	case formatSVG:
		return LogoConfig{}, ErrLogoSVG
	default:
		return LogoConfig{}, ErrLogoFormat
	}
	if err != nil {
		return LogoConfig{}, fmt.Errorf("%w: %s header: %w", ErrLogoUndecodable, name, err)
	}
	if cfg.Width < 1 || cfg.Height < 1 {
		return LogoConfig{}, fmt.Errorf("%w: %s header declares %dx%d",
			ErrLogoUndecodable, name, cfg.Width, cfg.Height)
	}
	// One bound, so the refusal has a verdict in it. The area a decode costs is
	// what this is really protecting, and the side cap already bounds it at
	// MaxDecodedLogoPixels — see the constant block.
	if cfg.Width > MaxLogoDimension || cfg.Height > MaxLogoDimension {
		return LogoConfig{}, &LogoBoundError{
			Width: cfg.Width, Height: cfg.Height, Bound: "side", Limit: MaxLogoDimension,
		}
	}
	return LogoConfig{Width: cfg.Width, Height: cfg.Height, Format: name}, nil
}

// NormalizeLogo turns an upload into the bytes this product will store.
//
// **What comes out is never what went in**, and that is three defences in one
// step rather than a tidiness preference. A polyglot — a file that is a valid
// PNG *and* a valid something-else — stops being one, because only the pixels
// survive. Metadata goes with it: EXIF, colour profiles, XMP, a JPEG comment
// holding whatever somebody put there. And what this product later serves is
// bytes it encoded, so a decoder bug in a reader downstream is not reachable
// through a file this instance merely relayed.
//
// The intermediate is [image.NRGBA] rather than whatever the decoder returned,
// which is what makes MaxLogoStoredBytes computable at all: a JPEG decodes to
// YCbCr and a paletted PNG to [image.Paletted], and the encoder's output size
// depends on which. One buffer shape, one arithmetic.
//
// **And it is shrunk to fit rather than refused** (F214). Everything past
// [MaxLogoPixels] is resampled down to it with its aspect ratio kept, through
// the same [resampleNRGBA] the composited drawing uses — the scaler M50.6 wrote
// by hand rather than adding a module, reused here rather than copied. The
// returned [Logo] carries both sizes so the caller can say what happened.
func NormalizeLogo(upload []byte) (Logo, error) {
	cfg, err := DecodeLogoConfig(upload)
	if err != nil {
		return Logo{}, err
	}

	// Only now, and only for a header that has already been bounded.
	var src image.Image
	switch cfg.Format {
	case "png":
		src, err = png.Decode(bytes.NewReader(upload))
	default:
		src, err = jpeg.Decode(bytes.NewReader(upload))
	}
	if err != nil {
		return Logo{}, fmt.Errorf("%w: %s body: %w", ErrLogoUndecodable, cfg.Format, err)
	}

	// The decoder is trusted for pixels and not for arithmetic: a header saying
	// one size and a body producing another is a decoder this product should
	// refuse rather than accommodate, and the buffer below is sized from what
	// came back either way.
	b := src.Bounds()
	if b.Dx() != cfg.Width || b.Dy() != cfg.Height {
		return Logo{}, fmt.Errorf("%w: %s header declared %dx%d and the body decoded %dx%d",
			ErrLogoUndecodable, cfg.Format, cfg.Width, cfg.Height, b.Dx(), b.Dy())
	}

	// resampleNRGBA is a copy when the size is unchanged and an area average when
	// it is not, so this one call covers both and the destination is never larger
	// than the storage target.
	w, h := FitStoredLogo(b.Dx(), b.Dy())
	out := resampleNRGBA(src, w, h)

	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&buf, out); err != nil {
		return Logo{}, fmt.Errorf("re-encode logo: %w", err)
	}
	if buf.Len() > MaxLogoStoredBytes {
		return Logo{}, fmt.Errorf("%w: %d bytes, and a stored logo stops at %d",
			ErrLogoStoreTooLarge, buf.Len(), MaxLogoStoredBytes)
	}
	return Logo{
		PNG: buf.Bytes(), Width: w, Height: h,
		SourceWidth: b.Dx(), SourceHeight: b.Dy(),
	}, nil
}

// FitStoredLogo is the size an image of w×h is stored at.
//
// **Both bounds, one ratio, and never larger than what came in.** The side bound
// is [MaxLogoDimension] and the area bound is [MaxLogoPixels]; an image inside
// both is stored untouched, because resampling a picture that already fits would
// throw away detail to no purpose. Anything outside either is scaled by the
// smallest factor that satisfies both, which keeps the aspect ratio: a wordmark
// stays a wordmark, which is [fitInside]'s reasoning applied to a rectangle
// rather than to a square.
//
// Rounding is to nearest rather than down, and then corrected. Flooring
// costs the common case a pixel for nothing — 813×813 lands on 511.99 and
// would store 511×511 where 512×512 fits exactly — and rounding up can cross
// the area bound by one row, so the loop below walks the longer side back until
// it does not. It runs at most a handful of times: one step of the longer side
// removes a whole row or column.
func FitStoredLogo(w, h int) (int, int) {
	if w < 1 || h < 1 {
		return max(w, 1), max(h, 1)
	}
	if w <= MaxLogoDimension && h <= MaxLogoDimension && w*h <= MaxLogoPixels {
		return w, h
	}
	f := math.Sqrt(float64(MaxLogoPixels) / float64(w*h))
	f = min(f, float64(MaxLogoDimension)/float64(w), float64(MaxLogoDimension)/float64(h))
	tw := max(1, int(math.Round(float64(w)*f)))
	th := max(1, int(math.Round(float64(h)*f)))
	for tw*th > MaxLogoPixels || tw > MaxLogoDimension || th > MaxLogoDimension {
		if tw >= th && tw > 1 {
			tw--
		} else if th > 1 {
			th--
		} else {
			break
		}
	}
	return tw, th
}
