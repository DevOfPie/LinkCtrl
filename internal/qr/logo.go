package qr

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
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
//     [MaxLogoDimension] and [MaxLogoPixels] are enforced *there*;
//  3. only then is the image decoded.
//
// [NormalizeLogo] performs 2 and 3 in that order and is the only way into the
// decoder, which is what keeps the ordering from being a comment.
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
// **2. The declared image.** [MaxLogoDimension] is 1024 pixels a side and
// [MaxLogoPixels] is 262,144 in total, both checked against the *header* before
// any pixel buffer exists. **Two caps because they answer different questions,
// and each refuses something the other admits**: a 2048×1 strip is 2048 pixels
// of nothing and fails the first while passing the second, and a 1024×1024
// image is a megapixel buffer that fails the second while passing the first.
// The area is what the allocation is proportional to; the side is what M50.6's
// geometry has to place inside a code that stops at [MaxSize].
//
// **3. The decoded image.** Normalized to [image.NRGBA], four bytes a pixel, so
// the largest decode this package can be made to perform is 262,144 × 4 =
// **1,048,576 bytes**.
//
// **4. The stored artefact, which is the number D134 is owed.** The output is a
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
	// ErrLogoTooLarge is an image whose *declared* dimensions exceed
	// MaxLogoDimension or MaxLogoPixels. Raised from the header, before a pixel
	// buffer exists.
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

// Logo is a stored logo: bytes this product produced, and the size they draw at.
type Logo struct {
	// PNG is what goes in the column. Never the received bytes.
	PNG []byte
	// Width and Height are the decoded dimensions, carried so a caller can
	// report them without decoding the output again.
	Width, Height int
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
	if cfg.Width > MaxLogoDimension || cfg.Height > MaxLogoDimension ||
		cfg.Width*cfg.Height > MaxLogoPixels {
		return LogoConfig{}, fmt.Errorf("%w: %dx%d, and a logo stops at %d pixels a side "+
			"and %d pixels in total", ErrLogoTooLarge,
			cfg.Width, cfg.Height, MaxLogoDimension, MaxLogoPixels)
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

	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)

	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&buf, out); err != nil {
		return Logo{}, fmt.Errorf("re-encode logo: %w", err)
	}
	if buf.Len() > MaxLogoStoredBytes {
		return Logo{}, fmt.Errorf("%w: %d bytes, and a stored logo stops at %d",
			ErrLogoStoreTooLarge, buf.Len(), MaxLogoStoredBytes)
	}
	return Logo{PNG: buf.Bytes(), Width: b.Dx(), Height: b.Dy()}, nil
}
