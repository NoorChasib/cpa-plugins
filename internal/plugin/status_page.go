package plugin

// The unauthenticated browser resource stays a static shell on the server
// side: renderStatusPage returns fixed bytes with no snapshot fields, no
// mutations, and no secrets, regardless of the request. The embedded script
// upgrades the view purely client-side: it recovers the same-origin
// management-console key from localStorage (see browserAuthScript) and
// fetches the authenticated management HTML view over the same origin, also
// picking up ambient reverse-proxy authentication when present. Browsers
// without management credentials keep seeing exactly this static shell.

const resourceBootstrapScript = `
(function () {
	"use strict";
	var note = document.getElementById("auto-baseline-session-note");
	function showNote(text) {
		if (note) {
			note.textContent = text;
		}
	}
	var auth = window.autoBaselineAuth;
	if (!auth || typeof window.fetch !== "function") {
		return;
	}
	showNote("Checking for a same-origin management session…");
	fetch(auth.managementPath("/status/html") + location.search, {
		credentials: "same-origin",
		headers: auth.authHeaders({})
	})
		.then(function (resp) {
			if (resp.status === 401 || resp.status === 403) {
				throw new Error("unauthenticated");
			}
			if (!resp.ok) {
				throw new Error("HTTP " + resp.status);
			}
			var type = resp.headers.get("Content-Type") || "";
			if (type.indexOf("text/html") !== 0) {
				throw new Error("unexpected content type");
			}
			return resp.text();
		})
		.then(function (html) {
			document.open();
			document.write(html);
			document.close();
		})
		.catch(function (err) {
			if (err && err.message === "unauthenticated") {
				showNote("No same-origin management session was found, so only this public shell is shown. Sign in to the management console served from this origin (with the management key remembered) and reload, or use the authenticated management API directly.");
			} else {
				showNote("Could not load the authenticated status view (" + (err && err.message ? err.message : "error") + "); showing the public shell.");
			}
		});
})();
`

var staticStatusPage = []byte(`<!doctype html><html><head><meta charset="utf-8"><title>Auto Baseline</title><style>body{font-family:system-ui,sans-serif;margin:2rem;max-width:48rem;color:#1f2933}p{line-height:1.5}.meta{color:#475467;font-size:.9rem}code{background:#f3f4f6;padding:.1rem .3rem;border-radius:.2rem}</style></head><body><h1>Auto Baseline</h1><p data-auto-baseline-status="ready">The plugin resource is available.</p><p>This public page is a read-only shell. Complete operational details are available only through the authenticated management API. When this page is opened from the same origin as a signed-in management console, it automatically loads the authenticated status view in place.</p><p class="meta">Use authenticated <code>GET /v0/management/plugins/auto-baseline/status</code> to view complete status as JSON, or the authenticated browser view at <code>GET /v0/management/plugins/auto-baseline/status/html</code>. Dry-run configuration recommended for initial validation.</p><p class="meta" id="auto-baseline-session-note"></p><script>` + browserAuthScript + resourceBootstrapScript + `</script></body></html>`)

func renderStatusPage() []byte { return staticStatusPage }

// renderStatusPageForTest is the seam Dispatch uses so tests can inject a
// panicking handler and prove the recover boundary. Production never
// reassigns it.
var renderStatusPageForTest = renderStatusPage
