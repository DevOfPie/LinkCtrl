package addon

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// This file is M68.6's container: the two files an install needs, as one object
// that can be fetched in one request and covered by one digest.
//
// # Why there is a container at all
//
// An install is a manifest *and* a module, and M67's upload carries both because
// a multipart body has parts. A fetch has one response. The alternative — fetch
// the manifest, read the module's name out of it, fetch that too — is the one
// shape m68.6.md rules out by name: it makes the pair separable, so the manifest
// an operator's digest covered and the module that actually got installed could
// come from two different moments and two different files. One object, one
// digest, and the pair cannot be taken apart on the wire.
//
// # Three containers, and the member rule is what makes that affordable
//
// `.tar`, `.tar.gz` and `.zip` are all accepted, which is [D384] and is the
// owner's answer to a question this milestone's first pass decided for itself:
// *allow all 3 and limit the directory depth that is extracted for compressed
// options as well if it will help defend against exploits*. It does help, and it
// is the bound this reader was already built on — **exactly two plain files,
// bare names, no directory component, no symlink, no traversal, no duplicates**.
// That is depth zero, the strictest form of the limit that was asked for.
//
// **The rule is one rule, not three.** [bundleMembers] holds it, every format
// walks its own entries into that one object, and a test asserts the same table
// of hostile shapes against all three — so a container format cannot be the thing
// that widens what a bundle may contain. The reason to have accepted only tar was
// that a compressed container has to be defended; the reason accepting the other
// two is affordable is that a bomb still has to present two bare-named plain
// files, and [maxBundleInflated] refuses it long before the member rule is
// reached. Format variety is a parser question rather than a policy question.
//
// **Format is decided by the bytes, never by the URL.** An extension is typed by
// whoever supplied the address and proves nothing at all about what is served; a
// `.tar.gz` that is really a zip is a fact about the file rather than an error
// about its name. See [detectBundleFormat].
//
// # What bounds a compressed bundle, and when it is allowed to cost anything
//
// An uncompressed tar needs one bound: [MaxUploadBytes] caps the fetch, and the
// members of a tar are smaller than the tar. Inflation breaks that arithmetic, so
// a compressed bundle is bounded **on the wire as before, and again on what it
// may amount to once opened** — [bundleInflatedLimit], which is
// [maxBundleInflated] and [maxBundleRatio] as one number rather than as two
// checks in sequence.
//
// **One number, because it is the point the decompressor is stopped at.** A limit
// evaluated after inflating has already bought the allocation and the CPU it
// existed to refuse: a two-kilobyte gzip of zeros read to 32 MiB and *then*
// declined has cost this host the whole 32 MiB for the price of one small
// request, which is the attack rather than the defence. So the ratio is not a
// verdict passed on what was produced; it is what makes the stopping point small
// when the request was small. The zip reader reaches the same number from the
// other side — its central directory declares the sizes, so it refuses before a
// byte is inflated at all — and that is one bound applied at each format's
// earliest opportunity rather than two policies.
//
// Both ends of that bound refuse with [CodeBundleExpands] rather than with
// [CodeBundleInvalid]: an operator whose release archive is legitimate and
// enormous has something different to do about it than one who fetched a web page
// by mistake.
//
// # Exactly two members, and nothing that is not a file
//
// A tar can express a directory, a symbolic link, a hard link, a device node, a
// path with `..` in it and a name with a separator; a zip can express all of that
// plus duplicate names and a compression method nobody has heard of. None of
// those is a thing an add-on is made of, so every one is refused by name here and
// the reader never reaches a filesystem at all — the members are held in memory
// and handed to the same [Host.stage] an upload goes through, which writes them
// under names it derives from the manifest rather than from the archive. So a
// hostile bundle cannot choose a path even if this reader were wrong about one of
// them.

// maxBundleMembers is how many entries this reader will walk before it stops.
//
// Two is what a bundle holds, so this is not the bound — the refusal below is.
// It exists so that an archive declaring a million empty members is a refusal
// after three headers rather than a walk, which is the one shape the byte cap
// does not already bound: a tar header is 512 bytes and 32 MiB of them is 65,536
// entries, and a zip's central directory is cheaper still per entry.
const maxBundleMembers = 3

// maxBundleInflated is the ceiling of what a compressed bundle may amount to once
// it has been decompressed, whatever it was fetched as — the upper end of
// [bundleInflatedLimit], where [maxBundleRatio] supplies the lower.
//
// The same number as the fetch cap, and deliberately the same: what is being
// bounded is what an install may hold in memory, and where the compression
// boundary falls does not change that. It is written as its own constant because
// it is a *different bound* that happens to share a value — the wire cap is about
// what this host will download and this one is about what it will produce.
const maxBundleInflated = MaxUploadBytes

// maxBundleRatio is how much larger than its compressed form a bundle may
// inflate to.
//
// **The absolute cap is not enough on its own**, which is what [D384] asks this
// to answer: with [maxBundleInflated] alone, a two-kilobyte gzip of zeros still
// buys 32 MiB of allocation and the CPU to produce it, for the cost of one small
// request. The ratio is what makes a cheap request stay a cheap refusal — and it
// only makes it one because the figure is applied as a bound on the *read*, in
// [bundleInflatedLimit]. A ratio measured after the fact would be a description
// of what had already been spent.
//
// **Fifty, against measurements at both ends.** A `GOOS=wasip1` module — the
// thing actually in these archives — gzips at three to five times: this
// repository's own fixtures measure 1,866,051 bytes to 570,076 and 3,609,321 to
// 1,027,181, which is 3.3 and 3.5. Text-heavy containers reach ten. Deflate's
// ceiling is 1032. So fifty is an order of magnitude above anything a publisher
// produces and an order of magnitude below what an attack needs, which is the
// only shape of bound worth having when both ends are estimates.
//
// **Pinned where it binds**, which is between roughly 21 KB and 640 KB fetched —
// the only range in which neither [maxBundleRatioFloor] nor [maxBundleInflated]
// sets the limit. A bomb small enough to build cheaply is refused by the floor
// whatever this constant says, so a test built out of one asserts nothing about
// it: TestTheRatioBindsWhereTheFetchedSizeSetsTheLimit is built inside the window
// and writes fifty out as a literal for that reason.
const maxBundleRatio = 50

// maxBundleRatioFloor is the inflated size below which [maxBundleRatio] is not
// applied.
//
// **A small archive's ratio is noise rather than a signal.** A tar pads every
// member to 512 bytes and ends with 1024 zero bytes, so a bundle holding a
// hundred-byte module is mostly padding and compresses at twenty or thirty times
// while amounting to nothing. Below this floor the absolute cost is already
// trivial — a megabyte is not a bomb — so the ratio would be refusing legitimate
// small bundles for a property that says nothing about them.
const maxBundleRatioFloor = 1 << 20

// bundleError is a refusal from this reader, carrying the code the surfaces
// branch on beside the sentence an operator reads.
//
// Two codes come out of this file and the split is what m68.6.md means by *a
// refusal names which bound bit*: [CodeBundleInvalid] is *these bytes are not an
// add-on bundle*, and [CodeBundleExpands] is *they are an archive and it is too
// big once opened*. Nothing an operator does about the first helps with the
// second.
type bundleError struct {
	code string
	msg  string
}

func (e bundleError) Error() string { return e.msg }

// invalidBundle and expandedBundle are the two constructors, so that a refusal
// added later has to pick a code rather than inheriting one.
func invalidBundle(format string, a ...any) error {
	return bundleError{code: CodeBundleInvalid, msg: fmt.Sprintf(format, a...)}
}

func expandedBundle(format string, a ...any) error {
	return bundleError{code: CodeBundleExpands, msg: fmt.Sprintf(format, a...)}
}

// bundleRefusal reads the code out of a refusal this file produced, defaulting to
// [CodeBundleInvalid] for anything that reached the caller without one.
func bundleRefusal(err error) string {
	var be bundleError
	if errors.As(err, &be) {
		return be.code
	}
	return CodeBundleInvalid
}

var errNotABundle = bundleError{
	code: CodeBundleInvalid,
	msg: "the fetched bytes are not an add-on bundle: a bundle is a tar, a gzipped " +
		"tar or a zip holding " + ManifestFile + " and the module it names",
}

// bundleFormat is what the leading bytes say this is.
type bundleFormat int

const (
	formatUnknown bundleFormat = iota
	formatTar
	formatTarGz
	formatZip
)

// detectBundleFormat decides the container from its content and from nothing
// else.
//
// **The URL's extension is never read**, and that is a security property rather
// than a convenience: the address was typed by whoever is being trusted least in
// this transaction, so a name is a claim and the magic number is a fact. It also
// means a publisher whose release page serves `module.bin` is not turned away for
// it.
//
// Three signatures. gzip's is two bytes at the head, and it is checked first
// because a gzipped tar has no tar magic until it is inflated. zip's local file
// header is `PK\x03\x04`; the other two `PK` signatures are the end-of-directory
// record of an empty archive and a spanned archive's marker, and both are matched
// here so that they refuse as *a zip holding the wrong thing* rather than as
// unrecognised bytes. tar's is the POSIX `ustar` magic at offset 257, which
// USTAR, PAX and GNU archives all carry.
func detectBundleFormat(raw []byte) bundleFormat {
	switch {
	case len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b:
		return formatTarGz
	case len(raw) >= 4 && raw[0] == 'P' && raw[1] == 'K' &&
		(raw[2] == 3 && raw[3] == 4 || raw[2] == 5 && raw[3] == 6 || raw[2] == 7 && raw[3] == 8):
		return formatZip
	case len(raw) >= 262 && string(raw[257:262]) == "ustar":
		return formatTar
	}
	return formatUnknown
}

// unbundle reads the manifest and the module out of one fetched bundle.
//
// Returns the manifest bytes **verbatim** — the same property [Host.stage] relies
// on for an upload, and for the same reason: what a publisher signed off is the
// file's own text, and nothing in this product covers those bytes with a digest
// once they are on disk. The module's name inside the archive is returned so the
// caller can hold it against what the manifest declares; nothing is written under
// it.
func unbundle(raw []byte) (manifest []byte, moduleName string, module []byte, err error) {
	switch detectBundleFormat(raw) {
	case formatTar:
		return readTar(raw)
	case formatTarGz:
		inner, err := inflate(raw)
		if err != nil {
			return nil, "", nil, err
		}
		return readTar(inner)
	case formatZip:
		return readZip(raw)
	case formatUnknown:
	}
	// Nothing recognised the leading bytes, and that is the refusal an HTML error
	// page or a bare `.wasm` arrives at: a bundle is a container, and no container
	// is being read out of a name.
	return nil, "", nil, errNotABundle
}

// bundleInflatedLimit is the most a container of this many fetched bytes may
// amount to once it is opened: [maxBundleInflated] and [maxBundleRatio] as a
// single figure, so that there is something to *stop at* rather than something to
// compare against afterwards.
//
// Which of the two produced it is what words the refusal, and it is recoverable
// from the number: a limit equal to [maxBundleInflated] is the absolute cap
// binding, and anything below it is the ratio. That is why this returns the limit
// rather than a verdict — the caller needs the figure to bound a read with, and
// [overInflated] needs it to say which bound bit.
//
// A container already as large as the cap is bounded by the cap alone, which is
// stated as an early return rather than left to the clamp below because it is
// also what keeps the multiplication from overflowing.
func bundleInflatedLimit(compressed int64) int64 {
	if compressed >= maxBundleInflated {
		return maxBundleInflated
	}
	limit := compressed * maxBundleRatio
	if limit < maxBundleRatioFloor {
		// [maxBundleRatioFloor]: below this the ratio is measuring a format's padding
		// rather than an archive's intent, so the absolute size is the only bound.
		limit = maxBundleRatioFloor
	}
	if limit > maxBundleInflated {
		limit = maxBundleInflated
	}
	return limit
}

// overInflated is the refusal both formats produce, worded by which bound made
// the limit — because *too big* and *expands too fast* are different things for
// the operator to do something about, and neither refusal is actionable without a
// figure in it.
//
// **It tells two bounds apart and there are three**, which is a deliberate
// imprecision rather than an oversight. `bundleInflatedLimit` can be set by the
// cap, by the ratio, or by [maxBundleRatioFloor]; this distinguishes only the cap
// from everything else, so a bundle fetched small enough for the floor to bind is
// told it expanded past fifty times when the figure that actually applied was the
// floor. The sentence stays **true** — the limit is never below fifty times the
// compressed size — and a third wording would have to explain a floor to an
// operator whose real problem is that their archive is mostly padding. Named here
// so a reader of the two branches does not conclude the floor is unreachable.
//
// It says *more than* rather than naming what was measured: a reader that stopped
// at the limit does not know what the container would have amounted to, and that
// ignorance is the point of stopping.
func overInflated(limit int64, subject string) error {
	if limit == maxBundleInflated {
		return expandedBundle("%s more than %s, which is as much as an add-on bundle "+
			"may be", subject, byteBound(maxBundleInflated))
	}
	return expandedBundle("%s more than %d times the bytes it was fetched as; a "+
		"container that expands like that is a decompression bomb rather than a "+
		"module, and nothing a publisher builds comes near it", subject, maxBundleRatio)
}

// The two voices [overInflated] speaks in. A gzip stream was read and what it
// produced is a measurement; a zip's central directory is a claim the archive
// makes about itself, and saying so is what tells an operator that nothing was
// unpacked to find it out.
const (
	bundleUnpacksTo  = "that bundle is compressed and unpacks to"
	zipSaysUnpacksTo = "that zip says it unpacks to"
)

// inflate is the gzip layer, and the whole of what makes accepting one
// affordable.
//
// **The limit is the read's**, which is the entire defence: the stream is read
// through one byte past [bundleInflatedLimit], so what a bomb costs is set by the
// largest bundle that many fetched bytes could have amounted to rather than by
// the cap — a few kilobytes fetched buy a mebibyte, not 32 of them. Bounding at
// [maxBundleInflated] alone and consulting the ratio afterwards would spend the
// whole 32 MiB before deciding not to, which is the cost the ratio exists to
// remove. `io.ReadAll` grows by doubling on top of that figure, and that is the
// reason it is the figure and not the cap: the constant is the standard
// library's, the number it multiplies is this one.
//
// The extra byte is what separates *the limit was exactly reached* from *the
// limit was passed*, so a bundle that is precisely as large as it may be is not
// refused for it.
func inflate(raw []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, errNotABundle
	}
	defer func() { _ = zr.Close() }()

	limit := bundleInflatedLimit(int64(len(raw)))
	inner, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		// A truncated or corrupt deflate stream. Whatever was produced before it
		// failed is discarded — a bundle is the whole object or it is nothing.
		return nil, errNotABundle
	}
	if int64(len(inner)) > limit {
		return nil, overInflated(limit, bundleUnpacksTo)
	}
	return inner, nil
}

// bundleMembers is the member rule, and it is one rule for all three formats.
//
// **Every container walks its entries into this**, which is what stops the tar
// path and the zip path from drifting into two policies — a defect that would not
// show up as a failing test in either of them, because each would be enforcing
// something. A test asserts the same table of hostile shapes against all three
// formats for exactly that reason.
//
// `budget` is the inflated bound spent across members rather than per member: a
// zip holding two entries of 30 MiB each is 60 MiB of allocation and the cap is
// on what an install holds, not on any one file in it.
type bundleMembers struct {
	seen   int
	budget int64
	files  map[string][]byte
	others []string
}

func newBundleMembers() *bundleMembers {
	return &bundleMembers{budget: maxBundleInflated, files: map[string][]byte{}}
}

// header is every part of the rule that can be decided before a byte of content
// is read: how many entries, what kind of thing each is, and what it is called.
func (b *bundleMembers) header(name string, plain bool) error {
	b.seen++
	if b.seen > maxBundleMembers {
		return invalidBundle("the bundle holds more than %d entries, and an add-on "+
			"bundle holds two: %s and the module it names", maxBundleMembers, ManifestFile)
	}
	if !plain {
		// A directory, a symlink, a hard link, a device, a fifo. Refused by name
		// rather than skipped, because a bundle containing one was built by
		// something that thought it was installing a tree, and quietly installing
		// two of its files instead is worse than saying no. This is also the depth
		// limit [D384] asks for, at depth zero: a bundle cannot contain a directory,
		// so there is no tree to bound.
		return invalidBundle("the bundle holds %q, which is not a plain file; an "+
			"add-on bundle holds two plain files and nothing else", safeName(name))
	}
	if !isBareFilename(name) {
		// The name never reaches a filesystem — stage writes under names it takes
		// from the manifest — so this is a refusal about what the bundle *is*
		// rather than a path defence standing alone. Both, deliberately: a
		// defence that is the only one is a defence nobody can check.
		return invalidBundle("the bundle holds %q, and every entry in an add-on "+
			"bundle is a bare filename", safeName(name))
	}
	if _, dup := b.files[name]; dup {
		return invalidBundle("the bundle holds %q twice, so which of the two an "+
			"install would use is a question about the archive format rather than "+
			"about the add-on", safeName(name))
	}
	return nil
}

// read pulls one member's content through what is left of the inflated budget.
//
// The limit is a byte past what remains, so *the budget was exactly spent* and
// *the budget was exceeded* are different outcomes rather than the same one.
//
// **This is a backstop and it is written as one.** Every container reaching here
// has already been bounded — a plain tar by the fetch cap, a gzipped one by
// [inflate], a zip by its declared sizes and by `archive/zip` refusing a member
// that outruns its own declaration — so nothing in the suite reaches this
// refusal. It exists because each of those bounds belongs to something else: two
// are arithmetic about a format and one is the standard library's, and what an
// install may hold in memory should not be a property this file has to infer from
// any of them.
func (b *bundleMembers) read(name string, r io.Reader) error {
	body, err := io.ReadAll(io.LimitReader(r, b.budget+1))
	if err != nil {
		return errNotABundle
	}
	if int64(len(body)) > b.budget {
		return expandedBundle("%q inside the bundle takes the bundle past %s, which "+
			"is as much as an add-on bundle may unpack to", safeName(name),
			byteBound(maxBundleInflated))
	}
	b.budget -= int64(len(body))
	b.files[name] = body
	if name != ManifestFile {
		b.others = append(b.others, name)
	}
	return nil
}

// result is the last two clauses of the rule: a manifest, and exactly one other
// file.
func (b *bundleMembers) result() (manifest []byte, moduleName string, module []byte, err error) {
	manifest, ok := b.files[ManifestFile]
	if !ok {
		if b.seen == 0 {
			return nil, "", nil, errNotABundle
		}
		return nil, "", nil, invalidBundle("the bundle holds no %s, so nothing in it "+
			"says what the module is", ManifestFile)
	}
	if len(b.others) != 1 {
		return nil, "", nil, invalidBundle("the bundle holds %s beside %s, and an "+
			"add-on bundle holds exactly one module",
			plural(len(b.others), "file"), ManifestFile)
	}
	return manifest, b.others[0], b.files[b.others[0]], nil
}

// readTar walks an uncompressed tar — either the fetched bytes themselves, or
// what [inflate] produced from them, which is why this takes a slice rather than
// a stream.
func readTar(raw []byte) (manifest []byte, moduleName string, module []byte, err error) {
	tr := tar.NewReader(bytes.NewReader(raw))
	members := newBundleMembers()
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", nil, errNotABundle
		}
		if err := members.header(h.Name, h.Typeflag == tar.TypeReg); err != nil {
			return nil, "", nil, err
		}
		if err := members.read(h.Name, tr); err != nil {
			return nil, "", nil, err
		}
	}
	return members.result()
}

// readZip walks a zip, and it carries the refusals the other two do not need.
//
// **Zip has the largest parser surface at this door and gets the most specific
// refusals**, which is [D384]'s instruction. Three things a tar cannot express
// are refused here by name: a member stored with a compression method this
// product does not implement, a member the central directory describes as
// anything other than a plain file, and — because a zip's entries are a list
// rather than a stream — two members under one name. The duplicate is the
// dangerous one: which of the two any given reader returns is a property of the
// reader, so an archive holding both a benign `addon.json` and a hostile one is
// an archive that means different things to different tools.
//
// The declared uncompressed sizes are checked against [bundleInflatedLimit] —
// the same figure the gzip path stops its read at — before anything is inflated,
// because they are free to read and refuse a bomb at its central directory. That
// is what *one bound at each format's earliest opportunity* means here: this
// reader never has to inflate to find out, so the cheapness the ratio buys the
// gzip path is a property this path had already. They
// are **not** trusted afterwards: [bundleMembers.read] bounds what actually
// arrives, since a declared size is a number the archive's author chose — though
// in practice `archive/zip` refuses a member that outruns its own declaration
// before that backstop is reached, which is what makes the cheap check a bound
// rather than a hint.
//
// **What accepting zip costs, stated rather than discovered**: `zip.NewReader`
// parses the entire central directory before any of the above runs, so a
// 32-mebibyte archive of empty entries is a large allocation this reader never
// gets a say in. The answer to that is the ordering in install_url.go and not
// anything here — the bytes have to hash to what the operator typed before a
// parser is pointed at them — which is the same answer the tar and gzip readers
// rely on and the reason the digest check comes first.
func readZip(raw []byte) (manifest []byte, moduleName string, module []byte, err error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, "", nil, errNotABundle
	}
	limit := bundleInflatedLimit(int64(len(raw)))
	var declared uint64
	for _, f := range zr.File {
		// Written as a subtraction rather than as a sum compared afterwards, because
		// a central directory is a number an attacker chose: two entries declaring
		// most of 2^64 each add up to something small, and a wrapped total is a total
		// that passes. `declared` is never above the limit, so the subtraction cannot
		// underflow.
		//nolint:gosec // G115: limit is between the floor and the cap, both positive.
		if f.UncompressedSize64 > uint64(limit)-declared {
			return nil, "", nil, overInflated(limit, zipSaysUnpacksTo)
		}
		declared += f.UncompressedSize64
	}

	members := newBundleMembers()
	for _, f := range zr.File {
		// [fs.FileMode.IsRegular] is what refuses a directory, a symlink and a
		// device in one test: it is true only when no type bit is set, and a zip
		// entry written without external attributes has none.
		if err := members.header(f.Name, f.Mode().IsRegular()); err != nil {
			return nil, "", nil, err
		}
		if f.Method != zip.Store && f.Method != zip.Deflate {
			// Registering a decompressor for anything else would be adding a
			// dependency to satisfy an archive nobody has asked to publish. Stored and
			// deflated is what every zip tool writes by default.
			return nil, "", nil, invalidBundle("%q inside the bundle is compressed with "+
				"method %d, and an add-on bundle's entries are stored or deflated",
				safeName(f.Name), f.Method)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, "", nil, errNotABundle
		}
		err = members.read(f.Name, rc)
		_ = rc.Close()
		if err != nil {
			return nil, "", nil, err
		}
	}
	return members.result()
}

// isBareFilename is the same shape [Manifest.Validate] requires of the module it
// names: a filename, resolved inside the add-on's own directory, and not a path.
func isBareFilename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch name[i] {
		case '/', '\\', 0:
			return false
		}
	}
	return true
}

// safeName is an archive member's name on its way into a refusal an operator
// reads.
//
// The refusals above are [domain.ValidationErrors] messages and reach the API
// verbatim, and an archive written by somebody else can call a member anything at
// all — including a kilobyte of control characters. Bounded and neutralized here
// rather than at each site, and the dashboard never renders any of them anyway:
// that surface words its own sentence from the code and reads nothing out of a
// bundle (see internal/httpx/web_addons.go).
func safeName(name string) string {
	const most = 64
	if len(name) > most {
		name = name[:most] + "…"
	}
	return moduleText(name)
}

// plural writes "1 file" or "3 files", for the one refusal above that counts.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
