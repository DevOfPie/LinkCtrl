package abi

// Functions is the ABI: every function an add-on may import from this host.
//
// This slice is the enumeration m61.md requires "in one place". The SDK, the
// documentation table and the host module are all generated or derived from it,
// so a function added here appears on every side of the boundary and a function
// removed here disappears from all of them — which is the property that makes
// the deprecation policy enforceable rather than aspirational.
//
// Each function names the [Permission] it costs in Requires, or names none — the
// vocabulary is [Permissions] and the host refuses a call whose grant the calling
// add-on's manifest did not declare. Four functions are ungated on purpose, and
// permissions_test.go names them rather than counting them: abi_version reports a
// constant; log is the capability that was granted deliberately rather than by
// accident, since a module's stdout and stderr are discarded and it is the only way
// out; and random_bytes and time_now are host facts a module already reaches
// through crypto/rand and time.Now, which the host wires to the same sources, so a
// grant on either would be a line in every manifest that bought an operator
// nothing.
//
// Ten capability groups across eight declarable permissions, and they do not map
// one to one onto milestones: logging and
// config are M61's, storage is M63's, routes and the session *read* are M64's, the
// session *hook* and the two ungated host facts — entropy and the clock — are
// M65's, and the redirect group is M66's. All of those work. Template rendering
// from a module's own files is declared and
// **still refused** — M64 answered the rendering half by wrapping what a module
// returns rather than by parsing markup a module ships, which is D259 and is why
// the function that would parse it did not land with the milestone that backs it.
// It is the last one left. That order is deliberate — a module written against the
// whole contract compiles today, and the milestone that implements a limb turns
// a refusal into behaviour without changing a signature. m61.md's second risk is
// exactly this pattern rotting into permanently-refused, and the mitigation is
// the pair of tests over this slice plus M64.9 reading it against what M62–M64
// built.
//
// # The redirect group is three functions and two of them are not for the same caller
//
// `redirect_event_read` belongs to the **observe** class, which runs after the
// response and off the path. `redirect_decision_read` and `redirect_answer_write`
// belong to the **inline** class, which runs on it. They are separate functions
// rather than one read with a mode, because the two classes are separate grants
// for the owner's own reason — a module cannot acquire the sharper one by
// accident — and one function serving both would be a place where holding the
// weaker grant reached the stronger one's payload.
//
// **What an inline invocation may call is narrower than what its manifest
// declared**, and [InlineSafe] is the whole of it. That is the redirect tree's
// own rule reaching across the boundary: no storage, no request, no session and
// no template on the hot path, refused by the host at the call rather than
// promised about add-on code.
var Functions = []Function{
	{
		Name: "abi_version", Go: "HostABIVersion", Since: "0.1.0", BackedBy: "M61", Live: true,
		Params: []Param{
			{Name: "version", Kind: OutString, Doc: "the host's ABI version, as SemVer"},
		},
		Doc: "HostABIVersion is the ABI version of the host this module is running in. " +
			"A module's manifest declares the generation it was built against and the host " +
			"refuses a mismatch before instantiation, so this is not how a module checks " +
			"compatibility — it is how one logs what it is talking to, and how it decides " +
			"whether a function added in a later patch is worth probing for.",
	},
	{
		Name: "log", Go: "Log", Since: "0.1.0", BackedBy: "M61", Live: true,
		Params: []Param{
			{Name: "level", Kind: String, Doc: "one of the Level constants"},
			{Name: "message", Kind: String, Doc: "the line, without a trailing newline"},
		},
		Doc: "Log writes one line to the host's logger, attributed to this add-on. " +
			"It is the only way out: a module's stdout and stderr are discarded, because " +
			"routing them into an operator's log is a capability and the host grants none " +
			"it was not asked for. The host adds the add-on's name; a message that repeats " +
			"it is noise. An unknown level is ErrInvalid rather than a silent default, so a " +
			"typo does not become a line nobody greps for. The message is neutralized " +
			"before it is written and bounded at 4 KiB, and the rule is stated as what " +
			"survives rather than as what is caught: a graphic character reaches the line " +
			"as itself, in any script, and everything else becomes its escape — a newline, " +
			"a control character, an ANSI escape, every format and bidirectional control, " +
			"every unassigned or private-use code point, and the 268 graphic code points " +
			"this host treats as invisible: the 267 graphic members of Unicode's derived " +
			"Default_Ignorable_Code_Point, which the host computes rather than reads " +
			"because Go ships only the residue property the derivation subtracts from, " +
			"plus U+2800 BRAILLE PATTERN BLANK, the one blank that is not whitespace. " +
			"One class is deleted rather than escaped, and it is the only one: every " +
			"variation selector is removed from the message. So a heart written as U+2764 " +
			"U+FE0F arrives as U+2764 and is still a heart, an emoji that carries no " +
			"selector is untouched, and a selector hung off a letter, a space, an " +
			"ideograph or a block element takes nothing with it when it goes. There is no " +
			"exemption and no base list: a selector after a character the reader's " +
			"renderer does not vary is invisible, and no property tells the host which " +
			"those are. That set is a published property and not the set of characters " +
			"that render as nothing, because Unicode publishes no such property: eight " +
			"combining marks it annotates as not visibly rendered — U+2D7F, U+17D2, " +
			"U+10A3F, U+1107F, U+11A47, U+11A99, U+11F42 and U+16FE4 — reach the line as " +
			"themselves, as do seventeen space characters and the prepended concatenation " +
			"marks named below. What bounds that residue is that this log is write-only to " +
			"you: Log declares no out-parameter, no function in this ABI hands log content " +
			"back, your module gets no preopened file and its stdout and stderr are " +
			"discarded, and your storage is a schema this log does not live in. So a " +
			"character that survives is one an operator can still see; it is not a channel " +
			"you can read back. A code point Unicode " +
			"adds after the host was built is escaped rather than let through. One graphic " +
			"character does not reach the line as itself: a backslash is doubled, so that " +
			"the two characters \\ and n cannot be mistaken for an escaped newline, and a " +
			"module cannot spell the host's own truncation mark. The named exceptions run " +
			"the other way: Unicode's prepended concatenation marks — the Arabic, Syriac " +
			"and Kaithi signs that scope the digits after them — are left alone, read from " +
			"Unicode's property rather than from a list, so a host built against a newer " +
			"revision carries the marks it added. Nothing is refused for any of it, and a " +
			"message that needed none arrives as it was written, backslashes aside.",
	},
	{
		Name: "random_bytes", Go: "RandomBytes", Since: "0.1.1", BackedBy: "M65", Live: true,
		Params: []Param{
			{Name: "count", Kind: Int32, Doc: "how many bytes to draw, at most 4096"},
			{Name: "bytes", Kind: OutBytes, Doc: "the bytes, drawn from the host's own source"},
		},
		Doc: "RandomBytes draws bytes from the operating system's entropy source, through " +
			"the host. It is what a nonce, a `state` parameter or a PKCE verifier is built " +
			"from. A count outside 1..4096 is ErrInvalid rather than a clamped answer, " +
			"because a caller that asked for the wrong number of bytes wanted a different " +
			"number and not a shorter one. Nothing about this function is a permission: " +
			"every module already reaches the same source through crypto/rand, which the " +
			"host wires to the same reader, so gating it would buy an operator nothing and " +
			"cost every manifest a line.",
	},
	{
		Name: "time_now", Go: "TimeNow", Since: "0.1.1", BackedBy: "M65", Live: true,
		Params: []Param{
			{Name: "now", Kind: OutString, Doc: "the host's wall clock, RFC 3339 with nanoseconds, in UTC"},
		},
		Doc: "TimeNow is the host's wall clock, which is this machine's. It is what an " +
			"expiry is compared against and what a record's timestamp is stamped from. " +
			"UTC and RFC 3339, so there is one spelling to parse and no zone to guess. " +
			"Ungated for the reason random_bytes is: a module already reads the same clock " +
			"through time.Now, and this is the same value with a documented shape.",
	},
	{
		Name: "config_get", Go: "ConfigGet", Since: "0.1.0", BackedBy: "M61", Live: true,
		Requires: "config.read",
		Params: []Param{
			{Name: "key", Kind: String, Doc: "the name of a setting this add-on's manifest declares"},
			{Name: "value", Kind: OutString, Doc: "the value, or the declared default"},
		},
		Doc: "ConfigGet reads one of this add-on's own settings. The key must be one the " +
			"add-on's manifest declares; anything else is ErrDenied, which is what scopes " +
			"the function to the add-on rather than to the instance — there is no way to " +
			"ask for another add-on's setting or for one of this product's own " +
			"configuration values. A declared setting with no value yet answers with the " +
			"default the manifest gave it, and ErrNotFound only when it declared none. " +
			"An operator sets a value with LINKCTRL_ADDON_<NAME>_<SETTING> and it outranks " +
			"the manifest's default; editing them in the Add-on manager is M68's, and until " +
			"that lands the environment is the only way to set one.",
	},
	{
		Name: "storage_query", Go: "StorageQuery", Since: "0.1.0", BackedBy: "M63", Live: true,
		Requires: PermissionStorage,
		Params: []Param{
			{Name: "sql", Kind: String, Doc: "a statement against this add-on's own schema"},
			{Name: "args", Kind: Bytes, Doc: "positional arguments, as a JSON array", GuestShaped: true},
			{Name: "rows", Kind: OutBytes, Doc: "the result, as a JSON array of objects", GuestShaped: true},
		},
		Doc: "StorageQuery runs a read against the Postgres schema this add-on owns. " +
			"The schema boundary is the whole of the permission: an add-on names no " +
			"database, no connection and no search_path, and a statement that reaches " +
			"outside its own schema is refused rather than executed — ErrDenied, which " +
			"is distinguishable from ErrInvalid so that a module can tell confinement " +
			"from its own mistake. One statement per call: the host parses through the " +
			"extended protocol, so a payload carrying two is refused. The read is a " +
			"read at the server, in a READ ONLY transaction, so this function cannot be " +
			"used to write. Arguments are a JSON array of strings, numbers, booleans " +
			"and nulls; pass JSON as a string and cast it. Rows come back as a JSON " +
			"array of objects keyed by column name, and a result with two columns of " +
			"one name is refused rather than collapsed.",
	},
	{
		Name: "storage_exec", Go: "StorageExec", Since: "0.1.0", BackedBy: "M63", Live: true,
		Requires: PermissionStorage,
		Params: []Param{
			{Name: "sql", Kind: String, Doc: "a statement against this add-on's own schema"},
			{Name: "args", Kind: Bytes, Doc: "positional arguments, as a JSON array", GuestShaped: true},
		},
		Doc: "StorageExec runs a write against the Postgres schema this add-on owns. " +
			"Migrations are not this function: the host runs an add-on's migrations, which " +
			"is what keeps *DDL is additive within a minor version* a promise somebody can " +
			"keep — the add-on ships them in its own `migrations/` directory and names " +
			"each with its digest in the manifest, and the host applies them at load " +
			"inside the same schema this function writes to. Everything StorageQuery " +
			"says about the boundary, the single statement and the arguments applies " +
			"here too; what differs is that the transaction is not read-only.",
	},
	{
		Name: "http_request_read", Go: "HTTPRequestRead", Since: "0.1.0", BackedBy: "M64", Live: true,
		Requires: "routes.own_prefix",
		Params: []Param{
			{Name: "request", Kind: OutBytes, Doc: "the request, as an HTTPRequest record"},
		},
		Carries: []string{"HTTPRequest"},
		Doc: "HTTPRequestRead reads the request that reached one of this add-on's routes. " +
			"It answers ErrNotFound outside a request, which is what a module calling it " +
			"from package initialization gets — an instance is made per request and its " +
			"initialization runs before the request is attached, so this is the ordinary " +
			"answer during init rather than an edge case. Read twice in one request it " +
			"answers the same record twice: the host holds it, the guest does not consume it.",
	},
	{
		Name: "http_response_write", Go: "HTTPResponseWrite", Since: "0.1.0", BackedBy: "M64", Live: true,
		Requires: "routes.own_prefix",
		Params: []Param{
			{Name: "response", Kind: Bytes, Doc: "the response, as an HTTPResponse record"},
		},
		Carries: []string{"HTTPResponse"},
		Doc: "HTTPResponseWrite answers the request that reached one of this add-on's " +
			"routes. Called twice for one request it is ErrInvalid: a response is one " +
			"record, not a stream, because a module that can hold a connection open is a " +
			"module that can hold every connection open. What the record may carry is " +
			"bounded by the host and not by the module: `content_type` is a closed " +
			"vocabulary that does not include text/html, because the host wraps a page " +
			"and an add-on that could choose the type could choose markup; `location` is " +
			"answered 302 and never a permanent redirect; and `set_cookie` is bounded by " +
			"the prefixes the manifest declares and by a `max_age` of at most 400 days, " +
			"with the host's own Secure, HttpOnly and SameSite attributes applied. Each of " +
			"those is ErrInvalid rather than a " +
			"silently corrected response. The cookies themselves are carried in one cookie " +
			"of the host's rather than written individually, so what an add-on occupies in " +
			"a browser does not grow with what it sets or with how often it is visited — a " +
			"set too large to pack into one is ErrInvalid at this call.",
	},
	{
		Name: "template_render", Go: "TemplateRender", Since: "0.1.0", BackedBy: "M64",
		Requires: "routes.own_prefix",
		Params: []Param{
			{Name: "name", Kind: String, Doc: "a template this add-on shipped"},
			{Name: "data", Kind: Bytes, Doc: "the template's data, as a JSON object", GuestShaped: true},
			{Name: "html", Kind: OutBytes, Doc: "the rendered fragment"},
		},
		Doc: "TemplateRender renders one of this add-on's own templates through the host's " +
			"renderer, so a page an add-on draws inherits the product's escaping, its theme " +
			"tokens and its Content-Security-Policy. It is also how an add-on reaches the " +
			"page without bringing a front-end toolchain: it renders nothing itself. A " +
			"host that does not implement it yet answers ErrNotAvailable.",
	},
	{
		Name: "session_context", Go: "SessionContextRead", Since: "0.1.0", BackedBy: "M64", Live: true,
		Requires: "session.context",
		Params: []Param{
			{Name: "context", Kind: OutBytes, Doc: "who is signed in, as a SessionContext record"},
		},
		Carries: []string{"SessionContext"},
		Doc: "SessionContextRead asks the host who is signed in on the request this add-on " +
			"is answering. It is the *read* half of the session boundary and the whole of " +
			"it: what comes back is an identity and where it is working, never a cookie, a " +
			"token or a session row, so an add-on can draw a page for the person in front " +
			"of it and cannot act as them anywhere else. Nobody signed in is not an error " +
			"— add-on routes are reachable without a session, because a sign-in flow could " +
			"not otherwise begin — so the record's `signed_in` is false and every other " +
			"field is empty. Outside a request it is ErrNotFound, which is what a module " +
			"calling it from package initialization gets. Minting a session is " +
			"session_mint and is a different grant.",
	},
	{
		Name: "session_mint", Go: "SessionMint", Since: "0.1.0", BackedBy: "M65", Live: true,
		Requires: "session.mint",
		Params: []Param{
			{Name: "claim", Kind: Bytes, Doc: "who authenticated, as a SessionClaim record"},
			{Name: "session", Kind: OutBytes, Doc: "what the host minted, as a MintedSession record"},
		},
		Carries: []string{"SessionClaim", "MintedSession"},
		Doc: "SessionMint tells the host that this add-on authenticated somebody, and asks " +
			"for a session. The add-on does not make a session and never sees a token: it " +
			"makes an assertion, the host decides whether an account exists for it and what " +
			"the session may do, and the cookie is written by the host. That split is what " +
			"keeps the host, and not an add-on, the authority over who is signed in. What " +
			"comes back is a MintedSession, and it is enumerated for the same reason the " +
			"claim is: an answer described only as \"a JSON object\" is an answer the " +
			"credential assertion over this surface cannot read. Four host rules decide " +
			"whether anything is minted, and each is a status rather than a page: the claim " +
			"must name a subject and an issuer (ErrInvalid); that subject must already be " +
			"linked to an account, through a linking flow the host owns and this function is " +
			"not (ErrNotFound); the account must be active and not locked out (ErrDenied); " +
			"and nobody may already be signed in on the request, because a mint is how " +
			"somebody signs in and not how a browser changes who it is (ErrDenied). Called " +
			"twice in one request the second is ErrInvalid, for the reason " +
			"http_response_write is. An account with a second factor enrolled meets it after " +
			"this call rather than instead of it: the host answers with " +
			"second_factor_required set, and sends the visitor to its own prompt before your " +
			"response's location. **What that replaces is your response, and not your " +
			"cookies**: every set_cookie you made on the request is written to the browser " +
			"either way, so a callback that clears the `state` cookie it set at the start " +
			"clears it for an account with a second factor exactly as for one without. You " +
			"cannot see which kind of account you asserted about, so nothing about your " +
			"flow's own state may depend on it. **The out buffer is checked before anything is minted**, " +
			"which is the one place this ABI's retry convention needs saying twice: a " +
			"buffer too small for the record answers with the size to retry at and mints " +
			"nothing, so the retry is the first mint rather than a second one and the " +
			"sentence above about the second call keeps meaning what it says. A buffer of " +
			"zero, offered to ask for the size, costs nothing for the same reason. The " +
			"generated SDK starts larger than the record and never sees it.",
	},
	{
		Name: "identity_link", Go: "IdentityLink", Since: "0.1.1", BackedBy: "M65", Live: true,
		Requires: "session.mint",
		Params: []Param{
			{Name: "claim", Kind: Bytes, Doc: "who was authenticated, as a SessionClaim record"},
		},
		Carries: []string{"SessionClaim"},
		Doc: "IdentityLink connects an external identity to the account of the person who " +
			"is **already signed in** on this request, and it is the only way anything an " +
			"add-on does writes that mapping. It is session_mint's mirror and its " +
			"precondition: a subject nobody has linked mints nothing, and a subject can " +
			"only be linked while its owner is in front of the browser. So the two " +
			"functions have opposite requirements — this one is ErrDenied when nobody is " +
			"signed in, and session_mint is ErrDenied when somebody is — which is what " +
			"stops either being used to do the other's job. Linking the same subject to " +
			"the same account twice succeeds and changes nothing; linking one another " +
			"account already holds is ErrDenied and never moves it, because a link is a " +
			"credential and re-pointing one is the takeover this table exists to prevent. " +
			"An API key is not a person and cannot be the signed-in party. " +
			"**Your callback still needs its own CSRF defence.** The host's guarantee is " +
			"that a link is only ever made for whoever is signed in, in their own " +
			"browser, at that moment; whether that browser meant to be there is what " +
			"OAuth's `state` parameter is for, and it is yours.",
	},
	{
		Name: "redirect_event_read", Go: "RedirectEventRead", Since: "0.1.0", BackedBy: "M66", Live: true,
		Requires: "redirect.observe",
		Params: []Param{
			{Name: "event", Kind: OutBytes, Doc: "the redirect, as a RedirectEvent record"},
		},
		Carries: []string{"RedirectEvent"},
		Doc: "RedirectEventRead reads the redirect this add-on is observing. What it carries " +
			"is at most what click_events may carry — prefix-derived and country-level, and " +
			"no client address in any form. The grant it costs is redirect.observe, which " +
			"is out-of-band observation and nothing more: running inside the redirect path " +
			"itself is redirect.inline, a separate declaration, so a module cannot reach " +
			"the path by holding this. The host calls your " +
			"`linkctrl_redirect_observe` export once per recorded redirect, **after the " +
			"visitor has already been answered and after the click is durable**, so " +
			"nothing you do here can delay or fail a redirect — and nothing you do here " +
			"can affect one either. Outside such an invocation it is ErrNotFound, which " +
			"is what a module calling it from package initialization gets. An instance " +
			"that could not be given the event within the host's own bound is dropped " +
			"rather than queued: observation is best-effort by construction, exactly as " +
			"the click pipeline it is fed from is.",
	},
	{
		Name: "redirect_decision_read", Go: "RedirectDecisionRead", Since: "0.1.2", BackedBy: "M66", Live: true,
		Requires: "redirect.inline",
		Params: []Param{
			{Name: "decision", Kind: OutBytes, Doc: "where this visitor is about to be sent, as a RedirectDecision record"},
		},
		Carries: []string{"RedirectDecision"},
		Doc: "RedirectDecisionRead reads the redirect this module is being asked about, " +
			"while the visitor waits. The host calls your `linkctrl_redirect_inline` " +
			"export after it has decided where the visitor goes and **before it has " +
			"written anything** — before the gates that spend a link's budget, so a veto " +
			"costs nobody a click. What crosses is the decision and not the visitor: the " +
			"link, the alias and the destination, and no field derived from the person in " +
			"front of the browser. Watching visitors is redirect.observe's job and it " +
			"happens off this path. Outside an inline invocation this is ErrNotFound.",
	},
	{
		Name: "redirect_answer_write", Go: "RedirectAnswerWrite", Since: "0.1.2", BackedBy: "M66", Live: true,
		Requires: "redirect.inline",
		Params: []Param{
			{Name: "answer", Kind: Bytes, Doc: "your verdict, as a RedirectAnswer record"},
		},
		Carries: []string{"RedirectAnswer"},
		Doc: "RedirectAnswerWrite is how an inline module answers, and it is the only " +
			"channel it has: `linkctrl_redirect_inline` returns a status and not a " +
			"payload, for the reason the request handler does. Not calling it is the " +
			"ordinary case and means *allow* — a module that only watches writes nothing, " +
			"and a module the host had to kill wrote nothing either, so the two agree. " +
			"A verdict of `veto` refuses the visitor with the same page a gate refuses " +
			"with; the alias, the destination and the reason are never echoed to them. " +
			"A `query` alters the destination's query string and costs " +
			"redirect.rewrite_query on top of redirect.inline — ErrDenied without it — " +
			"and it is a **replacement** rather than a merge: what you write is the whole " +
			"query, and an empty string with `rewrite` set removes it. You cannot reach " +
			"the scheme, the host, the port or the path, because the host substitutes " +
			"your query into the URL it already decided rather than accepting a URL from " +
			"you. A query carrying anything outside RFC 3986's query characters is " +
			"ErrInvalid, and so is a verdict outside the vocabulary. Called twice in one " +
			"invocation the second is ErrInvalid, for the reason http_response_write is.",
	},
}

// Records is every structured payload the ABI carries.
//
// Enumerated for the same reason the functions are: the privacy property m61.md
// asks for is a property of the *surface*, and a surface described only by prose
// cannot be asserted by a test. abi_test.go walks this slice.
var Records = []Record{
	{
		Name: "RedirectEvent",
		Doc: "One redirect this instance served, handed to an observing add-on. " +
			"Every field is one click_events may carry, which is asserted rather than " +
			"promised: the test reads the column list out of the migration.",
		ClickDerived: true,
		Fields: []Field{
			{"link_id", "string", "the link, as a UUID"},
			{"workspace_id", "string", "the workspace the link belongs to, as a UUID"},
			{"occurred_at", "string", "RFC 3339, from the host's clock — the instant this instance served the redirect, not the instant a module read the record"},
			{"visitor_hash", "string", "the daily-salted visitor hash, hex — irreversible once the day's salt is purged, and not joinable across workspaces"},
			{"is_first_visit", "boolean", "as stored: dormant, and therefore always false"},
			{"country", "string", "ISO 3166-1 alpha-2, and the finest location this ABI carries"},
			{"device", "string", "device class"},
			{"browser", "string", "browser family"},
			{"os", "string", "operating-system family"},
			{"language", "string", "the primary Accept-Language tag"},
			{"referrer_host", "string", "the referrer's host only; the full URL is discarded at the edge"},
			{"is_bot", "boolean", "whether the request was classified as a bot"},
		},
	},
	{
		Name: "RedirectDecision",
		Doc: "Where a visitor is about to be sent, handed to an inline add-on before " +
			"anything is written. **Deliberately not click-derived**: every field here is " +
			"a property of the link and of the decision this instance made, and not one " +
			"of them describes the person waiting. An inline module is on the hot path " +
			"and holds the visitor's own request open, which is the worst place in this " +
			"product to hand anything about them over — and it would buy nothing that " +
			"RedirectEvent does not already carry off the path, under a grant an operator " +
			"declares separately.",
		Fields: []Field{
			{"link_id", "string", "the link, as a UUID"},
			{"workspace_id", "string", "the workspace the link belongs to, as a UUID"},
			{"alias", "string", "the short code this request resolved, canonicalised"},
			{"destination", "string", "the absolute URL this visitor is about to be sent to, " +
				"as the host has decided it — routing rules, split arms, deep-link path and " +
				"forwarded query all already applied, so it is the Location header and not " +
				"the link's stored URL"},
		},
	},
	{
		Name: "RedirectAnswer",
		Doc: "What an inline add-on answers with. Every field is optional and the empty " +
			"record means *allow, unchanged*, which is what a module that only watches " +
			"writes and what the host assumes of a module that wrote nothing at all.",
		Fields: []Field{
			{"verdict", "string", "allow or veto; empty is allow. A veto is answered with the " +
				"gate refusal page and nothing about the add-on reaches the visitor"},
			{"rewrite", "boolean", "whether query is to be applied at all, which is what makes " +
				"removing a query expressible: an empty query with this false is a module " +
				"that did not ask for a rewrite, and an empty query with it true is a module " +
				"asking for the query to be dropped"},
			{"query", "string", "the destination's new query string, without the leading `?`. " +
				"It replaces the query the host decided and reaches nothing else about the " +
				"URL, which the host enforces by substitution rather than by inspection"},
		},
	},
	{
		Name: "HTTPRequest",
		Doc: "A request that reached one of an add-on's routes. The header set is an " +
			"allowlist and not a map: every address-bearing header — Forwarded, " +
			"X-Forwarded-For, X-Real-IP and the CDN spellings beside them — is absent, " +
			"because handing them over would put a client address across this boundary " +
			"through a field nobody called an address. Cookies reach an add-on because " +
			"an authentication flow cannot work without them, and only the ones it " +
			"declared a prefix for: this product's sessions are server-side and opaque, " +
			"so the Cookie header is the credential rather than a description of one.",
		PrefixedCookies: true,
		Fields: []Field{
			{"method", "string", "the HTTP method"},
			{"path", "string", "the path within the add-on's own route prefix"},
			{"query", "string", "the raw query string"},
			{"cookies", "object", "the cookies whose names begin with one of the prefixes this " +
				"add-on's manifest declares, by name — and nothing else, so no prefix an " +
				"add-on may declare reaches a cookie of the host's"},
			{"content_type", "string", "the request's Content-Type"},
			{"accept_language", "string", "the request's Accept-Language"},
			{"body", "string", "the body, base64 when body_base64 says so"},
			{"body_base64", "boolean", "whether body is base64 rather than text; it is true " +
				"exactly when the request's own body was not valid UTF-8, and it exists " +
				"because a guest cannot otherwise tell an encoded body from one that " +
				"happens to look encoded"},
		},
	},
	{
		Name: "HTTPResponse",
		Doc: "What an add-on answers a request with, and what the host will let it. " +
			"`content_type` is a closed vocabulary — text/plain and application/json — " +
			"and **text/html is deliberately absent**: leaving it empty is the ordinary " +
			"case and means the host wraps the body in the dashboard's own page, escaped, " +
			"which is what makes \"an add-on cannot inject markup\" a property of this " +
			"record rather than of a filter somewhere. Every bound here is checked when " +
			"the record is written, so a module learns it was refused from the call it " +
			"made rather than from a page that differs from what it asked for.",
		Fields: []Field{
			{"status", "number", "the HTTP status code"},
			{"content_type", "string", "the response's Content-Type"},
			{"location", "string", "for a redirect; never a permanent one, which the host enforces rather than trusts"},
			{"set_cookie", "array", "cookies to set, bounded by the same prefixes the manifest " +
				"declares — a namespace an add-on owns is one it owns in both directions, or " +
				"it could overwrite a cookie it is not allowed to read; the host applies its " +
				"own Secure, HttpOnly and SameSite attributes, and packs the whole set into " +
				"one cookie of its own so that an add-on's share of a browser's cookie store " +
				"is fixed rather than chosen"},
			{"body", "string", "the body, as UTF-8 text — this direction carries no encoded " +
				"form, because the content types an add-on may name are text and a flag " +
				"saying otherwise would be a flag with nothing behind it"},
		},
	},
	{
		Name: "SessionContext",
		Doc: "Who is signed in on the request an add-on is answering, and where they are " +
			"working. It is deliberately not the session: no cookie, no token and no " +
			"session identifier crosses, because this product's sessions are opaque " +
			"server-side rows and the cookie *is* the credential (D232) — an add-on handed " +
			"one could act as whoever is signed in, without escaping the sandbox. What is " +
			"here is what a page needs in order to be drawn for somebody: who they are, " +
			"and which tenant and workspace their request landed in. Every field is empty " +
			"and `signed_in` is false when nobody is, which is the ordinary state of a " +
			"route that begins an authentication flow.",
		Fields: []Field{
			{"signed_in", "boolean", "whether anybody is signed in at all; false makes every field below empty"},
			{"user_id", "string", "the account, as a UUID — stable, and the only identifier of a person this record carries"},
			{"email", "string", "the account's email address"},
			{"display_name", "string", "the person's name, for display"},
			{"workspace_id", "string", "the workspace this request landed in, as a UUID"},
			{"organization_id", "string", "the organization that workspace belongs to, as a UUID — the tenant, which a workspace is not"},
			{"role", "string", "the role held in that organization: owner, admin, editor or viewer"},
		},
	},
	{
		Name: "SessionClaim",
		Doc: "An add-on's assertion that somebody authenticated. It is a claim and not a " +
			"session: the host decides whether an account exists for this subject, what " +
			"role it holds and how long the session lives.",
		Fields: []Field{
			{"subject", "string", "the identity provider's stable identifier for the person"},
			{"issuer", "string", "which provider asserted it"},
			{"email", "string", "the person's email address, as the provider gave it"},
			{"email_verified", "boolean", "whether the provider says it verified that address"},
			{"display_name", "string", "the person's name, for display"},
			{"groups", "array", "provider groups, for whatever mapping M65 decides on"},
		},
	},
	{
		Name: "MintedSession",
		Doc: "What the host hands back when it accepted a claim and minted a session. It " +
			"is deliberately not the session: no token, no cookie and no row of the " +
			"sessions table crosses, because the host writes the cookie itself and an " +
			"add-on able to read one would be able to replay it. What is here is what an " +
			"add-on's own response depends on, and every field traces to a decision m65.md " +
			"already states is the host's; a field M65 finds it needs is additive.",
		Fields: []Field{
			{"expires_at", "string", "RFC 3339, when this session stops being one — how long a " +
				"session lives is the host's decision and not the claim's"},
			{"second_factor_required", "boolean", "whether the person still owes a second " +
				"factor: an account with TOTP enrolled meets it after an add-on's assertion " +
				"rather than instead of it, so this is what an add-on has to read before it " +
				"decides the page it sends them to"},
		},
	},
}
