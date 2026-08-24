package addon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tetratelabs/wazero/api"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
	"github.com/DevOfPie/LinkCtrl/internal/observability"
	"github.com/DevOfPie/LinkCtrl/internal/store"
)

// This file is the host half of the ABI. The guest half is the generated SDK in
// sdk/, and neither of them enumerates the functions: both derive from
// abi.Functions, which is the whole point of that slice existing.
//
// # What is live, and what is declared
//
// Which functions work here is abi.Functions' Live field and not this comment's to
// count — it has been wrong once already. What is left refused is registered all
// the same, and answers abi.StatusNotAvailable to an add-on that holds the
// permission it costs and abi.StatusDenied to one that does not. Which of the two
// comes first is the next section's, and it is a decision rather than an accident.
// That the remainder is refused rather than absent is m61.md's requirement, not a
// shortcut: the contract has to be complete on paper before it is complete in
// behaviour, because the add-on repository compiles against it from its first
// commit and cannot wait for six milestones. The refusal is a status a module
// branches on rather than a link failure it cannot, and implementing a refused
// function is explicitly not a breaking change — docs/addon-abi.md says so.
//
// The pairing is checked rather than trusted: registerABI panics if a function
// abi.Functions marks Live has no implementation here, or if one it does not mark
// Live has one. A milestone that implements a limb and forgets to flip the flag
// therefore fails at construction, in every test that opens a host, instead of
// shipping a function the documentation calls refused.
//
// # What a call costs
//
// Every function names the permission it costs in abi.Function.Requires, and
// dispatch refuses a call whose grant the calling add-on did not declare —
// **before** it refuses one this host has not implemented, so a module that
// declared nothing cannot probe for the limbs a host has. Grants are resolved once
// at load (grants.go) because from M66 this check sits on the redirect path. Two
// functions cost nothing: abi_version reports a constant, and log is the one
// capability granted on purpose, since a module's stdout and stderr are discarded
// and it has no other way out.
//
// # How one host module serves many add-ons
//
// wazero resolves imports by module name from the runtime's registry, so there is
// exactly one "linkctrl" module per runtime and every add-on imports the same
// one. Scoping is per *call*: api.GoModuleFunction hands the implementation the
// **calling** module, whose name is the add-on's — Open sets it from the manifest
// — and hostState turns that into the manifest and logger for the add-on that
// called. The state is registered before instantiation because package
// initialization runs *during* it, which is the one window where a module can
// call a host function before Open has finished with it.

// maxStringIn bounds a single value crossing from a guest into the host.
//
// 64 KiB, and it is a liveness bound rather than a validation rule. A module can
// address up to wazero's memory limit, so a log call naming a length of a
// gigabyte is a gigabyte copied into host memory on the host's own heap, once per
// call, from code an operator did not write. Nothing in this ABI has a legitimate
// argument that large: the largest is a SQL statement.
const maxStringIn = 64 << 10

// maxRandomBytes bounds one draw from random_bytes.
//
// 4 KiB, which is three orders of magnitude more than anything this function
// exists for — a 32-byte nonce, a 32-byte `state`, a 96-byte PKCE verifier — and
// small enough that a module looping on it is asking the host for work rather than
// for memory. It is not maxStringIn: that bound protects the host's heap from a
// length the *guest* chose, and this one is a policy about what a random number is.
const maxRandomBytes = 4096

// hostFunc is one live ABI function. It reads its arguments off the wasm stack —
// the layout is abi.Function.Params, expanded by hostSignature — and returns what
// the caller gets: a length, or one of abi.Statuses.
type hostFunc func(ctx context.Context, st *hostState, mod api.Module, stack []uint64) int32

// hostFuncs is every function this host implements. A name here that
// abi.Functions does not mark Live, or a Live one missing from here, is a
// programming error registerABI refuses to build past.
var hostFuncs = map[string]hostFunc{
	"abi_version": func(_ context.Context, _ *hostState, mod api.Module, stack []uint64) int32 {
		return writeOut(mod, stack[0], stack[1], []byte(abi.Version))
	},

	"log": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		level, ok := readString(mod, stack[0], stack[1])
		if !ok {
			return int32(abi.StatusInvalid)
		}
		message, ok := readString(mod, stack[2], stack[3])
		if !ok {
			return int32(abi.StatusInvalid)
		}
		if !slices.Contains(abi.LogLevels, level) {
			// Not defaulted to info. A level nobody spelled correctly becomes a
			// line nobody greps for, and the add-on's author is the one person who
			// can fix it.
			return int32(abi.StatusInvalid)
		}
		// Handed over as the module wrote it, and neutralized by the handler st.log
		// is built on — see logsafe.go. Escaping it here as well would double every
		// backslash twice, which is why the boundary is one place and not two. The
		// level needs none of it: it is compared against a closed vocabulary one line
		// above rather than passed through.
		st.log.Log(context.Background(), slogLevels[level], message)
		return 0
	},

	// The two host facts M65 turned on, and the two that cost nothing (D292).
	//
	// Neither answer is scoped to the caller: what comes back is a property of the
	// machine, identical for every add-on, and the state is reached only to say
	// whose call the host was answering when its own random source failed. They are
	// in the ABI so that
	// value has a documented shape a publisher can rely on — not because the
	// capability is rationed. A guest reaches the same two sources through
	// crypto/rand and time.Now, because guestModuleConfig wires WASI's random_get
	// and clock_time_get to them.
	"random_bytes": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		n := api.DecodeI32(stack[0])
		if n < 1 || n > maxRandomBytes {
			// Refused rather than clamped, in both directions. A caller that asked
			// for zero bytes of entropy has a bug that a zero-length success would
			// hide, and one that asked for a megabyte wanted a stream this ABI does
			// not carry — a value crosses in one piece, and the answer to "how much
			// randomness" is never "as much as fits".
			return int32(abi.StatusInvalid)
		}
		buf := make([]byte, n)
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand.Read does not fail on any platform this product runs on;
			// it is checked because the one thing worse than no entropy is a buffer
			// of zeroes that reports success.
			st.hostLog.Error("the host could not read from its random source",
				slog.String("addon", st.manifest.Name),
				slog.Any("error", err))
			return int32(abi.StatusInternal)
		}
		return writeOut(mod, stack[1], stack[2], buf)
	},

	"time_now": func(_ context.Context, _ *hostState, mod api.Module, stack []uint64) int32 {
		// UTC and RFC 3339 with nanoseconds: one spelling to parse, no zone for a
		// guest to guess at, and the same format the RedirectEvent record's
		// occurred_at already promises.
		return writeOut(mod, stack[0], stack[1], []byte(time.Now().UTC().Format(time.RFC3339Nano)))
	},

	"config_get": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		key, ok := readString(mod, stack[0], stack[1])
		if !ok {
			return int32(abi.StatusInvalid)
		}
		s, declared := st.settings[key]
		if !declared {
			// Denied rather than not-found, and the difference is the whole of the
			// scoping: an undeclared key is not a value that happens to be absent,
			// it is a value this add-on has no standing to ask for. A module cannot
			// probe for another add-on's settings or for this product's own
			// configuration, because neither is in its manifest.
			//
			// The second of two questions, and dispatch has already answered the
			// first: `config.read` is whether this add-on may read settings at all,
			// and this is whether the key is one of its own. Both answer Denied and
			// only the first is a permission, which is why only the first is
			// counted in the refusals metric.
			return int32(abi.StatusDenied)
		}
		if v, configured := st.values[key]; configured {
			// What an operator set, which outranks the manifest's default for the
			// reason every other value in this product does: the declaration is the
			// publisher's answer and the environment is the operator's.
			return writeOut(mod, stack[2], stack[3], []byte(v.Reveal()))
		}
		if s.Default == "" {
			// A declared setting with nothing behind it yet. A secret nobody has
			// configured is here by construction — a manifest may not give one a
			// default — and so is any setting whose value the Add-on manager will
			// supply.
			return int32(abi.StatusNotFound)
		}
		return writeOut(mod, stack[2], stack[3], []byte(s.Default))
	},

	// The two storage functions (M63). They share every line of their argument
	// handling and differ in one thing that matters: a query runs in a READ ONLY
	// transaction and an exec does not, so which of the pair a module called is a
	// fact Postgres enforces rather than a description of intent.
	"storage_query": func(ctx context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		statement, args, status := readStatement(mod, stack)
		if status != 0 {
			return status
		}
		if st.storage == nil {
			return st.noStorage("storage_query")
		}
		rows, err := st.storage.Query(ctx, statement, args)
		if err != nil {
			return st.storageFailed("storage_query", err)
		}
		return writeOut(mod, stack[4], stack[5], rows)
	},

	"storage_exec": func(ctx context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		statement, args, status := readStatement(mod, stack)
		if status != 0 {
			return status
		}
		if st.storage == nil {
			return st.noStorage("storage_exec")
		}
		if err := st.storage.Exec(ctx, statement, args); err != nil {
			return st.storageFailed("storage_exec", err)
		}
		// Zero, not a row count. The ABI's convention is that a non-negative answer
		// is a length, and this function has no out parameter for one to be the
		// length of; inventing a meaning for the number here would be a second
		// convention for one i32.
		return 0
	},

	// The three functions M64 turned on. Each is answered against the *request's*
	// state, which is a per-request instance's own — see Host.Route — so "outside
	// a request" is not a flag to check but a state that cannot be faked: the
	// load-time instance has no request, and neither does another request's.
	"http_request_read": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		if st.request == nil {
			// What a module calling this from package initialization gets, which is
			// every module: initialization runs during instantiation, and the request
			// is attached to the state before that. NotFound rather than Invalid — the
			// call is well formed and there is simply nothing to read.
			return int32(abi.StatusNotFound)
		}
		encoded, err := st.encodedRequest()
		if err != nil {
			return st.marshalFailed("http_request_read", err)
		}
		return writeOut(mod, stack[0], stack[1], encoded)
	},

	"http_response_write": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		raw, ok := readBytes(mod, stack[0], stack[1])
		if !ok {
			return int32(abi.StatusInvalid)
		}
		if st.request == nil {
			return int32(abi.StatusNotFound)
		}
		if st.response != nil {
			// A response is one record, not a stream. Refused rather than replacing
			// the first one: a module that wrote twice does not know which of its two
			// answers the visitor got, and neither would its author.
			return int32(abi.StatusInvalid)
		}
		resp, err := decodeResponse(raw, st.manifest.CookiePrefixes)
		if err != nil {
			// Debug, and the reason never crosses: the guest gets StatusInvalid and
			// the detail goes where an operator can read it. A module looping on a
			// malformed record would otherwise decide how much an instance logs.
			// decodeResponse names what it refused and what it refused is the
			// module's — a header name, a cookie name, a status — and hostLog is a
			// neutralizing logger, so the raw error is what goes in.
			st.hostLog.Debug("refused an add-on's response",
				slog.String("addon", st.manifest.Name),
				slog.Any("error", err))
			return int32(abi.StatusInvalid)
		}
		st.response = &resp
		return 0
	},

	"identity_link": func(ctx context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		raw, ok := readBytes(mod, stack[0], stack[1])
		if !ok {
			return int32(abi.StatusInvalid)
		}
		if st.request == nil {
			return int32(abi.StatusNotFound)
		}
		if st.minter == nil {
			st.hostLog.Debug("an add-on called identity_link and this host has no session service",
				slog.String("addon", st.manifest.Name))
			return int32(abi.StatusInternal)
		}
		claim, err := decodeClaim(raw)
		if err != nil {
			st.hostLog.Debug("refused an add-on's link claim",
				slog.String("addon", st.manifest.Name),
				slog.Any("error", err))
			return int32(abi.StatusInvalid)
		}
		// The **actor is the host's**, resolved from the request's own session and
		// never from anything the module wrote. That is the whole of "linking is
		// explicit": a module names a subject and the host names the account, so
		// there is no shape in which a module links a subject to somebody it chose.
		if err := st.minter.LinkAddonIdentity(ctx, st.identity, st.manifest.Name,
			claim.Issuer, claim.Subject); err != nil {
			return st.linkRefused(claim, err)
		}
		return 0
	},

	"session_mint": func(ctx context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		raw, ok := readBytes(mod, stack[0], stack[1])
		if !ok {
			return int32(abi.StatusInvalid)
		}
		if st.request == nil {
			// Outside a request, which is where package initialization always is.
			// NotFound rather than Invalid, and the same answer the M64 functions give
			// from there: a mint with no visitor in front of it is a mint for nobody.
			return int32(abi.StatusNotFound)
		}
		if st.minted != nil {
			// One mint per request. Refused rather than replacing the first, for the
			// reason http_response_write refuses a second response: a module that
			// minted twice does not know which of the two the browser is holding, and
			// neither would its author.
			return int32(abi.StatusInvalid)
		}
		if st.minter == nil {
			// A host built without an auth service, which in this product is a test
			// and never an instance. Said once per call at debug, the way noStorage
			// does, because a module branching on the status deserves the reason to be
			// findable.
			st.hostLog.Debug("an add-on called session_mint and this host has no session service",
				slog.String("addon", st.manifest.Name))
			return int32(abi.StatusInternal)
		}
		claim, err := decodeClaim(raw)
		if err != nil {
			// Debug, and the reason never crosses: the guest gets a status and the
			// detail goes where an operator can read it. decodeClaim names what it
			// refused and what it refused is the module's, so the neutralizing logger
			// is what makes the raw error safe to write.
			st.hostLog.Debug("refused an add-on's session claim",
				slog.String("addon", st.manifest.Name),
				slog.Any("error", err))
			return int32(abi.StatusInvalid)
		}
		// **The buffer is checked before anything is minted.** Every other function
		// on this ABI can answer a short buffer with a size and let the guest retry,
		// because a second read costs nothing; this one has a side effect, so the
		// convention's *nothing was written* has to mean nothing happened. Without
		// this the SDK's own retry loop re-enters, meets the one-mint guard above,
		// and is told ErrInvalid while the host has minted and is about to set the
		// cookie — the module and the host disagreeing about who is signed in.
		//
		// The record's own size is not known until after the mint, so what is
		// required is its maximum. See mintedSessionMaxBytes for the arithmetic and
		// TestAMintedSessionFitsItsPublishedBound for what holds it.
		if int(api.DecodeU32(stack[3])) < mintedSessionMaxBytes {
			return mintedSessionMaxBytes
		}
		mint, err := st.minter.MintFromAddonAssertion(ctx, auth.AddonAssertion{
			// The add-on's name comes from the *host's* record of which module is
			// calling, not from anything in the claim. A module cannot assert in
			// another add-on's name, which is what makes `addon` safe as a key column.
			Addon:                 st.manifest.Name,
			Issuer:                claim.Issuer,
			Subject:               claim.Subject,
			Email:                 claim.Email,
			DisplayName:           claim.DisplayName,
			EmailVerified:         claim.EmailVerified,
			Groups:                claim.Groups,
			AlreadySignedIn:       st.identity != nil,
			SatisfiesSecondFactor: st.mfaSatisfied,
			IP:                    st.clientIP,
			UserAgent:             st.userAgent,
		})
		if err != nil {
			return st.mintRefused(claim, err)
		}
		out := Minted{
			ExpiresAt:            mint.ExpiresAt,
			SecondFactorRequired: mint.SecondFactorRequired(),
		}
		if mint.Pending != nil {
			out.PendingToken = config.Secret(mint.Pending.Token)
		} else {
			out.Token = config.Secret(mint.Login.Token)
		}
		encoded, err := json.Marshal(MintedSession{
			ExpiresAt:            out.ExpiresAt.UTC().Format(time.RFC3339),
			SecondFactorRequired: out.SecondFactorRequired,
		})
		if err != nil {
			return st.marshalFailed("session_mint", err)
		}
		if len(encoded) > mintedSessionMaxBytes {
			// Unreachable: the record has one shape and the bound is that shape at its
			// widest with slack on top. Checked because the alternative to checking it
			// is the defect the pre-mint check above removes, arriving from the other
			// end — a record that does not fit, on a call that has already minted. The
			// session is recorded, because it exists and hiding it would sign somebody
			// in with nothing carrying the fact; the guest gets a status it can branch
			// on rather than a size whose retry would now be refused.
			st.minted = &out
			return st.marshalFailed("session_mint",
				fmt.Errorf("a minted session encoded to %d bytes against a bound of %d",
					len(encoded), mintedSessionMaxBytes))
		}
		// Recorded **before** the record is written back, and the write can no longer
		// fail for want of room: the capacity was checked before the mint. What is
		// left is a guest whose pointer does not address its own memory, and that is a
		// module the host cannot answer at all — the session row exists either way,
		// so whether somebody is signed in must not depend on how a module sized or
		// placed a slice.
		st.minted = &out
		// **`hostLog`, not `log`.** This is the host saying it minted a session, and
		// `st.log` stamps `source=addon` — so the one security-relevant record on
		// this boundary would be attributed to the party it is a record *about*, and
		// a module holding only the ungated `log` could emit a byte-identical line.
		st.hostLog.Info("minted a session on this add-on's assertion",
			slog.String("addon", st.manifest.Name),
			slog.String("issuer", claim.Issuer),
			slog.Bool("second_factor_required", out.SecondFactorRequired))
		return writeOut(mod, stack[2], stack[3], encoded)
	},

	"session_context": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		if st.request == nil {
			return int32(abi.StatusNotFound)
		}
		encoded, err := json.Marshal(st.session)
		if err != nil {
			return st.marshalFailed("session_context", err)
		}
		return writeOut(mod, stack[0], stack[1], encoded)
	},

	// The redirect limb (M66). Two classes, three functions, and the class an
	// invocation is in is a property of its state rather than an argument — so
	// "outside a redirect" needs no flag to check and cannot be faked by a module
	// calling the wrong one.

	"redirect_event_read": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		if st.event == nil {
			// Package initialization, a route's instance, or an inline invocation
			// reaching for the other class's payload — though the last of those is
			// refused before it arrives here, because redirect_event_read is not in
			// abi.InlineSafe. NotFound for the reason http_request_read gives: the
			// call is well formed and there is nothing to read.
			return int32(abi.StatusNotFound)
		}
		encoded, err := st.encodedRedirect()
		if err != nil {
			return st.marshalFailed("redirect_event_read", err)
		}
		return writeOut(mod, stack[0], stack[1], encoded)
	},

	"redirect_decision_read": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		if st.decision == nil {
			return int32(abi.StatusNotFound)
		}
		encoded, err := st.encodedRedirect()
		if err != nil {
			return st.marshalFailed("redirect_decision_read", err)
		}
		return writeOut(mod, stack[0], stack[1], encoded)
	},

	"redirect_answer_write": func(_ context.Context, st *hostState, mod api.Module, stack []uint64) int32 {
		raw, ok := readBytes(mod, stack[0], stack[1])
		if !ok {
			return int32(abi.StatusInvalid)
		}
		if st.decision == nil {
			return int32(abi.StatusNotFound)
		}
		if st.answer != nil {
			// One answer per invocation, refused rather than replacing the first —
			// the reason http_response_write gives, and it is sharper here: a module
			// that vetoed and then allowed does not know which the visitor got.
			return int32(abi.StatusInvalid)
		}
		answer, err := decodeRedirectAnswer(raw)
		if err != nil {
			// Debug, and the reason never crosses. A module looping on a malformed
			// record would otherwise decide how much an instance logs, on the one
			// path in this product where that is a latency question as well as a
			// disk one.
			st.hostLog.Debug("refused an add-on's redirect answer",
				slog.String("addon", st.manifest.Name),
				slog.Any("error", err))
			return int32(abi.StatusInvalid)
		}
		if answer.Rewrite && !st.grants.Has(PermissionRewriteQuery) {
			// The second grant, checked here rather than in dispatch, because
			// dispatch checks what a *function* costs and this is what one field of
			// one record costs. D317: holding redirect.inline buys watching and
			// refusing, and editing the destination is a token of its own.
			//
			// Counted on the same series an undeclared call is, and for the same
			// reason: it is an add-on reaching past what its manifest declared, and
			// an operator asking "is anything being refused" should get one answer.
			st.metrics.ObserveAddonRefusal(st.manifest.Name, PermissionRewriteQuery)
			st.hostLog.Debug("refused an add-on's query rewrite: it did not declare the "+
				"permission that a rewrite costs",
				slog.String("addon", st.manifest.Name),
				slog.String("permission", PermissionRewriteQuery))
			return int32(abi.StatusDenied)
		}
		st.answer = &answer
		return 0
	},
}

// readStatement reads the (sql, args) pair both storage functions begin with.
//
// The layout is abi.Function.Params expanded by hostSignature: the statement is
// stack[0..1] and the JSON argument array is stack[2..3], which is the same for
// both because both declare the same first two parameters. A test holds that
// sentence to the ABI rather than leaving it as a comment.
func readStatement(mod api.Module, stack []uint64) (string, []any, int32) {
	statement, ok := readString(mod, stack[0], stack[1])
	if !ok {
		return "", nil, int32(abi.StatusInvalid)
	}

	if statement == "" {
		return "", nil, int32(abi.StatusInvalid)
	}
	raw, ok := readBytes(mod, stack[2], stack[3])
	if !ok {
		return "", nil, int32(abi.StatusInvalid)
	}
	args, err := store.DecodeAddonArgs(raw)
	if err != nil {
		// The guest's fault and the guest's to fix, so nothing is logged: an add-on
		// looping on a malformed argument list would otherwise decide how much an
		// instance logs.
		return "", nil, int32(abi.StatusInvalid)
	}
	return statement, args, 0
}

// noStorage is the answer when this add-on holds the grant and the host has no
// database to honour it with — a host constructed without one, which in this
// product is a test and never an instance. See Host.openStorage, which has already
// said so once at load; this says it again per call at debug, because a module
// branching on the status deserves the reason to be findable.
func (s *hostState) noStorage(function string) int32 {
	s.hostLog.Debug("an add-on called a storage function and this host has no database",
		slog.String("addon", s.manifest.Name),
		slog.String("function", function))
	return int32(abi.StatusInternal)
}

// storageFailed turns a database error into the status the guest gets, and puts the
// detail where an operator can read it.
//
// **The message never crosses**, which is the same rule StatusInternal is
// documented with: a Postgres error names tables, columns and constraints, and an
// add-on that can read one can print this product's schema into somebody's page.
// What the guest gets is a number it can branch on.
//
// Denied is separated from Invalid on purpose. A privilege refusal is confinement
// working, and it is the one failure the add-on's author cannot fix by editing
// their statement — telling them apart is what lets a module report "that is not
// mine to read" rather than "your SQL is wrong". It costs the module nothing it did
// not already know: it knows which schema it owns.
func (s *hostState) storageFailed(function string, err error) int32 {
	status := abi.StatusInvalid
	level := slog.LevelDebug
	if errors.Is(err, store.ErrAddonDenied) {
		status = abi.StatusDenied
		// Warned rather than debugged, and this is the one storage failure worth
		// waking somebody for: a module reaching outside its schema is either a bug
		// its author has not noticed or an attempt. Either way an operator wants to
		// know, and it is bounded — a module that keeps trying is a module whose log
		// volume is already the smaller problem.
		level = slog.LevelWarn
	}
	// A Postgres error quotes the fragment of the statement it failed on, and the
	// statement is the module's. Logged raw and neutralized by the handler; errors.Is
	// above is asked of the error itself and so does not depend on any of it.
	s.hostLog.Log(context.Background(), level,
		"an add-on's statement failed",
		slog.String("addon", s.manifest.Name),
		slog.String("function", function),
		slog.Any("error", err))
	return int32(status)
}

// linkRefused turns a refusal from the linking flow into the guest's status.
//
// Two refusals and a fallback, and the two are told apart because a module does
// different things with them: "nobody is signed in" is a flow that began in the
// wrong place and is answered by sending the person to sign in first, while "that
// identity belongs to somebody else" is a dead end the person has to resolve on
// the other account. Neither names the other account, and neither says whether the
// subject was linked at all when nobody is signed in — the signed-in check comes
// first, so an anonymous browser cannot ask.
func (s *hostState) linkRefused(claim SessionClaim, err error) int32 {
	status, level := abi.StatusInternal, slog.LevelError
	switch {
	case errors.Is(err, auth.ErrAssertionIncomplete):
		status, level = abi.StatusInvalid, slog.LevelDebug
	case errors.Is(err, domain.ErrUnauthorized), errors.Is(err, domain.ErrForbidden):
		status, level = abi.StatusDenied, slog.LevelDebug
	case errors.Is(err, auth.ErrSubjectLinkedElsewhere):
		// Warned. One external identity being offered to a second account is either
		// a person with two accounts or an attempt to take one over, and an operator
		// is the only one who can tell which.
		status, level = abi.StatusDenied, slog.LevelWarn
	}
	s.hostLog.Log(context.Background(), level,
		"refused an add-on's request to connect an external identity",
		slog.String("addon", s.manifest.Name),
		slog.String("issuer", claim.Issuer),
		slog.Any("error", err))
	return int32(status)
}

// mintRefused turns a refusal from the session service into the status the guest
// gets, and puts the reason where an operator can read it.
//
// **The four refusals are distinguishable to the guest**, and that is a decision
// rather than a leak. An add-on has to be able to tell "nobody has connected this
// identity" — the ordinary first visit, which it answers with a linking page —
// from "that account cannot sign in", which it cannot do anything about. What it
// never learns is *which* account, or whether one exists at all beyond the link:
// an unlinked subject and a subject linked to a locked account are two statuses,
// and neither names anybody. That is a narrower disclosure than the password
// form's, which m65.md's own boundary makes safe: only a module holding
// `session.mint` can ask, and an operator installed it deliberately.
func (s *hostState) mintRefused(claim SessionClaim, err error) int32 {
	status := abi.StatusInternal
	level := slog.LevelError
	switch {
	case errors.Is(err, auth.ErrAssertionIncomplete):
		status, level = abi.StatusInvalid, slog.LevelDebug
	case errors.Is(err, auth.ErrSubjectNotLinked):
		// Debug, and this is the one refusal that is *ordinary*: every first visit
		// by somebody who has not connected a provider looks exactly like this.
		status, level = abi.StatusNotFound, slog.LevelDebug
	case errors.Is(err, auth.ErrAccountLocked), errors.Is(err, auth.ErrAccountInactive):
		// Warned. An external provider vouching for an account this instance has
		// locked or disabled is the event an operator wants to see, and it is
		// bounded — one line per attempt, and an attempt needs a visitor.
		status, level = abi.StatusDenied, slog.LevelWarn
	case errors.Is(err, auth.ErrAlreadySignedIn):
		status, level = abi.StatusDenied, slog.LevelWarn
	}
	s.hostLog.Log(context.Background(), level,
		"refused an add-on's session claim",
		slog.String("addon", s.manifest.Name),
		// The issuer is the module's own string and is neutralized by the handler.
		// The subject is **not** logged: it identifies a person at an external
		// provider, and a log is not a place this product puts one.
		slog.String("issuer", claim.Issuer),
		slog.Any("error", err))
	return int32(status)
}

// slogLevels maps the ABI's level vocabulary onto slog's. Keyed off
// abi.LogLevels by a test, so a level added there without a mapping here fails
// rather than logging at debug.
var slogLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// maxLogMessage bounds the line one log call can put in front of a reader.
//
// Not the same bound as maxStringIn, which protects this process's heap: 64 KiB of
// escaped control characters is a record nobody reads, and a module writing one per
// call is a denial of service against whoever has to read the log rather than
// against the host. 4 KiB is longer than any message with something to say and
// short enough that a truncated one is still greppable. The bound is on what gets
// written, after escaping, so it holds whatever the message was made of.
const maxLogMessage = 4 << 10

// logTruncated is what a reader sees in place of the rest. Present or absent, it
// answers the question a bounded line otherwise leaves open — whether the message
// ended or the host stopped copying it.
//
// It carries a backslash, and that is the whole of what makes it the host's own
// (D244). Every backslash a module writes is doubled by escapeLogRune, so a lone one
// in a written line can only have come from this file: a module ending its message
// with `…(truncated)` produces exactly those characters and reads as a message that
// was cut, while one ending it with `…\(truncated)` reads as `…\\(truncated)`. The
// mark is a claim the host makes about its own copying, so a module has to be unable
// to make it.
const logTruncated = `…\(truncated)`

// sanitizeLogMessage is one log record's worth of neutralized text: the escaping,
// plus the bound that belongs to a log line and to nothing else.
//
// It is what neutralizingHandler applies to every message and every attribute this
// subsystem logs — **not** what a call site applies to its own argument. That
// inversion is D286: the rule *neutralize module text where you log it* was written
// down three times and enumerated wrongly twice, most recently missing a migration
// filename on store.MigrateAddon's success path, in another package entirely.
// logsafe.go is where the property lives now.
//
// D240's requirement, and the reason it is a boundary at all: log is ungated, so
// every loaded module can reach it including one that declared no permission at all,
// which makes it the widest untrusted input this host has — and there is no second
// boundary between here and an operator's screen. Two harms follow, and a permission
// check would have stopped neither. A message carrying a newline can close the
// host's own record and open one that reads as the host's, and log records are what
// an operator reasons from when something has gone wrong. A message carrying an ANSI
// escape, a zero-width character or a bidi override can put bytes in front of a
// reader arranged to be overlooked.
//
// Escaped rather than dropped, because what a module tried to write is evidence: a
// reader who sees \n knows more than one handed a message with a hole in it. Not
// delegated to slog either, though both handlers NewLogger builds do quote what
// they write: which handler an operator configured is not something this boundary
// may depend on, and neither of them bounds a length.
//
// **One class is dropped, and it is the exception that names its own reason** — see
// strippedRune. Evidence is why everything else is escaped; a variation selector is
// evidence of nothing, since it has no appearance of its own to report and every
// spelling of one costs a reader the emoji it was riding on (D283).
//
// **What this function does not do is bounded by something outside it, and that is
// deliberate** (D285). It neutralizes a published Unicode property and not every
// character that renders as nothing, because no property names that set — see
// escapedRune for what is left over. A covert channel needs an end to write bits and
// an end to read them back. The write end is open by construction, since log costs no
// permission. **The read end is closed**: log takes a level and a message and declares
// no out-parameter, no ABI function hands log content back, a guest gets no preopened
// file and its stdout and stderr are discarded, and an add-on's storage is a schema of
// its own that this log does not live in. So the residue only pays out if an operator
// hands the log to the add-on's author, which is a person's decision rather than a
// capability this host grants. TestAnAddonPostsToTheLogAndCannotReadItBack is that
// property as assertions, over every function in the ABI rather than over log alone.
func sanitizeLogMessage(s string) string { return escapeModuleText(s, maxLogMessage) }

// unbounded is what escapeModuleText is given when the text is not going into a log
// line. See moduleText for why that is a case at all.
const unbounded = 0

// escapeModuleText is the neutralization itself, with the bound as a parameter
// rather than as part of it.
//
// **The two were one function and F-5 is what that cost** (D286). A manifest error
// is not a log line: Manifest.Validate aggregates with errors.Join precisely so that
// somebody publishing an add-on for the first time sees every problem at once, and
// running that list through the log line's 4 KiB cap cut it silently at the point a
// long list becomes worth reading. The escaping is a claim about what a reader may
// be shown; the cap is a claim about how much of an operator's log one call may
// occupy. Only the second belongs to a logger.
func escapeModuleText(s string, max int) string {
	var b strings.Builder
	// Sized to what will be written, not to what arrived: on the log path the input may
	// be maxStringIn and the output cannot exceed maxLogMessage, and Builder.String does
	// not copy — so growing to the input would leave a 4 KiB line holding a 64 KiB array
	// for as long as the log record lives.
	if max > unbounded {
		b.Grow(min(len(s), max))
	} else {
		b.Grow(len(s))
	}
	// The mark is reserved rather than appended over the bound, so max is the size of
	// what gets written and not the size of most of it.
	limit := max - len(logTruncated)
	for _, r := range s {
		if strippedRune(r) {
			// Deleted rather than escaped, and it is the only class handled that way
			// (D283). Nothing is written, so nothing is counted against the limit either.
			continue
		}
		esc := escapeLogRune(r)
		n := len(esc)
		if esc == "" {
			n = utf8.RuneLen(r)
		}
		if max > unbounded && b.Len()+n > limit {
			b.WriteString(logTruncated)
			break
		}
		if esc == "" {
			// WriteRune, so a byte sequence that was not valid UTF-8 becomes the
			// replacement character rather than reaching the log as itself. readString
			// has already refused one, and this function is also called on its own.
			b.WriteRune(r)
			continue
		}
		b.WriteString(esc)
	}
	return b.String()
}

// foldToLogLine puts already-neutralized text into one log record: it folds the
// newlines the host's own aggregation put there and applies the log line's bound.
//
// Nothing here escapes anything, and that is the point — the text has been through
// escapeModuleText already and a second pass would double every backslash a second
// time. What is left to do is the part that belongs to the log and not to the
// neutralization: an error carrying a joined list is many lines for an operator
// reading a fatal message and must be one record for an operator reading a log.
//
// The `\n` it writes is a lone backslash, which is the same signature logTruncated
// relies on: every backslash a module writes is doubled by escapeLogRune, so a lone
// one in a written line can only have come from this file.
func foldToLogLine(s string) string {
	if !strings.ContainsRune(s, '\n') && len(s) <= maxLogMessage {
		return s
	}
	var b strings.Builder
	b.Grow(min(len(s), maxLogMessage))
	limit := maxLogMessage - len(logTruncated)
	for _, r := range s {
		esc := ""
		n := utf8.RuneLen(r)
		if r == '\n' {
			esc = `\n`
			n = len(esc)
		}
		if b.Len()+n > limit {
			b.WriteString(logTruncated)
			break
		}
		if esc == "" {
			b.WriteRune(r)
			continue
		}
		b.WriteString(esc)
	}
	return b.String()
}

// moduleText neutralizes a string a module supplied, for a reader who is not
// reading a log.
//
// **sanitizeLogMessage was the boundary for one function and the claim was made for
// the module** (docs/SECURITY.md). Those are not the same statement, and the gap was
// real: manifest validation embeds the offending value with `%q`, a load failure
// logs the resulting error, and `%q` escapes on unicode.IsPrint — which is true of
// every mark and every letter, so `U+3164 HANGUL FILLER`, `U+FE0F`, `U+E0100` and
// `U+2D7F` all passed through it unchanged. migrationFileRe admits every code point
// but a newline, so a filename is a 4 KiB channel with nothing between it and an
// operator's screen.
//
// The **log** half of that is logsafe.go's now. What is left here is the other
// destination: the fatal message cmd/linkctrl prints when a `required` add-on
// refuses to load, which is a page of stderr rather than a record and takes no cap
// — see escapeModuleText, and D286.
//
// Applied **once** either way: the escaping doubles a backslash, so a second
// application would double it again.
func moduleText(s string) string { return escapeModuleText(s, unbounded) }

// neutralize wraps an error whose text a module had a hand in, so that Error()
// answers the neutralized form wherever it is printed — an slog attribute, a
// wrapped error a caller formats, or the fatal message cmd/linkctrl prints when a
// `required` add-on refuses to load.
//
// Unwrap is kept, because the status a call answers with is decided by errors.Is on
// the error underneath — store.ErrAddonDenied is the one that matters — and a
// neutralization that changed which status a module got would be a different defect
// than the one it fixes.
func neutralize(err error) error {
	if err == nil {
		return nil
	}
	return moduleErr{err: err}
}

type moduleErr struct{ err error }

func (e moduleErr) Error() string { return neutralizedErrText(e.err) }
func (e moduleErr) Unwrap() error { return e.err }

// neutralized marks moduleErr as carrying text that has already been through
// escapeModuleText, so that neutralizingHandler folds and bounds it for a log record
// instead of escaping it a second time. Nothing calls it.
func (moduleErr) neutralized() {}

// neutralizedErrText neutralizes an error's text without flattening the structure
// the *host* put there.
//
// errors.Join's Error() is its branches separated by newlines, and Manifest.Validate
// aggregates with it deliberately: the person reading the output is publishing an
// add-on for the first time and should see the whole list (F-5, D286). Escaping the
// aggregate whole turned every one of those separators into the two characters `\`
// and `n`, so the list arrived as one run-on line — the host's own formatting
// destroyed by a boundary meant for the module's text.
//
// So a join is descended into and each branch neutralized on its own, and the
// separators are put back as themselves. A branch that is not a join is a leaf and
// is escaped whole, which is what keeps a newline a *module* supplied from becoming
// a separator: only the host's aggregation makes one here.
//
// **fmt.Errorf with two %w verbs also answers Unwrap() []error and is not a join**,
// which is why the shape is checked rather than the type: errors.Join's own text is
// exactly its branches joined by newlines, and fmt.Errorf's is a sentence the host
// wrote. Anything that is not that join is treated as a leaf.
func neutralizedErrText(err error) string {
	j, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return moduleText(err.Error())
	}
	branches := j.Unwrap()
	raw := make([]string, len(branches))
	safe := make([]string, len(branches))
	for i, b := range branches {
		raw[i] = b.Error()
		safe[i] = neutralizedErrText(b)
	}
	if err.Error() != strings.Join(raw, "\n") {
		return moduleText(err.Error())
	}
	return strings.Join(safe, "\n")
}

// escapeLogRune is the escape for a rune that may not reach a log line as itself,
// or "" for one that may. The ones a reader actually meets keep their familiar
// spellings; everything else becomes its code point, which is what makes an
// invisible character visible rather than merely absent.
//
// **Backslash is escaped although it is graphic, and that is what makes the escaping
// injective** (D244). Left alone, a module writing the two characters `\` and `n`
// produced a line byte-identical to the one a real newline produced, so a reader
// could not tell which had happened — and this log is meant to be read as evidence.
// Doubling it is what strconv.Quote does, for this reason. It is the one place where
// a graphic character does not reach the line as itself, and it is the reason
// logTruncated can be a mark a module cannot forge.
func escapeLogRune(r rune) string {
	switch r {
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	case '\\':
		return `\\`
	}
	if !escapedRune(r) {
		return ""
	}
	if r > 0xFFFF {
		return `\U` + hexRune(r, 8)
	}
	return `\u` + hexRune(r, 4)
}

const hexDigits = "0123456789abcdef"

func hexRune(r rune, digits int) string {
	out := make([]byte, digits)
	for i := digits - 1; i >= 0; i-- {
		out[i] = hexDigits[r&0xf]
		r >>= 4
	}
	return string(out)
}

// escapedRune reports whether a rune may not reach a log line as itself.
//
// **It is a default-deny, and that shape is the decision** (D242). The first form of
// this function enumerated the invisible code points, and an enumeration is
// permanently behind Unicode: it missed U+061C ARABIC LETTER MARK, which arrived in
// the same revision as the bidi isolates it did cover, along with U+180E, the
// interlinear annotation controls and the musical and hieroglyphic format
// characters. Three documents describe this function's set as closed, and a list a
// Unicode revision can outdate cannot keep that description true.
//
// So the test is inverted: **everything that is not a graphic character is escaped**
// — Cc, Cf, Cn, Co, Zl and Zp, which is the C0 and C1 controls, every format
// character, every unassigned code point and every private use one. Category Cf
// needs no limb of its own; no Cf code point is graphic, checked over the whole
// range rather than assumed.
//
// Three corrections sit on top, each a named case rather than a category, and each
// one a place default-deny alone answers wrongly.
//
// **This function decides between reaching the line and being escaped, and there is
// a third answer it never gives.** sanitizeLogMessage deletes a variation selector
// before asking (D283, strippedRune), so on that path this function is asked about
// one only if the strip is ever removed — and it answers *escape*, which is what
// makes the removal loud instead of silent.
//
// **What the inversion costs**, stated because it is the mirror of what the
// enumeration cost: Cn means unassigned *in the Unicode tables this Go was built
// with*, so a code point assigned after them is escaped until Go catches up. The
// enumeration's staleness was a hole a new code point walked through; this one is a
// message that reads worse for a release. Failing closed on the way Unicode moves is
// the trade.
func escapedRune(r rune) bool {
	if meaningfulFormatRune(r) {
		return false
	}
	// A Braille blank cell is a genuine graphic character and is not
	// default-ignorable, so neither limb below reaches it. What earns it a named case is
	// that it is the one blank which is not whitespace: the seventeen Zs code points
	// that survive here render as nothing too — U+2000-U+200A, U+202F, U+205F and
	// U+3000 among them, several wider than a space — but a reader knows whitespace when
	// they meet it and so does everything that trims, collapses or splits on it, while a
	// run of U+2800 is content that looks like blank. Padding is not the reason and
	// cannot be: what a line can hold is bounded by maxLogMessage whatever it is made
	// of. Braille text an add-on logs gets its spaces escaped: loud rather than silent,
	// which is the direction this whole function leans.
	if r == '\u2800' {
		return true
	}
	// Some code points are letters or marks by category — the Hangul fillers, the Khmer
	// inherent vowels, the combining grapheme joiner — and render as nothing, so
	// IsGraphic says yes where a reader sees no. Unicode has a property for exactly this
	// class, and asking Go for the table whose name most resembles it was F285.
	//
	// **It is not a property for every character that renders as nothing, and no such
	// property exists** — which is the answer four attempts at this milestone were
	// looking for. Eight combining marks the UCD annotates as "shape shown is arbitrary
	// and is not visibly rendered" sit outside it and reach a line as themselves:
	// U+2D7F, U+17D2, U+10A3F, U+1107F, U+11A47, U+11A99, U+11F42 and U+16FE4. So do
	// seventeen Zs and thirteen prepended concatenation marks. That residue is
	// conceded in writing rather than chased, it is pinned by
	// TestTheResidualClassIsWhatTheDocumentsConcede, and what bounds it is
	// sanitizeLogMessage's last paragraph: a module can write into this log and cannot
	// read out of it.
	if defaultIgnorable(r) {
		return true
	}
	return !unicode.IsGraphic(r)
}

// strippedRune reports whether a rune is deleted on its way to a log line rather
// than escaped. It is one class — Unicode's Variation_Selector, 260 code points —
// and it is the only thing this boundary removes (D283).
//
// **The threat is legibility, not confidentiality.** log is ungated, so a module may
// write a secret in plain text whenever it likes and nothing here stops it. What this
// boundary owes a reader is narrower and checkable: that what they see is what is
// there. A variation selector breaks exactly that. It is category Mn, so IsGraphic
// says yes; it has no appearance of its own; and it acts on the character before it,
// or on nothing at all when that character has no sequence registered for it. F285
// was 260 of them carrying `SECRET=hunter2` out of a line reading *everything is
// fine*, from a module that declared no permission at all.
//
// **Removing the bit does not require making logs ugly**, which is what decided the
// shape. Stripping removes it and costs the reader nothing: `❤️` becomes `❤`, still a
// heart, and `😀` is untouched because U+1F600 has emoji presentation by default and
// carries no selector to lose. Escaping would instead put `\ufe0f` through the
// middle of every emoji anybody logs.
//
// **There is no base set, and that is where two attempts at an exemption died.** The
// first asked category So: 6304 of the 6634 symbols have no registered emoji
// variation sequence, so a progress bar drawn from U+2588 and U+2591 carried the
// secret through byte-identical at log₂3 bits per cell. 6634 is the size of the
// category and is pinned by TestNoDefaultIgnorableCharacterReachesALogLine like every
// other number this file prints. **6304 is attributed rather than pinned**, and the
// difference is stated because D284 forbids a stated number with nothing under it: it
// is not computable from the tables Go ships, having been measured against the UCD's
// emoji-variation-sequences.txt on 2026-08-20, on the parked branch that vendored
// that file. It survives here as the reason an attempt failed and not as a claim
// about this boundary, which has no base set for it to be about. The second asked Unicode's
// registered sequences, read from the UCD's own file, and a reviewer defeated that
// too — by building the channel out of registered bases whose selector a renderer
// ignores. An exemption is only safe over bases where a reader can see the selector
// act, and no set of those exists to name. So none is named. Both attempts are on a
// parked branch and neither shipped.
//
// **What is lost is injectivity, and nothing rested on it** — proved in
// TestStrippingIsLossyAndForgesNothing rather than asserted here. Two messages
// differing only in their selectors now produce one line, which the escaping form
// did not permit. It shrinks what a module can put in front of a reader rather than
// widening it: this function deletes and never inserts, so every line the boundary
// can emit was emittable before, and the two marks a reader leans on — the escape's
// own backslash and logTruncated behind it (D244) — stay out of reach either way.
func strippedRune(r rune) bool {
	return unicode.Is(unicode.Variation_Selector, r)
}

// defaultIgnorable is Unicode's `Default_Ignorable_Code_Point`, derived rather than
// asked for.
//
// **Go ships no table under that name.** It ships
// `Other_Default_Ignorable_Code_Point`, and the first form of escapedRune asked for
// that one — which is Unicode's *residue* property, the members left over once the
// derivation's other terms are removed, and so is smaller than the property whose
// name it nearly has. The real definition is
//
//	Other_DI ∪ Cf ∪ Variation_Selector − White_Space − FFF9..FFFB − 13430..13440 − PCM
//
// and the missing Variation_Selector term was the whole of F285. That line is
// transcribed term for term from the `Generated from` block above
// Default_Ignorable_Code_Point in Unicode 15.0's DerivedCoreProperties.txt, which is
// the file this claim answers to.
//
// **The same failure shape as the list D242 replaced, one level up.** A property
// whose name begins with `Other_` is incomplete by construction — it exists to be
// unioned, not to be asked — and this file had already learned twice that the way to
// carry a claim about a Unicode set is to compute the set Unicode defines.
//
// **The first fix for F285 wrote six of the seven terms**, dropping 13430..13440 —
// the Egyptian hieroglyph format characters — and so claiming 4190 members where the
// property has 4174. Behaviour was untouched, since the sixteen the union carries are
// Cf and the !IsGraphic fallback escapes them either way; what was wrong was the
// claim, which is this milestone's deliverable. It is F285's own shape a third level
// in: a derivation trimmed to the terms that change an answer is not the property,
// and the paragraph below — already in the file — is the argument against doing
// exactly that.
//
// Three of the four subtractions change no answer downstream, and are written anyway
// because this function's claim is that it *is* the property rather than that it
// happens to agree with one: FFF9..FFFB are Cf, so are the sixteen members of
// 13430..13440 that the union reaches (U+13440 itself is Mn and never enters it), and
// the !IsGraphic fallback escapes all nineteen regardless; PCM never reaches
// escapedRune, being answered one limb earlier. The White_Space subtraction is the
// derivation's, and no member of the union carries that property in the tables this
// Go was built with.
//
// **The Variation_Selector term decides nothing on the sanitizer's own path**, since
// sanitizeLogMessage strips those before asking (D283). It is written in for two
// reasons. A derivation trimmed to its caller's reach is one nobody can check
// against the standard, and checking it against the standard is the entire lesson of
// F285. And escapeLogRune is package-visible and is called on its own: with the term
// present, a caller that forgets to strip escapes a selector instead of passing it,
// which fails loudly rather than reopening the channel.
//
// Numbers, since a set claim without one is not a claim: 4174 members, 267 of them
// graphic, against the residue property's 3776 and 7 — and 4174 is what the UCD file
// totals for the property itself, arrived at independently. Pinned as **equalities**
// by TestNoDefaultIgnorableCharacterReachesALogLine, against unicode.Version 15.0. A
// floor is what let 4190 sit inside an enforcing test for a whole round, so a
// toolchain whose Unicode revision moves fails here and the numbers in this comment
// and in the six documents that print them move with it.
func defaultIgnorable(r rune) bool {
	if unicode.Is(unicode.White_Space, r) ||
		(r >= '\ufff9' && r <= '\ufffb') ||
		(r >= '\U00013430' && r <= '\U00013440') ||
		unicode.Is(unicode.Prepended_Concatenation_Mark, r) {
		return false
	}
	return unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
		unicode.Is(unicode.Cf, r) ||
		unicode.Is(unicode.Variation_Selector, r)
}

// meaningfulFormatRune is the allowlist D241 argued for: format characters that
// carry meaning in a message rather than hiding one.
//
// Each is a prefixed sign that scopes the digits after it — the Arabic number, sign,
// footnote marker, end-of-ayah, pound and piastre marks, the Syriac abbreviation
// mark, the Kaithi number signs. They are Cf, so default-deny would escape them, and
// a sanitizer that mangles Arabic is a bug with a worse blast radius than the one it
// prevents. Nothing here is invisible in the sense that matters: each changes how the
// run that follows it is read, in the place a reader is already looking.
//
// **It is Unicode's own property, not a transcription of one** (D243). Those
// characters are exactly `Prepended_Concatenation_Mark`, and the first form of this
// allowlist copied that property's members out by hand — eleven of the thirteen, so
// U+0890 ARABIC POUND MARK and U+0891 ARABIC PIASTRE MARK were escaped from the day
// it was written. That is the staleness U+061C was, and it is what D242 replaced an
// enumeration to be rid of one function up: a list a Unicode revision can outdate
// cannot carry a claim that names its members. Reading the table Go already ships
// means a toolchain update moves the allowlist with it.
//
// This is the whole of what default-deny gives back, which is why it is one
// predicate. Everything else in Cf is escaped, U+061C included — D241's own
// reasoning put it there and only the code disagreed.
func meaningfulFormatRune(r rune) bool {
	return unicode.Is(unicode.Prepended_Concatenation_Mark, r)
}

// hostState is what one add-on's calls are answered against.
type hostState struct {
	manifest Manifest
	// log carries the add-on's name and marks the line as an add-on's own, so an
	// operator reading a log can tell this product's words from a module's.
	log *slog.Logger
	// hostLog is the host's own voice about this add-on: it names the add-on and
	// does not mark the line as the add-on's words. The distinction is the one
	// dispatch already makes when it refuses a call — a refusal is the host
	// speaking about a module, not the module speaking.
	hostLog *slog.Logger
	// settings is manifest.Settings by name, which is what config_get scopes to.
	settings map[string]Setting
	// values is what an operator configured, by setting name, and it is what
	// config_get answers with when there is one. Held as config.Secret for every
	// setting rather than only the ones a manifest called secret, so that no
	// path out of this struct can print a configured value.
	values map[string]config.Secret
	// grants is what this add-on holds, resolved once at load. Read on every call
	// through the ABI and never rebuilt — see Grants, where the reason it has to be
	// a lookup rather than a walk of the manifest is the redirect path's budget.
	grants Grants
	// storage is this add-on's confined connection to the schema it owns, or nil
	// for one that declared no storage — and also for one that declared it on a
	// host with no database, which noStorage answers.
	storage *store.AddonDB

	// request is what this instance is answering, and nil for the instance that
	// was created at load. It is the whole of "outside a request": a per-request
	// instance has its own state, so nothing has to be cleared afterwards and no
	// two requests can see each other's.
	request *Request
	// encoded is request, marshalled once. A guest that reads the record twice —
	// which the retry half of the calling convention makes ordinary, since a
	// first call with a small buffer answers with the size — pays for one
	// marshal rather than two.
	encoded []byte
	// session is who is signed in on that request, and it is a value rather than
	// a pointer because "nobody" is a SessionContext with SignedIn false rather
	// than an absence (D261).
	session SessionContext
	// response is what the guest wrote, or nil until it does.
	response *Response

	// minter is what answers session_mint, resolved once at load from
	// Options.Sessions. Nil is a host that cannot mint — every unit test in this
	// package, and no instance.
	minter SessionMinter
	// mfaSatisfied is the operator's answer for *this* add-on: whether the
	// provider behind it already met a second factor. False is the default and the
	// safe reading; it is read from the environment and never from the manifest,
	// because it is a claim about a provider's authentication strength that only
	// the person who configured that provider can make.
	mfaSatisfied bool

	// identity is whoever the host resolved for this request, or nil for nobody,
	// and it is deliberately not read off `session`: Route blanks that record for
	// an add-on that did not declare session.context, so reading it here would make
	// an add-on's own manifest decide whether the host notices a session. It is
	// also the actor identity_link writes a link for, which is what makes "the
	// module names a subject and the host names the account" structural.
	identity *auth.Identity
	// clientIP and userAgent are the request's, held so that a session minted here
	// records where the sign-in came from exactly as the password path's does.
	// **Neither crosses to the guest**: no ABI record carries either, the address
	// becomes a /24 or /48 prefix inside internal/auth before it reaches a column,
	// and abi.AddressBearing is what keeps the first half of that true from the
	// other end.
	clientIP  netip.Addr
	userAgent string
	// minted is what the host produced on this request, or nil. It holds the
	// session token and never crosses the ABI — see Minted.
	minted *Minted

	// The redirect limb (M66). Three fields and each of them is nil outside the
	// invocation it belongs to, which is what makes "outside a redirect" a state
	// rather than a flag: an inline invocation gets its own state, so a load-time
	// instance and a route's instance answer StatusNotFound to both reads without
	// anything having to be cleared.
	//
	// decision is what an inline module is being asked about, and encodedDecision
	// is it marshalled once — for the reason `encoded` is, since the retry half of
	// the calling convention makes a second read ordinary.
	decision        *RedirectDecision
	encodedDecision []byte
	// answer is what the inline module wrote back, or nil for a module that wrote
	// nothing. Nil is *allow, unchanged*, which is also what a module the host had
	// to kill leaves behind — the two agreeing is deliberate and is why the veto is
	// a written verdict rather than an unwritten one.
	answer *RedirectAnswer
	// event is what an observing module is being handed, marshalled once beside it.
	// The observe class runs after the response, so nothing here is on the hot path.
	event        *RedirectEvent
	encodedEvent []byte
	// inline marks an invocation that is holding a visitor's redirect open. It is
	// what dispatch reads to refuse everything outside abi.InlineSafe — storage
	// above all — whatever this add-on's manifest declared.
	inline bool

	// metrics is the host's registry, held so that a refusal decided *inside* a
	// host function rather than by dispatch reaches the same counter dispatch
	// writes. There is one such refusal — the query rewrite's second grant — and
	// an operator asking "is anything being refused" is entitled to one answer
	// rather than two places to look. Nil-safe, like every method on it, which is
	// what lets a test build a state without a registry.
	metrics *observability.Metrics
}

// forRedirect is the per-invocation copy of an add-on's state, for either
// redirect class.
//
// A copy for the reason [hostState.forRequest] is one: the fields below are
// written by the goroutine that made them and read only by the guest they belong
// to, so neither class needs a lock and neither can see the other's subject.
// Exactly one of the two arguments is non-nil at any call site, which is what
// makes the class a property of the state rather than an argument a host function
// has to be told.
func (s *hostState) forRedirect(decision *RedirectDecision, event *RedirectEvent) *hostState {
	out := *s
	out.request = nil
	out.encoded = nil
	out.response = nil
	out.minted = nil
	out.session = SessionContext{}
	out.identity = nil
	out.decision = decision
	out.encodedDecision = nil
	out.answer = nil
	out.event = event
	out.encodedEvent = nil
	out.inline = decision != nil
	return &out
}

// forPool is the state a pooled instance rests in: on the redirect path, holding
// its class, and carrying neither a decision nor an event (M66.5).
//
// It exists because package initialization runs during instantiation and an entry
// is instantiated once for many redirects. Handing init the first redirect that
// happened to need an instance would tell a module about a visitor it is not being
// asked about, and would put that redirect's record in the image every later
// invocation is reset to.
//
// The inline flag is kept anyway, so an instance made for the inline class is held
// to abi.InlineSafe from its first instruction rather than from its first
// invocation.
func (s *hostState) forPool(class string) *hostState {
	out := s.forRedirect(nil, nil)
	out.inline = class == ClassInline
	return out
}

// encodedRedirect marshals whichever redirect record this invocation carries,
// once, and remembers it.
func (s *hostState) encodedRedirect() ([]byte, error) {
	if s.decision != nil {
		if s.encodedDecision == nil {
			b, err := json.Marshal(s.decision)
			if err != nil {
				return nil, err
			}
			s.encodedDecision = b
		}
		return s.encodedDecision, nil
	}
	if s.encodedEvent == nil {
		b, err := json.Marshal(s.event)
		if err != nil {
			return nil, err
		}
		s.encodedEvent = b
	}
	return s.encodedEvent, nil
}

// forRequest is the per-request copy of an add-on's state.
//
// A copy, so the load-time state keeps answering config_get and log for whatever
// else is running, and so the fields above are written once by the goroutine that
// created them and read only by the guest it belongs to — which is what makes
// this path need no lock of its own.
func (s *hostState) forRequest(req *Request, sess SessionContext, in RequestIn) *hostState {
	out := *s
	out.request = req
	out.session = sess
	out.identity = in.Identity
	out.clientIP = in.ClientIP
	out.userAgent = in.UserAgent
	out.encoded = nil
	out.response = nil
	out.minted = nil
	return &out
}

// encodedRequest marshals the request record once and remembers it.
func (s *hostState) encodedRequest() ([]byte, error) {
	if s.encoded == nil {
		b, err := json.Marshal(s.request)
		if err != nil {
			return nil, err
		}
		s.encoded = b
	}
	return s.encoded, nil
}

// marshalFailed is a record this host could not encode, which is the host's own
// fault and is StatusInternal by the ABI's definition of it.
func (s *hostState) marshalFailed(function string, err error) int32 {
	s.hostLog.Error("the host could not encode a record it owes an add-on",
		slog.String("addon", s.manifest.Name),
		slog.String("function", function),
		slog.Any("error", err))
	return int32(abi.StatusInternal)
}

func newHostState(m Manifest, grants Grants, storage *store.AddonDB,
	values map[string]config.Secret, log *slog.Logger,
	minter SessionMinter, mfaSatisfied bool) *hostState {
	// Open has wrapped this already and wrapping is idempotent; it is repeated here
	// because a state built directly — which is what a test does — must be as safe as
	// one built through a load.
	log = neutralizingLogger(log)
	settings := make(map[string]Setting, len(m.Settings))
	for _, s := range m.Settings {
		settings[s.Name] = s
	}
	return &hostState{
		manifest: m,
		log: log.With(
			slog.String("addon", m.Name),
			slog.String("source", "addon"),
		),
		hostLog:      log,
		settings:     settings,
		values:       values,
		grants:       grants,
		storage:      storage,
		minter:       minter,
		mfaSatisfied: mfaSatisfied,
	}
}

// registerState makes an add-on's state reachable from a host function before the
// module that will call one exists. Returns the deregistration.
func (h *Host) registerState(m Manifest, grants Grants, storage *store.AddonDB,
	values map[string]config.Secret) func() {
	// The two M65 facts are read here rather than passed in, because both are
	// properties of the host and of this add-on's name and neither changes for the
	// life of the load — the same reason grants are resolved once.
	st := newHostState(m, grants, storage, values, h.log,
		h.sessions, mfaSatisfiedByProvider(h.overrides(m.Name)))
	// Set here rather than taken as an eighth parameter: it is the same host's
	// registry for every add-on and it is nil-safe, so threading it through the
	// constructor would add an argument to every test that builds a state by hand
	// and buy nothing.
	st.metrics = h.metrics
	h.mu.Lock()
	if h.states == nil {
		h.states = make(map[string]*hostState)
	}
	h.states[m.Name] = st
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.states, m.Name)
		h.mu.Unlock()
	}
}

func (h *Host) hostState(name string) *hostState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.states[name]
}

// registerABI instantiates the one host module every add-on imports.
//
// Called once per runtime, from Open, and only when there is an add-ons directory
// to read — an instance with none constructs no runtime, so it registers no
// module either and the ABI costs it nothing.
func (h *Host) registerABI(ctx context.Context) error {
	b := h.runtime.NewHostModuleBuilder(abi.HostModule)
	for _, f := range abi.Functions {
		impl, ok := hostFuncs[f.Name]
		switch {
		case f.Live && !ok:
			// A construction-time panic rather than an error, because it cannot
			// depend on anything an operator did: abi.Functions and hostFuncs are
			// both in this binary, so this is a build that should not have linked.
			panic(fmt.Sprintf("addon: ABI function %q is marked live and has no host implementation", f.Name))
		case !f.Live && ok:
			panic(fmt.Sprintf("addon: ABI function %q has a host implementation and is not marked live", f.Name))
		}
		params, results := hostSignature(f)
		b.NewFunctionBuilder().
			WithGoModuleFunction(h.dispatch(f, impl), params, results).
			WithName(f.Name).
			Export(f.Name)
	}
	if _, err := b.Instantiate(ctx); err != nil {
		return fmt.Errorf("host module %q: %w", abi.HostModule, err)
	}
	return nil
}

// dispatch is the wrapper every ABI function is registered through: it finds the
// calling add-on's state, checks the grant the function costs, and answers on the
// wasm stack.
//
// **The permission check comes before everything else, including before a refused
// function's StatusNotAvailable.** Two reasons, and the second is the one worth
// stating: an add-on that declared nothing must not be able to use the ABI's own
// capability probe to enumerate which limbs this host implements, and a refusal
// counted per module is only complete if it counts the calls to functions that do
// not work yet — which is every capability worth abusing until M63 to M66 land.
//
// The check is a map lookup on a set resolved at load. It sits on the redirect
// path from M66, where the inherited rule is a cached p99 under 20 ms, so it
// touches no manifest, no vocabulary and nothing on disk. See Grants.
func (h *Host) dispatch(f abi.Function, impl hostFunc) api.GoModuleFunc {
	return func(ctx context.Context, mod api.Module, stack []uint64) {
		st := h.hostState(mod.Name())
		if st == nil {
			// The calling module is not one this host registered state for, which
			// is either a module instantiated outside Open or a name collision the
			// directory-equals-name rule is supposed to make impossible. Refuse,
			// and say so where an operator can see it: silently answering with
			// another add-on's configuration is the failure worth being loud about.
			h.log.Error("an add-on called a host function and the host has no state for it",
				slog.String("module", mod.Name()),
				slog.String("function", f.Name))
			stack[0] = api.EncodeI32(int32(abi.StatusInternal))
			return
		}
		if f.Requires != "" && !st.grants.Has(f.Requires) {
			// Counted always, logged at debug. The counter is what an operator
			// alerts on and it is bounded — one series per add-on per permission —
			// while a warning per call would be a module's own loop deciding how
			// much an instance logs, on a path that from M66 is the redirect path.
			// The add-on's name comes from a validated manifest and the permission
			// from a closed vocabulary, so neither label is guest input.
			h.metrics.ObserveAddonRefusal(st.manifest.Name, f.Requires)
			// h.log rather than st.log: st.log marks a line as the add-on's own
			// words, and this is the host's refusal of them.
			h.log.Debug("refused an add-on's call: it did not declare the permission "+
				"the function needs",
				slog.String("addon", st.manifest.Name),
				slog.String("function", f.Name),
				slog.String("permission", f.Requires))
			stack[0] = api.EncodeI32(int32(abi.StatusDenied))
			return
		}
		if st.inline && !abi.CallableInline(f.Name) {
			// The redirect tree's own rule, enforced at the boundary (M66): an
			// inline invocation is holding a visitor's redirect open, so it reaches
			// only what abi.InlineSafe names — no storage, no request, no session,
			// no template — **whatever this add-on's manifest declared**. Denied
			// rather than NotFound, because the capability exists and this is not
			// where it may be used.
			//
			// After the grant check, so a module that declared neither the
			// permission nor a redirect class cannot tell the two refusals apart,
			// and not counted on the refusals series: that counter is undeclared
			// calls, and this is a declared one in the wrong place. The log line is
			// the record, and it is at debug for the reason the one above is — this
			// is the redirect path.
			h.log.Debug("refused an add-on's call: an inline redirect invocation reaches "+
				"only the redirect-safe subset of this ABI",
				slog.String("addon", st.manifest.Name),
				slog.String("function", f.Name))
			stack[0] = api.EncodeI32(int32(abi.StatusDenied))
			return
		}
		if impl == nil {
			stack[0] = api.EncodeI32(int32(abi.StatusNotAvailable))
			return
		}
		stack[0] = api.EncodeI32(impl(ctx, st, mod, stack))
	}
}

// hostSignature expands an ABI function's parameters into the wasm value types
// wazero needs. It is the same expansion the generator applies to produce the
// //go:wasmimport declarations, from the same slice, which is what makes a
// signature mismatch between host and guest unrepresentable rather than merely
// unlikely.
func hostSignature(f abi.Function) (params, results []api.ValueType) {
	for _, p := range f.Params {
		switch p.Kind {
		case abi.Int32:
			params = append(params, api.ValueTypeI32)
		case abi.Int64:
			params = append(params, api.ValueTypeI64)
		default:
			// A pointer and a length, both i32: wasm32 addresses are 32 bits and so
			// is every length this convention carries.
			params = append(params, api.ValueTypeI32, api.ValueTypeI32)
		}
	}
	return params, []api.ValueType{api.ValueTypeI32}
}

// readString copies a (pointer, length) pair out of guest memory.
//
// Copied, not aliased: mod.Memory().Read hands back a window onto the guest's own
// linear memory, and holding one past the call means holding a view the guest can
// rewrite. It also validates UTF-8, because every String parameter in this ABI is
// a name, a level or a statement — things a host would otherwise store or log with
// replacement characters in them.
func readString(mod api.Module, ptr, length uint64) (string, bool) {
	b, ok := readBytes(mod, ptr, length)
	if !ok {
		return "", false
	}
	if !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

func readBytes(mod api.Module, ptr, length uint64) ([]byte, bool) {
	n := api.DecodeU32(length)
	if n == 0 {
		return nil, true
	}
	if n > maxStringIn {
		return nil, false
	}
	view, ok := mod.Memory().Read(api.DecodeU32(ptr), n)
	if !ok {
		return nil, false
	}
	out := make([]byte, len(view))
	copy(out, view)
	return out, true
}

// writeOut implements the out-parameter half of the calling convention: the value
// goes into the guest's buffer when it fits, and either way the answer is the
// size it occupies.
//
// The "does not fit" case writes nothing at all rather than truncating. A
// truncated JSON record is a parse error a publisher debugs as a host bug; a size
// they can allocate against is the retry the SDK already does for them.
func writeOut(mod api.Module, ptr, capacity uint64, value []byte) int32 {
	if len(value) > math.MaxInt32 {
		// Not reachable from anything this host produces, and checked because the
		// convention's return value is an i32: a size that did not fit one would come
		// back negative and the guest would read it as a status. StatusInternal is the
		// honest answer — the host has a value it cannot describe.
		return int32(abi.StatusInternal)
	}
	size := int32(len(value)) //nolint:gosec // G115: bounded against MaxInt32 immediately above
	// Compared as int, because the guest's capacity is a u32 and the size is an i32:
	// converting either into the other's type is the overflow this function just ruled
	// out for one of them and cannot rule out for the other.
	if int(size) > int(api.DecodeU32(capacity)) {
		return size
	}
	if size > 0 && !mod.Memory().Write(api.DecodeU32(ptr), value) {
		return int32(abi.StatusInvalid)
	}
	return size
}
