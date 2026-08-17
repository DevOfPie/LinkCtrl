// The QR tab's size control, and where a save on that tab comes back to
// (M50.8, D191, D193).
//
// **This is the first script this product's dashboard depends on**, and what it
// is allowed to contain is bounded in m50.8.md rather than left to whoever
// arrives next: it binds the two halves of the size control, and it puts the
// page back where it was after a write. Anything else wanting script on this
// dashboard is a decision, not a follow-on from this file.
//
// It is not the first script this product writes — static/js/docs.js has booted
// Swagger UI on /docs for two phases — and nothing about the policy moves for
// it. Served from the same directory under `script-src 'self'`, no Node, no
// CDN, no build step, no `unsafe-` waiver, which is the whole of what the
// inherited *`ui` stays stdlib-only* rule asks.
//
// **Delegated from `document`, not bound to the elements.** The QR tab arrives
// by an htmx swap of `#link-tabs` as often as by a page load, so anything that
// held a reference to `#qr_size` would be holding a node the next swap
// discarded. Listening on the document costs two handlers for the whole
// application and survives every swap without re-running.
//
// **And it runs with parsing blocked, in `<head>`, since the second reopening**
// (F246a, D196). Nothing below may touch `document.body` or any element at the
// top level: at the moment this executes the document is an open `<html>` and a
// `<head>`, and that is deliberate — it is the only moment from which the first
// paint can still be withheld. Everything that needs the document waits for
// `DOMContentLoaded` or is delegated from `document`.
(function () {
	"use strict";

	// --- the slider and the number ------------------------------------------
	//
	// One value, two inputs, and until this file nothing copied either into the
	// other: the number lied about what the slider would post until the moment
	// somebody saved. `httpx.requestedQRSize` still arbitrates on the server and
	// is still what a browser with this script blocked falls back to.
	//
	// **The number moves the slider only when it is a value the slider could
	// hold**, and that asymmetry is the one piece of judgement in here. A range
	// input clamps: typing 9999 into the box and mirroring it would leave the
	// slider on 2048, which is off the `size_shown` witness, so the server's rule
	// would take the slider and store a size nobody asked for. Leaving the slider
	// alone leaves it wherever it already was — and where that is still the
	// witness, which is every typed size on a form nobody has dragged, the box
	// wins the arbitration and the service refuses 9999 with a sentence naming
	// the range.
	//
	// Where the slider *has* been dragged, it is already off the witness and it
	// wins whatever the box says, so no refusal is produced and the dragged size
	// is stored. That is D182's arbitration, unchanged and out of this
	// milestone's spec — F240 carries it. Nothing here can fix it: the slider
	// cannot both hold a value it clamps and stay on a witness it left.
	function mirror(event) {
		var moved = event.target;
		if (!moved || !moved.id) {
			return;
		}
		var slider = document.getElementById("qr_size_slider");
		var number = document.getElementById("qr_size");
		if (!slider || !number) {
			return;
		}
		if (moved === slider) {
			number.value = slider.value;
			return;
		}
		if (moved !== number) {
			return;
		}
		var want = parseInt(number.value, 10);
		var lo = parseInt(slider.min, 10);
		var hi = parseInt(slider.max, 10);
		if (isNaN(want) || isNaN(lo) || isNaN(hi) || want < lo || want > hi) {
			return;
		}
		slider.value = String(want);
	}

	document.addEventListener("input", mirror);

	// --- where a save lands --------------------------------------------------
	//
	// Every write on this tab is a POST answered with a redirect, so the browser
	// loads a fresh page and starts it at the top — or at `#qr`, which the
	// link-page branch of `qrReturn` carries and which is the top of the QR card
	// rather than the control that was being used. The owner reported the
	// consequence: *"The whole page resets to the top every time the save button
	// is used… a force to top is jarring and bad UX."*
	//
	// **A remembered position rather than a fragment**, which is D193 and is
	// argued there: a fragment lands every reader on one element, and what was
	// asked for is *"keep its current position whenever possible"*. The position
	// is stored on the way out and applied once on the way in.
	//
	// `sessionStorage` because the value must not outlive the tab and must not
	// travel: it is a scroll offset, which is nobody's business but this
	// browser's. Every access is guarded — a browser with storage disabled or a
	// quota exhausted throws rather than returning null, and losing the scroll
	// position is not a reason to break the page.
	var key = "linkctrl:qr-scroll";

	function remember() {
		try {
			window.sessionStorage.setItem(key, JSON.stringify({
				path: window.location.pathname,
				y: Math.round(window.scrollY || window.pageYOffset || 0),
			}));
		} catch (e) {
			/* storage refused; the save still works, it just lands at the top */
		}
	}

	// Both ways a write leaves this tab. A native submit covers the style form,
	// the add prompt and every control on a row; `htmx:beforeRequest` covers the
	// logo upload, which posts on `change` and never fires a submit event at all.
	// Capture on the submit listener, because a handler that cancelled the event
	// on the way up would otherwise decide whether the position is stored.
	//
	// **The htmx half stopped being only writes at the third reopening**: a code
	// selection is an `hx-get` from a row inside `#qr`, so it stores a position
	// too. Harmless in itself — a swap disturbs no scroll and the offset is
	// forgotten below — and it is why forgetting had to stop depending on a swap
	// arriving (F250).
	document.addEventListener("submit", function (event) {
		var form = event.target;
		if (form && form.closest && form.closest("#qr")) {
			remember();
		}
	}, true);

	// **On `document`, not on `document.body`.** htmx's events bubble, so either
	// would receive them — but this file runs before the body exists since the
	// second reopening (D196), and `document.body.addEventListener` at that point
	// is a TypeError that would take the mirror above down with it.
	document.addEventListener("htmx:beforeRequest", function (event) {
		var from = event.target;
		if (from && from.closest && from.closest("#qr")) {
			remember();
		}
	});

	// **And forgotten again the moment it is clear no load is coming.** Only a
	// document load reads this back, so an offset stored for a request that never
	// produces one would sit in storage keyed to this pathname until *some* later
	// load of it — plausibly the reader's own reload half an hour on, which would
	// then jump to where they stood during a request they have forgotten about.
	//
	// **The end of the request is the proof, not the swap** (M50.8's fourth
	// reopening, F250, D205). This listened on `htmx:afterSwap`, on the argument
	// that a swap is the observable fact and does not depend on which htmx event
	// runs first. That argument is sound and answers the wrong question: a swap is
	// observable *when there is one*, and the requests this file has to forget for
	// are precisely the ones that produce none — a 5xx, a refusal htmx will not
	// swap, an abort, a timeout. Since the third reopening every row of the codes
	// list issues one of these requests, so the case went from a rare failed
	// upload to a click on this tab's most-used control. `htmx:afterRequest` fires
	// on every one of those endings, which is the fact that actually matches the
	// question.
	//
	// **One response is not an ending, and that is the whole of the guard.** htmx
	// answers a successful write here with `HX-Redirect`, and it fires
	// `afterRequest` *before* the navigation it just asked for — so an unguarded
	// listener would throw away the offset on exactly the load that exists to read
	// it back, which is every accepted logo upload. `HX-Refresh` is the same
	// promise by another name. `HX-Location` is deliberately not in the list: htmx
	// performs that one itself, over ajax, and no document load follows to restore
	// anything.
	function forget() {
		try {
			window.sessionStorage.removeItem(key);
		} catch (e) {
			/* storage refused; there was nothing to forget */
		}
	}

	// Whether the response promises the document load that reads the offset back.
	function loadIsComing(xhr) {
		if (!xhr || !xhr.getResponseHeader) {
			return false;
		}
		return !!(xhr.getResponseHeader("HX-Redirect") || xhr.getResponseHeader("HX-Refresh"));
	}

	document.addEventListener("htmx:afterRequest", function (event) {
		var detail = event.detail || {};
		if (!loadIsComing(detail.xhr)) {
			forget();
		}
	});

	// Taken out of storage once, on the way in, and cleared whether or not it is
	// ever applied: an offset that survived would put a later, unrelated load
	// somewhere nobody scrolled to.
	function take() {
		var raw = null;
		try {
			raw = window.sessionStorage.getItem(key);
			window.sessionStorage.removeItem(key);
		} catch (e) {
			return null;
		}
		if (!raw) {
			return null;
		}
		var at;
		try {
			at = JSON.parse(raw);
		} catch (e) {
			return null;
		}
		if (!at || at.path !== window.location.pathname || typeof at.y !== "number") {
			return null;
		}
		return at;
	}

	var pending = take();

	// **The fragment yields, rather than being out-scrolled afterwards**
	// (M50.8's reopening, F244(a), D195).
	//
	// This applied the restore *twice*, and the second application was not belt
	// and braces: the link page's redirect carries `#qr`, the browser performs
	// that fragment scroll of its own after `DOMContentLoaded`, and a correction
	// made after it is by construction a correction the reader watches happen.
	// The owner reported exactly that — *"immediately upon changing the default
	// the screen reloads at the top of the page then jumps down to match the
	// previous scroll position"* — and *a save returns you to where you were* is
	// not what a jump-and-settle looks like. Two mechanisms were aiming at one
	// scroll and the later one existed only to undo the earlier.
	//
	// So the fragment is taken **off the URL before the browser acts on it**.
	// `replaceState` runs from `restore` below, at `DOMContentLoaded`, which is
	// before the fragment scroll — the same ordering the second restore was
	// written against, used to prevent the jump instead of to correct it. With no
	// fragment left there is nothing else aiming at the scroll, so one application
	// holds and the reader sees one position. *(It was a `defer`red script that
	// gave that ordering until the second reopening; the file is parser-blocking
	// now and the handler is what carries the ordering, which is why this says
	// which event rather than which attribute.)*
	//
	// **The fragment stays on the redirect and that is deliberate** (D178). It is
	// what lands a reader with this script blocked on the QR card rather than at
	// the top of the page, so it is given up only on the loads where a better
	// position is already known — never when there is nothing to restore, and
	// never on a page the tab did not render on.
	function dropFragment() {
		if (window.location.hash !== "#qr") {
			return;
		}
		try {
			window.history.replaceState(window.history.state, "",
				window.location.pathname + window.location.search);
		} catch (e) {
			/* history refused; the fragment keeps the scroll and the reader lands
			   at the top of the card, which is where D178 alone put them — and
			   not where they were standing. D195 states that cost: the restore
			   below still runs, the fragment scroll still overrides it after
			   DOMContentLoaded, and since M50.8's reopening there is no second
			   application left to put it back. */
		}
	}

	// **The paint is held until the position has been applied** (M50.8's second
	// reopening, F246a, D196).
	//
	// The first reopening stopped the *fragment* from moving the page after the
	// restore, and left the restore itself where it was: at `DOMContentLoaded`,
	// reached from a `defer`red script. That is after the document is parsed and
	// therefore after it has been painted, so on any connection slower than
	// localhost the reader was still shown the top of the page first — the same
	// correction as before, from a different starting point. Measured at
	// `928504f` over a 80 ms / 4 Mbps / 2× CPU profile: eight or nine frames at
	// the wrong offset, about a third of a second of it.
	//
	// **Running earlier cannot fix it, and that is not a cost but an
	// impossibility.** This file now runs in `<head>` with parsing blocked, which
	// is before the first paint — and at that moment the document has no body, no
	// height and nothing to scroll: `scrollTo` clamps to 0 and the restore is
	// silently lost. A position can only be applied to a laid-out document, so the
	// only thing that can be done before the first paint is to *withhold* it.
	//
	// So the class goes on `<html>` here, at head time, and app.css keeps `body`
	// unpainted while it is there. It is added **only when there is an offset to
	// restore**, which is the load after a write and nothing else: every other
	// page in the dashboard paints exactly as it did. What it costs on that one
	// load is the interval between the first paint and `DOMContentLoaded` spent
	// on the page's background rather than on its content — which is the
	// alternative the owner reported as jarring, made invisible instead of made
	// correct, because correct is not available.
	//
	// **Three ways back to a painted page**, because a page that stays hidden is
	// far worse than a page that jumps. `restore` reveals in a `finally`, so
	// every path out of it — no `#qr`, a stale path, a throw — paints. `load`
	// reveals again in case `DOMContentLoaded` never arrives. And a timer reveals
	// regardless, four seconds being past any load this product's own slowest
	// measured profile produces.
	var holding = false;

	function hold() {
		if (!pending) {
			return;
		}
		document.documentElement.classList.add("qr-restoring");
		holding = true;
	}

	function reveal() {
		if (!holding) {
			return;
		}
		holding = false;
		document.documentElement.classList.remove("qr-restoring");
	}

	function restore() {
		try {
			if (!pending) {
				return;
			}
			// Only where the tab actually rendered. A refusal renders the page with
			// the section on it and is exactly the case where the reader wants their
			// place back; a redirect that landed somewhere else is not.
			if (!document.getElementById("qr")) {
				return;
			}
			dropFragment();
			window.scrollTo(0, pending.y);
			// Once. A position that outlived its own load would follow the reader
			// into whatever they did next.
			pending = null;
		} finally {
			reveal();
		}
	}

	hold();

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", restore);
	} else {
		restore();
	}
	window.addEventListener("load", reveal);
	window.setTimeout(reveal, 4000);
})();
