// Swagger UI initializer for /docs.
//
// A separate file because the CSP allows no inline scripts, on /docs or
// anywhere else — the stock Swagger UI index.html boots from an inline
// <script>, which is exactly the pattern the policy exists to forbid. The spec
// URL arrives via a data attribute so this file stays static and cacheable.
(function () {
	"use strict";

	function boot() {
		var mount = document.getElementById("swagger-ui");
		if (!mount || typeof window.SwaggerUIBundle !== "function") {
			return;
		}
		window.SwaggerUIBundle({
			url: mount.dataset.specUrl,
			dom_id: "#swagger-ui",
			deepLinking: true,
			// No StandalonePreset: the top bar it adds is a URL box for
			// loading arbitrary specs, which an embedded viewer pointing at
			// its own API has no use for.
			presets: [window.SwaggerUIBundle.presets.apis],
			defaultModelsExpandDepth: 0,
			displayRequestDuration: true,
			tryItOutEnabled: true,
		});
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", boot);
	} else {
		boot();
	}
})();
