// The Add-on manager's select-mode counter (M68).
//
// **What it does, and the whole of what it does.** In select mode the removal
// button reads "Remove selected"; this puts the count in it — "Remove selected
// (2)" — as boxes are ticked. That is the owner's confirmed wireframe, and it is
// the one part of it a server-rendered page cannot produce, because the count
// changes without a request.
//
// **Everything it enhances already works without it.** With this file blocked the
// button reads "Remove selected", the form posts the ticked names, and the
// confirmation page says "Remove 2 add-ons?" — which is where the number that
// matters has always been. Nothing here is load-bearing, and nothing here is
// allowed to become load-bearing: an interaction that only works with script is a
// decision, and it is not this file's to take. The same bound qr-size.js is
// written under.
//
// Served from the same directory htmx, the /docs initialiser and qr-size.js come
// from, under `script-src 'self'`. No Node, no CDN, no build step, no `unsafe-`
// waiver, which is the whole of what the inherited *`ui` stays stdlib-only* rule
// asks.
//
// **Delegated from `document`, not bound to the boxes**, for the reason qr-size.js
// is: the manager's table is an ordinary page load today and an htmx swap the
// moment somebody adds one, and a handler holding a node reference would be
// holding a node the swap discarded. Two listeners for the whole application,
// surviving every swap without re-running.
//
// **Deferred, unlike its neighbour qr-size.js**, which is why nothing below runs
// at the top level: `defer` means this executes after the document is parsed, and
// header_test.go asserts the attribute in both directions — qr-size.js must not
// have it, and this must. It relabels a button nobody has pressed yet, so there is
// no first paint for it to block, and the delegation above is what makes the
// timing irrelevant either way: two listeners on `document`, which exists before
// either script does.
(function () {
	"use strict";

	// The button's own words, read off the DOM the first time rather than
	// duplicated here. A label written in two places is a label that will be
	// changed in one of them, and this file must never be the reason the page
	// says something the template does not.
	var LABEL = null;

	function button() {
		return document.querySelector("[data-addon-remove-button]");
	}

	function update() {
		var b = button();
		if (!b) {
			return;
		}
		if (LABEL === null) {
			LABEL = b.textContent.trim();
		}
		var n = document.querySelectorAll("[data-addon-select]:checked").length;
		// Zero renders the bare label rather than "(0)". A count of nothing is not
		// information, and the button is about to explain itself on the next page
		// anyway.
		b.textContent = n > 0 ? LABEL + " (" + n + ")" : LABEL;
	}

	document.addEventListener("change", function (e) {
		if (e.target && e.target.matches && e.target.matches("[data-addon-select]")) {
			update();
		}
	});

	// The initial pass, for the case a browser restored ticked boxes on a back
	// navigation. Without it the button would say "Remove selected" over a table
	// with two rows ticked, which is the one state this file exists to prevent.
	document.addEventListener("DOMContentLoaded", update);
})();
