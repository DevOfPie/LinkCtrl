package addon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tetratelabs/wazero/api"

	"github.com/DevOfPie/LinkCtrl/internal/addon/abi"
	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/config"
	"github.com/DevOfPie/LinkCtrl/internal/domain"
)

// agreeableMinter is a SessionMinter that accepts every assertion.
//
// It exists so that `session_mint` can be driven for real in tests that are about
// something else — what crosses the boundary, what a log record looks like —
// without a Postgres. It is deliberately not a fake of the *rules*: every refusal
// this milestone is about lives in internal/auth and is tested there and in
// test/integration against a real database, because a stub that decided who may
// sign in would be a test asserting its own opinion.
type agreeableMinter struct {
	// last is what it was asked, so a caller can assert what the host passed on —
	// which is how the tests that matter check that the add-on's name came from the
	// host and not from the claim.
	last auth.AddonAssertion
	// mints counts how many assertions reached it. A session_mint that answered a
	// guest without minting is only distinguishable from one that minted and then
	// reported a size by counting here.
	mints int
	// pending makes it answer with a second-factor challenge instead of a session.
	pending bool
	// err, when set, is what it answers instead — from both methods.
	err error
	// linked is every (issuer, subject) it was asked to connect, and to whom, so a
	// caller can assert that the account came from the host's identity and never
	// from the module's claim.
	linked []linkCall
}

// linkCall is one call to LinkAddonIdentity, as the stub saw it.
type linkCall struct {
	actor           *auth.Identity
	addon           string
	issuer, subject string
}

func (m *agreeableMinter) LinkAddonIdentity(_ context.Context, actor *auth.Identity,
	addon, issuer, subject string) error {
	m.linked = append(m.linked, linkCall{actor: actor, addon: addon, issuer: issuer, subject: subject})
	return m.err
}

func (m *agreeableMinter) MintFromAddonAssertion(_ context.Context,
	in auth.AddonAssertion) (*auth.AddonMint, error) {
	m.last = in
	m.mints++
	if m.err != nil {
		return nil, m.err
	}
	at := time.Now().Add(time.Hour).UTC()
	if m.pending {
		return &auth.AddonMint{
			Pending:   &auth.PendingSecondFactor{Token: "pending-token", Expires: at},
			ExpiresAt: at,
		}, nil
	}
	return &auth.AddonMint{
		Login:     &auth.LoginResult{Token: "session-token", Expires: at},
		ExpiresAt: at,
	}, nil
}

// --- M65: the authentication hook -------------------------------------------
//
// What is here is the *boundary*: what a module may assert, what it learns back,
// and what the host does with what it decided. The rules themselves — an account
// existing, being active, not being locked, still owing a second factor — are
// internal/auth's and are driven against a real Postgres in test/integration,
// because a stub that decided who may sign in would be a test asserting its own
// opinion.

// identityHost installs the M65 fixture and opens a host over it.
func identityHost(t *testing.T, minter *agreeableMinter, tweak func(*Manifest)) (*Host, *logSink) {
	t.Helper()
	sink := &logSink{}
	return identityHostLogging(t, minter, tweak,
		slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})), sink
}

// identityHostLogging is the same fixture with the log handler chosen by the
// caller, because one test here asks *which field* attributed a record and a text
// handler answers that only by substring — the confusion the field exists to end.
func identityHostLogging(t *testing.T, minter *agreeableMinter, tweak func(*Manifest),
	handler slog.Handler) *Host {
	t.Helper()
	code := fixture(t, "identity")
	dir := t.TempDir()
	m := manifestFor("identity", ClassDegrade, code)
	m.Permissions = []string{PermissionRoutes, PermissionSessionMint}
	if tweak != nil {
		tweak(&m)
	}
	install(t, dir, m, code)

	opts := Options{
		Dir:      dir,
		Logger:   slog.New(handler),
		Settings: func(string, []string) map[string]config.Secret { return nil },
	}
	// Assigned only when there is one, because a typed nil in an interface field is
	// not a nil interface — the host would call through it and the guest would meet
	// a trap where it was meant to meet a status.
	if minter != nil {
		opts.Sessions = minter
	}
	h, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("the identity fixture did not load: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

// assertAt drives one request at the identity fixture.
func assertAt(t *testing.T, h *Host, path, subject string, id *auth.Identity) Response {
	t.Helper()
	resp, err := h.Route(context.Background(), "identity", RequestIn{
		Method: http.MethodGet, Path: path, Query: subject,
		ClientIP:  netip.MustParseAddr("203.0.113.7"),
		UserAgent: "test-agent/1.0",
		Identity:  id,
	})
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return resp
}

// The milestone's first bullet, from both sides at once: the host mints, the
// add-on asserts, and the token exists on this side of the sandbox and nowhere
// else.
//
// The guest prints the **whole raw record** it was handed, not only the fields it
// has a struct for, so this assertion covers a field somebody adds to
// MintedSession later without touching the fixture.
func TestTheHostMintsAndTheModuleNeverSeesTheToken(t *testing.T) {
	minter := &agreeableMinter{}
	h, _ := identityHost(t, minter, nil)

	resp := assertAt(t, h, "/callback", "subject-1", nil)

	if resp.Minted == nil {
		t.Fatalf("the host carried no minted session out of the request: %q", resp.Body)
	}
	if resp.Minted.Token.Reveal() != "session-token" {
		t.Errorf("the host did not carry the token: %q", resp.Minted.Token.Reveal())
	}
	if !strings.Contains(resp.Body, "minted expires_at=") {
		t.Fatalf("the module did not report a mint: %q", resp.Body)
	}
	if !strings.Contains(resp.Body, "second_factor_required=false") {
		t.Errorf("the module read the wrong second-factor answer: %q", resp.Body)
	}
	// The claim this fixture makes that no host-side check can: the credential is
	// not in what the guest printed, and the guest printed everything it got.
	for _, forbidden := range []string{"session-token", "pending-token"} {
		if strings.Contains(resp.Body, forbidden) {
			t.Errorf("a credential crossed into the guest and came back on the page: %q", resp.Body)
		}
	}
}

// The add-on's name is the host's, and the claim has no field for it. Asserted
// against what the host passed on rather than against the ABI's shape, because
// "the record has no `addon` field" and "the host ignores the record's `addon`
// field" are different claims and only the second one is what keeps a module out
// of another add-on's name.
func TestTheHostNamesTheAssertingAddonAndTheModuleCannotChooseIt(t *testing.T) {
	minter := &agreeableMinter{}
	h, _ := identityHost(t, minter, nil)

	assertAt(t, h, "/callback", "subject-1", nil)

	if minter.last.Addon != "identity" {
		t.Errorf("the assertion named add-on %q, and the module is called identity", minter.last.Addon)
	}
	if minter.last.Subject != "subject-1" || minter.last.Issuer != "https://idp.test" {
		t.Errorf("the claim did not reach the service intact: %+v", minter.last)
	}
	// The two facts the host holds about the request, which no field of the claim
	// can carry and which a module therefore cannot forge.
	if minter.last.IP.String() != "203.0.113.7" || minter.last.UserAgent != "test-agent/1.0" {
		t.Errorf("the request's own facts did not reach the service: %+v", minter.last)
	}
	// The address the fixture asserted is deliberately somebody else's, and it is
	// carried rather than acted on — which is what makes "matching by email alone
	// is refused by design" checkable here as well as in the query.
	if minter.last.Email != "someone-else@elsewhere.test" {
		t.Errorf("the asserted address did not cross: %q", minter.last.Email)
	}
}

// A mint is how somebody signs in, not how a browser changes who it is signed in
// as. The host reports the request's own state and the service refuses on it.
func TestWhetherSomebodyIsAlreadySignedInIsTheHostsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   *auth.Identity
		want bool
	}{
		{"anonymous", nil, false},
		{"signed in", &auth.Identity{UserID: uuid.MustParse("0198c9c5-0000-7000-8000-000000000001")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			minter := &agreeableMinter{}
			h, _ := identityHost(t, minter, nil)
			assertAt(t, h, "/callback", "subject-1", tc.id)
			if minter.last.AlreadySignedIn != tc.want {
				t.Errorf("the host reported already_signed_in=%v, want %v",
					minter.last.AlreadySignedIn, tc.want)
			}
		})
	}
}

// And it is the *host's* answer rather than the record's, which is the hazard the
// grant blanking creates: an add-on that did not declare `session.context` is
// handed an empty SessionContext, and if the mint read that record it would see
// nobody signed in whenever the manifest was one token short.
//
// This is the test that would have caught it: the same signed-in request, to an
// add-on that reads no session at all.
func TestAnAddonCannotHideASessionByNotDeclaringSessionContext(t *testing.T) {
	minter := &agreeableMinter{}
	h, _ := identityHost(t, minter, func(m *Manifest) {
		// Exactly what the fixture needs and no session read.
		m.Permissions = []string{PermissionRoutes, PermissionSessionMint}
	})
	id := &auth.Identity{UserID: uuid.MustParse("0198c9c5-0000-7000-8000-000000000001")}

	assertAt(t, h, "/callback", "subject-1", id)

	if !minter.last.AlreadySignedIn {
		t.Error("an add-on that did not declare session.context hid a live session from the " +
			"mint, so its own manifest decided whether the host noticed one")
	}
}

// One mint per request, refused the way a second response is and for the same
// reason. The guest is told, so its author learns from the call rather than from
// a browser holding one of two sessions.
func TestASecondMintInOneRequestIsRefused(t *testing.T) {
	minter := &agreeableMinter{}
	h, _ := identityHost(t, minter, nil)

	resp := assertAt(t, h, "/callback-twice", "subject-1", nil)

	if !strings.Contains(resp.Body, "first: minted") {
		t.Fatalf("the first mint did not succeed: %q", resp.Body)
	}
	if !strings.Contains(resp.Body, "second: refused") {
		t.Errorf("a second mint in one request was allowed: %q", resp.Body)
	}
	if resp.Minted == nil || resp.Minted.Token.Reveal() != "session-token" {
		t.Error("the first mint was lost")
	}
}

// TestAShortBufferMintsNothingAndTheRetryIsTheFirstMint.
//
// The out-parameter convention docs/addon-abi.md publishes says a value too large
// for the buffer offered means **nothing was written** and the caller retries with
// a buffer that size. `session_mint` is the only function on this ABI with both an
// out parameter and a side effect, so on this one *nothing was written* has to
// mean nothing happened: otherwise the retry — which the generated SDK performs
// without asking — re-enters, meets the one-mint guard, and is answered ErrInvalid
// while the host has already minted and is about to set the cookie. The module and
// the host would then be telling two different stories about who is signed in, and
// the browser would be holding the host's.
//
// Driven from the guest, because the guest is the only place the buffer is chosen.
// The fixture declares the raw import and offers one byte, which is what a
// publisher writing against the ABI rather than against the SDK can do.
func TestAShortBufferMintsNothingAndTheRetryIsTheFirstMint(t *testing.T) {
	minter := &agreeableMinter{}
	h, _ := identityHost(t, minter, nil)

	resp := assertAt(t, h, "/callback-tiny-buffer", "subject-1", nil)

	if !strings.Contains(resp.Body, "tiny: size=") {
		t.Fatalf("a one-byte buffer was not answered with a size to retry at: %q", resp.Body)
	}
	if !strings.Contains(resp.Body, "retry: minted") {
		t.Errorf("the retry the convention prescribes was refused, so the host minted on "+
			"a call it told the guest wrote nothing: %q", resp.Body)
	}
	if want := "tiny: size=" + strconv.Itoa(mintedSessionMaxBytes); !strings.Contains(resp.Body, want) {
		t.Errorf("the size answered is not the bound the host requires; %q does not carry %q",
			resp.Body, want)
	}
	if resp.Minted == nil || resp.Minted.Token.Reveal() != "session-token" {
		t.Error("the retry's session was lost")
	}

	// And the short call on its own reaches the minter not at all. Both orderings
	// above ask for exactly one session — the one-mint guard sees to that — so the
	// count only says anything when the retry is absent.
	alone := &agreeableMinter{}
	ha, _ := identityHost(t, alone, nil)

	short := assertAt(t, ha, "/callback-tiny-only", "subject-1", nil)

	if !strings.Contains(short.Body, "tiny: size=") {
		t.Fatalf("a one-byte buffer was not answered with a size: %q", short.Body)
	}
	if alone.mints != 0 {
		t.Errorf("a call the host told the guest wrote nothing minted %d session(s). "+
			"*Nothing was written* has to mean nothing happened on the one function "+
			"here with a side effect", alone.mints)
	}
	if short.Minted != nil {
		t.Error("the host was about to set a cookie for a call it answered with a size")
	}
}

// TestAMintedSessionFitsItsPublishedBound.
//
// mintedSessionMaxBytes is what the host requires of a guest's buffer *before* it
// mints, and it is a constant because the record's real size is not known until
// after the mint. That makes it a claim about arithmetic nobody re-derives, so it
// is held here against the record actually marshalled — including the widest
// instant this product can put in a session expiry.
func TestAMintedSessionFitsItsPublishedBound(t *testing.T) {
	for _, tc := range []struct {
		what string
		at   time.Time
		mfa  bool
	}{
		{"an ordinary expiry", time.Now().Add(720 * time.Hour).UTC(), false},
		{"the second-factor answer", time.Now().Add(10 * time.Minute).UTC(), true},
		{"the widest year RFC 3339 writes in four digits",
			time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), false},
		{"the zero instant", time.Time{}.UTC(), false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			encoded, err := json.Marshal(MintedSession{
				ExpiresAt:            tc.at.UTC().Format(time.RFC3339),
				SecondFactorRequired: tc.mfa,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > mintedSessionMaxBytes {
				t.Errorf("a MintedSession encoded to %d bytes against a bound of %d. The "+
					"host checks that bound before it mints, so a record that outgrew it "+
					"would be minted and then unwritable: %s",
					len(encoded), mintedSessionMaxBytes, encoded)
			}
		})
	}
}

// A claim the host cannot resolve anybody from is the module's fault, and is
// answered as a different status from a subject nobody has linked. The difference
// matters to the module's author: one is a bug and the other is a person who has
// not connected a provider.
func TestAnIncompleteClaimIsTheModulesFault(t *testing.T) {
	for _, path := range []string{"/callback-no-subject", "/callback-no-issuer"} {
		t.Run(path, func(t *testing.T) {
			minter := &agreeableMinter{}
			h, _ := identityHost(t, minter, nil)

			resp := assertAt(t, h, path, "subject-1", nil)

			if !strings.Contains(resp.Body, "refused") {
				t.Fatalf("an incomplete claim was accepted: %q", resp.Body)
			}
			if !strings.Contains(resp.Body, "ErrInvalid") {
				t.Errorf("an incomplete claim did not answer ErrInvalid: %q", resp.Body)
			}
			if resp.Minted != nil {
				t.Error("something was minted for a claim naming nobody")
			}
			// The service was never asked, which is the property worth having: a
			// malformed claim does not become a database lookup.
			if minter.last.Subject != "" || minter.last.Issuer != "" {
				t.Errorf("an incomplete claim reached the session service: %+v", minter.last)
			}
		})
	}
}

// A second factor still owed crosses as a flag and never as the pending
// credential, which is a bearer token for the one operation that is a session.
func TestASecondFactorCrossesAsAFlagAndTheChallengeDoesNot(t *testing.T) {
	minter := &agreeableMinter{pending: true}
	h, _ := identityHost(t, minter, nil)

	resp := assertAt(t, h, "/callback", "subject-1", nil)

	if resp.Minted == nil || !resp.Minted.SecondFactorRequired {
		t.Fatalf("the host did not carry the second-factor answer: %+v", resp.Minted)
	}
	if resp.Minted.PendingToken.Reveal() != "pending-token" {
		t.Errorf("the host did not carry the challenge: %q", resp.Minted.PendingToken.Reveal())
	}
	if resp.Minted.Token.Reveal() != "" {
		t.Error("a session token was minted for somebody who still owes a second factor")
	}
	if !strings.Contains(resp.Body, "second_factor_required=true") {
		t.Errorf("the module was not told a factor is owed: %q", resp.Body)
	}
	if strings.Contains(resp.Body, "pending-token") {
		t.Errorf("the challenge crossed into the guest: %q", resp.Body)
	}
}

// Neither credential can be printed by anything that holds one, which is what the
// Secret wrapper buys and is worth asserting because the wrapper is invisible at
// the call site.
func TestTheMintedCredentialsCannotPrintThemselves(t *testing.T) {
	minter := &agreeableMinter{}
	h, _ := identityHost(t, minter, nil)
	resp := assertAt(t, h, "/callback", "subject-1", nil)

	for _, printed := range []string{
		fmt.Sprintf("%v", *resp.Minted),
		fmt.Sprintf("%+v", resp.Minted),
		fmt.Sprint(resp.Minted.Token),
	} {
		if strings.Contains(printed, "session-token") {
			t.Errorf("a minted session printed its own token: %s", printed)
		}
	}
}

// The linking half. The module names a subject; the **host** names the account,
// from the request's own session, and there is no field in the claim that could
// change it.
func TestOnlyTheHostNamesTheAccountALinkIsWrittenFor(t *testing.T) {
	minter := &agreeableMinter{}
	h, _ := identityHost(t, minter, nil)
	id := &auth.Identity{UserID: uuid.MustParse("0198c9c5-0000-7000-8000-000000000001")}

	resp := assertAt(t, h, "/connect", "subject-9", id)

	if !strings.Contains(resp.Body, "link: <nil>") {
		t.Fatalf("the link was not made: %q", resp.Body)
	}
	if len(minter.linked) != 1 {
		t.Fatalf("the service saw %d link calls", len(minter.linked))
	}
	got := minter.linked[0]
	if got.actor != id {
		t.Errorf("the link was written for %+v and the request's session is %+v", got.actor, id)
	}
	if got.addon != "identity" || got.issuer != "https://idp.test" || got.subject != "subject-9" {
		t.Errorf("the link call did not carry the claim intact: %+v", got)
	}
}

// TestTheMintRecordIsTheHostsOwnVoice.
//
// The one security-relevant statement on this boundary — *a session was minted on
// a module's word* — and the only thing that makes it a record rather than a claim
// is the field saying who wrote it.
//
// `st.log` carries `source=addon` and is what the ungated `log` host function
// hands a module; every host statement in hostabi.go uses `st.hostLog`. Written
// with the first, this line would be attributed to the party it is a record
// *about*, and a module holding nothing but `log` could emit a byte-identical one:
// the message is graphic ASCII, which is exactly what logsafe.go's neutralization
// passes through unchanged. That is the forgery Plan.md's add-on invariant is
// written against and D285's attribution property.
//
// Decoded rather than searched, for hostabi_test.go's reason: a substring test for
// `source=host` also matches a module's own message.
func TestTheMintRecordIsTheHostsOwnVoice(t *testing.T) {
	var buf strings.Builder
	h := identityHostLogging(t, &agreeableMinter{}, nil,
		slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if resp := assertAt(t, h, "/callback", "subject-9", nil); resp.Status != http.StatusOK {
		t.Fatalf("the assertion did not mint: %d %q", resp.Status, resp.Body)
	}

	var mint map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unreadable record %q: %v", line, err)
		}
		if rec["msg"] == "minted a session on this add-on's assertion" {
			mint = rec
		}
	}
	if mint == nil {
		t.Fatalf("nothing recorded the mint:\n%s", buf.String())
	}
	if got, ok := mint["source"]; ok {
		t.Errorf("the host's mint record is stamped source=%v. A record the host writes "+
			"about what a module asked for is the host's statement; stamped as the "+
			"add-on's, a module holding only the ungated log can write the same bytes",
			got)
	}
	if mint["addon"] != "identity" {
		t.Errorf("the mint record names addon=%v and the module that asserted is "+
			"identity; provenance nobody can attribute is not provenance", mint["addon"])
	}
	if mint["issuer"] != "https://idp.test" {
		t.Errorf("the mint record names issuer=%v", mint["issuer"])
	}
}

// Nobody signed in means no actor, and the refusal is the service's rather than a
// check this package could forget: the actor it is handed is nil, which is what
// requireSessionActor refuses.
func TestLinkingHandsOverNoActorWhenNobodyIsSignedIn(t *testing.T) {
	minter := &agreeableMinter{err: domain.ErrUnauthorized}
	h, sink := identityHost(t, minter, nil)

	resp := assertAt(t, h, "/connect", "subject-9", nil)

	if len(minter.linked) != 1 || minter.linked[0].actor != nil {
		t.Fatalf("the link call did not carry a nil actor: %+v", minter.linked)
	}
	if !strings.Contains(resp.Body, "ErrDenied") {
		t.Errorf("the refusal did not reach the module as ErrDenied: %q", resp.Body)
	}
	if !strings.Contains(sink.String(), "refused an add-on's request to connect an external identity") {
		t.Error("the refusal was not logged where an operator can read it")
	}
}

// An incomplete claim is refused before the service is reached here too.
func TestAnIncompleteLinkClaimNeverReachesTheService(t *testing.T) {
	minter := &agreeableMinter{}
	h, _ := identityHost(t, minter, nil)

	resp := assertAt(t, h, "/connect-no-subject", "", &auth.Identity{
		UserID: uuid.MustParse("0198c9c5-0000-7000-8000-000000000001"),
	})

	if !strings.Contains(resp.Body, "ErrInvalid") {
		t.Errorf("an incomplete link claim did not answer ErrInvalid: %q", resp.Body)
	}
	if len(minter.linked) != 0 {
		t.Errorf("an incomplete link claim reached the service: %+v", minter.linked)
	}
}

// A host with no session service answers StatusInternal rather than
// StatusNotAvailable, and the difference is what a module branches on: *this host
// does not have that function* and *this host could not do it* are different
// facts, and only the first is a reason to stop trying.
func TestAHostWithNoSessionServiceSaysSoRatherThanRefusingTheFunction(t *testing.T) {
	h, sink := identityHost(t, nil, nil)

	resp := assertAt(t, h, "/callback", "subject-1", nil)

	if !strings.Contains(resp.Body, "ErrInternal") {
		t.Errorf("a host with no session service answered %q, want ErrInternal", resp.Body)
	}
	if strings.Contains(resp.Body, "ErrNotAvailable") {
		t.Error("a host with no session service reported the function as unimplemented")
	}
	if !strings.Contains(sink.String(), "this host has no session service") {
		t.Error("the reason was not written where an operator can read it")
	}
}

// The record a module sees is derived here, from the identity the host resolved,
// and this is the assertion internal/httpx's own tests point at: what a signed-in
// visitor's request hands a module is who they are, and no credential of any kind.
func TestASignedInIdentityCrossesAsARecordAndNothingElseDoes(t *testing.T) {
	id := &auth.Identity{
		UserID:      uuid.MustParse("0198c9c5-0000-7000-8000-000000000001"),
		Email:       "owner@example.com",
		Name:        "Owner",
		WorkspaceID: uuid.MustParse("0198c9c5-0000-7000-8000-000000000002"),
		OrgID:       uuid.MustParse("0198c9c5-0000-7000-8000-000000000003"),
		SessionID:   uuid.MustParse("0198c9c5-0000-7000-8000-0000000000ff"),
		Role:        "owner",
	}
	got := RequestIn{Identity: id}.session()

	if !got.SignedIn || got.UserID != id.UserID.String() || got.Email != id.Email {
		t.Errorf("the record did not carry the identity: %+v", got)
	}
	if got.WorkspaceID != id.WorkspaceID.String() || got.OrganizationID != id.OrgID.String() {
		t.Errorf("the record did not carry where the request landed: %+v", got)
	}
	// The session id is the one field of an Identity that names the credential
	// rather than the person, and no field of the record may carry it.
	if strings.Contains(fmt.Sprintf("%+v", got), id.SessionID.String()) {
		t.Errorf("the session identifier crossed the boundary: %+v", got)
	}
	// And nobody is the zero value, with nothing left over from a previous caller.
	if empty := (RequestIn{}).session(); empty != (SessionContext{}) {
		t.Errorf("an anonymous request produced %+v", empty)
	}
}

// --- M65 / D292: a real random source and a real clock ----------------------

// The composition F292 filed, driven: a fresh instance per request (D260) and a
// deterministic random source together made **every visitor's** nonce the same,
// which is worse than either property alone.
//
// Three axes, because the constant was a compile-time one and so the stream was
// identical across all three: two draws inside one instance, two requests to one
// host, and two independently-opened hosts. The last of those is the one that
// would still have failed if wazero's source had merely been seeded once per
// runtime rather than at compile time.
func TestEveryRequestDrawsItsOwnEntropy(t *testing.T) {
	type drawn struct {
		ABIRandom      string `json:"abi_random"`
		ABIRandomLarge string `json:"abi_random_large"`
		StdRandom      string `json:"std_random"`
		ABINow         string `json:"abi_now"`
		StdNow         string `json:"std_now"`
	}
	draw := func(t *testing.T, h *Host) drawn {
		t.Helper()
		resp, err := get(t, h, "/entropy")
		if err != nil {
			t.Fatal(err)
		}
		var d drawn
		if err := json.Unmarshal([]byte(resp.Body), &d); err != nil {
			t.Fatalf("the fixture did not report a draw: %q", resp.Body)
		}
		if d.ABIRandom == "" || d.StdRandom == "" {
			t.Fatalf("a draw came back empty: %+v", d)
		}
		return d
	}

	first, _ := pagesHost(t, nil)
	second, _ := pagesHost(t, nil)

	a, b, c := draw(t, first), draw(t, first), draw(t, second)

	// Every pair of every value, across every axis. Written as a sweep rather than
	// as six comparisons because the failure this is about made *all* of them equal
	// and a test naming two of them would have reported a third of the defect.
	for label, values := range map[string][]string{
		"the ABI's random_bytes":      {a.ABIRandom, b.ABIRandom, c.ABIRandom},
		"a large ABI draw":            {a.ABIRandomLarge, b.ABIRandomLarge, c.ABIRandomLarge},
		"the guest's own crypto/rand": {a.StdRandom, b.StdRandom, c.StdRandom},
	} {
		seen := map[string]bool{}
		for _, v := range values {
			if seen[v] {
				t.Errorf("%s returned %q twice across three draws in two hosts; "+
					"every visitor is being handed the same value", label, v)
			}
			seen[v] = true
		}
	}
	// And the ABI's own draw is not the standard library's, which is what would
	// happen if both were reading one seeded stream a request at a time.
	if a.ABIRandom == a.StdRandom {
		t.Errorf("the ABI and crypto/rand returned the same 32 bytes in one request: %q", a.ABIRandom)
	}

	// The clock, and the assertion that can be made about it without asserting
	// that this machine's clock is right: wazero's fake one begins at
	// 2022-01-01T00:00:00Z and advances a millisecond per reading, so anything
	// after 2023 is not it. Both spellings, because a publisher writes the second.
	for label, stamp := range map[string]string{
		"the ABI's time_now":       a.ABINow,
		"the guest's own time.Now": a.StdNow,
	} {
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			t.Errorf("%s answered %q, which is not RFC 3339: %v", label, stamp, err)
			continue
		}
		if at.Year() < 2023 {
			t.Errorf("%s answered %q, which is wazero's fake clock rather than this machine's",
				label, stamp)
		}
		if d := time.Since(at); d > time.Minute || d < -time.Minute {
			t.Errorf("%s answered %q, %v away from the host's own clock", label, stamp, d)
		}
	}
}

// The bound random_bytes enforces, from the host side, so the refusal is asserted
// against the status rather than against the fixture's prose.
func TestRandomBytesRefusesACountItWillNotServe(t *testing.T) {
	h, _ := pagesHost(t, nil)
	st := h.hostState("pages")
	if st == nil {
		t.Fatal("no state for the pages fixture")
	}
	// mod is nil: every count below is refused before the out-buffer is touched,
	// which is the property being asserted — a refused draw writes nothing at all.
	for _, count := range []int32{0, -1, maxRandomBytes + 1, math.MaxInt32} {
		got := hostFuncs["random_bytes"](context.Background(), st, nil,
			[]uint64{api.EncodeI32(count), 0, 0})
		if got != int32(abi.StatusInvalid) {
			t.Errorf("random_bytes(%d) answered %d, want StatusInvalid (%d)",
				count, got, abi.StatusInvalid)
		}
	}
}

// **The only place in this package a module config is built**, asserted over the
// source rather than described in a comment.
//
// Two call sites exist today — the load-time instance and the per-request one —
// and the difference between them is invisible: a module drawing entropy would
// work at load and be predictable per request, or the reverse, with nothing
// failing and no test noticing. A third site added by a later milestone would get
// wazero's defaults, which are a constant random source and a clock that starts in
// 2022. This is the enumeration that cannot be forgotten, because it is a sweep
// rather than a list.
func TestOnlyOneModuleConfigIsBuilt(t *testing.T) {
	root := filepath.Join(repoRoot(t), "internal", "addon")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") {
			// Product code only. A test builds configs of its own on purpose — to
			// measure an instantiation, to construct an instance the host would not —
			// and none of those serves a visitor.
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name())) //nolint:gosec // G304: a file this test listed in the repository it lives in
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < strings.Count(string(b), "wazero.NewModuleConfig("); i++ {
			sites = append(sites, e.Name())
		}
	}
	if len(sites) != 1 || sites[0] != "host.go" {
		t.Errorf("wazero.NewModuleConfig( appears in %v; it belongs in guestModuleConfig "+
			"(host.go) and nowhere else, because a second site is a guest instance that "+
			"quietly gets wazero's fake clock and constant random source", sites)
	}
}

// --- M65: what happens when an authentication add-on will not load ----------

// **Anything on the authentication path defaults to `required`**, whatever the
// manifest says. It is a deliberate exception to *the add-on decides* (M60): the
// publisher of an authentication add-on cannot know whether the instance
// installing it has another way in, and the failure mode of guessing wrong is an
// instance that boots with sign-in silently missing.
func TestAnAddonThatMintsSessionsIsRequiredWhateverItsManifestSays(t *testing.T) {
	for _, tc := range []struct {
		name      string
		declared  []string
		class     FailureClass
		overrides map[string]string
		want      FailureClass
	}{
		{"a degrade auth add-on is required anyway",
			[]string{PermissionRoutes, PermissionSessionMint}, ClassDegrade, nil, ClassRequired},
		{"and a required one stays required",
			[]string{PermissionSessionMint}, ClassRequired, nil, ClassRequired},
		{"an add-on off the authentication path keeps its own answer",
			[]string{PermissionRoutes}, ClassDegrade, nil, ClassDegrade},
		{"the operator's override wins over the default",
			[]string{PermissionSessionMint}, ClassDegrade,
			map[string]string{"failure_class": "degrade"}, ClassDegrade},
		{"and over the manifest, in the other direction",
			[]string{PermissionRoutes}, ClassDegrade,
			map[string]string{"failure_class": "required"}, ClassRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Manifest{Name: "x", FailureClass: tc.class, Permissions: tc.declared}
			got, err := effectiveFailureClass(m, tc.overrides)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("the effective class is %q, want %q", got, tc.want)
			}
		})
	}
}

// An override this host cannot read is an error and never a fallback: the
// variable that decides whether this add-on may be skipped is the one that could
// not be read, so there is no answer to fall back to. It reaches an operator
// naming the variable, because a message that said "invalid failure class" would
// leave them looking for it in the manifest.
func TestAnUnreadableFailureClassOverrideStopsTheInstance(t *testing.T) {
	code := fixture(t, "minimal")
	dir := t.TempDir()
	m := manifestFor("minimal", ClassDegrade, code)
	install(t, dir, m, code)

	sink := &logSink{}
	_, err := Open(context.Background(), Options{
		Dir:       dir,
		Logger:    slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Overrides: func(string) map[string]string { return map[string]string{"failure_class": "maybe"} },
		Settings:  func(string, []string) map[string]config.Secret { return nil },
	})
	if err == nil {
		t.Fatal("an unreadable failure_class override let the instance boot; a degrade " +
			"add-on with an unreadable class is the one case where guessing decides " +
			"whether sign-in exists")
	}
	// The variable by name, because that is where the operator has to go.
	if !strings.Contains(err.Error(), config.AddonSettingVar("minimal", "failure_class")) {
		t.Errorf("the refusal does not name the variable: %v", err)
	}
	if !strings.Contains(err.Error(), "maybe") {
		t.Errorf("the refusal does not name what was set: %v", err)
	}
}

// The two reserved names are not settings, and a manifest declaring one is
// refused rather than resolved — the same answer D263 gave two add-on names whose
// environment variables collided, for the same reason: no lookup can tell which
// of the two `LINKCTRL_ADDON_OIDC_FAILURE_CLASS` was meant.
func TestAManifestCannotDeclareASettingThatIsAnOperatorsAnswer(t *testing.T) {
	for _, name := range config.AddonOverrideNames {
		t.Run(name, func(t *testing.T) {
			m := Manifest{
				SchemaVersion: SchemaVersion, Name: "reserved_names", Version: "1.0.0", ABIVersion: 1,
				Module: "x.wasm", SHA256: strings.Repeat("a", 64), FailureClass: ClassDegrade,
				Settings: []Setting{{Name: name, Type: SettingText}},
			}
			err := m.Validate()
			if err == nil {
				t.Fatalf("a manifest declared %q as a setting and loaded", name)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("the refusal does not say the name is reserved: %v", err)
			}
		})
	}
	// And a name that is not reserved still works, so the check is a list and not
	// a blanket refusal of settings.
	m := Manifest{
		SchemaVersion: SchemaVersion, Name: "reserved_names", Version: "1.0.0", ABIVersion: 1,
		Module: "x.wasm", SHA256: strings.Repeat("a", 64), FailureClass: ClassDegrade,
		Settings: []Setting{{Name: "client_id", Type: SettingText}},
	}
	if err := m.Validate(); err != nil {
		t.Errorf("an ordinary setting was refused: %v", err)
	}
}

// The second factor's default is the safe reading, and only the exact word turns
// it off. An operator who typed `yes` keeps being asked for the factor, which is
// what they meant even though it is not what they wrote.
func TestOnlyAnUnambiguousYesSaysAProviderMetTheSecondFactor(t *testing.T) {
	for value, want := range map[string]bool{
		"true": true, "": false, "TRUE": false, "yes": false, "1": false, "false": false,
	} {
		if got := mfaSatisfiedByProvider(map[string]string{"mfa_satisfied": value}); got != want {
			t.Errorf("mfa_satisfied=%q read as %v, want %v", value, got, want)
		}
	}
	if mfaSatisfiedByProvider(nil) {
		t.Error("an add-on with no override was read as satisfying the second factor")
	}
}

// And it reaches the assertion, per add-on, from the operator's environment and
// never from the manifest.
func TestTheOperatorsSecondFactorAnswerReachesTheAssertion(t *testing.T) {
	for _, satisfied := range []bool{false, true} {
		t.Run(fmt.Sprintf("mfa_satisfied=%v", satisfied), func(t *testing.T) {
			minter := &agreeableMinter{}
			code := fixture(t, "identity")
			dir := t.TempDir()
			m := manifestFor("identity", ClassDegrade, code)
			m.Permissions = []string{PermissionRoutes, PermissionSessionMint}
			install(t, dir, m, code)

			sink := &logSink{}
			overrides := map[string]string{}
			if satisfied {
				overrides["mfa_satisfied"] = "true"
			}
			h, err := Open(context.Background(), Options{
				Dir:       dir,
				Logger:    slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})),
				Sessions:  minter,
				Overrides: func(string) map[string]string { return overrides },
				Settings:  func(string, []string) map[string]config.Secret { return nil },
			})
			if err != nil {
				t.Fatalf("%v\n%s", err, sink.String())
			}
			t.Cleanup(func() { _ = h.Close(context.Background()) })

			assertAt(t, h, "/callback", "subject-1", nil)
			if minter.last.SatisfiesSecondFactor != satisfied {
				t.Errorf("the assertion carried satisfies_second_factor=%v, want %v",
					minter.last.SatisfiesSecondFactor, satisfied)
			}
		})
	}
}
