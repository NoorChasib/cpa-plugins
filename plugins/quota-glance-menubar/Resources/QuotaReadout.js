// Runs only in the configured dashboard's main frame. Authentication stays
// inside WebKit: the native host receives labels/percentages, never headers.
(function installQuotaGlanceReadout(config) {
  if (location.origin !== config.origin || location.pathname !== config.path) return;
  const originalFetch = window.fetch.bind(window);
  const handler = window.webkit?.messageHandlers?.quotaGlanceReadout;
  if (!handler || window.__quotaGlanceReadout) return;
  let template = null;
  let snapshot = null;
  let active = 0;
  let pending = null;
  let observations = Promise.resolve();

  const send = (kind, value, reason) => handler.postMessage({ session: config.session, kind, snapshot: value ?? null, reason: reason ?? null });
  const unavailable = () => send("unavailable");
  const matches = (request) => {
    const url = new URL(request.url);
    return request.method === "GET" && url.origin === config.origin && config.summaryPaths.includes(url.pathname);
  };
  const throughConsole = (request) => new URL(request.url).pathname === config.summaryPaths[0];
  // The console door presents CPA's management key, and CPA counts every
  // refusal toward its ban. A console request that got no answer (a dropped
  // connection, a deadline, or a redirect, which may be a proxy hiding CPA's
  // refusal) may still have been counted, so, as the dashboard does
  // (access.ts readThrough), it is never repeated automatically: the console
  // request is forgotten, and the host told not to reload the page for it.
  // refresh() asks whether the server answers at all before it replays one.
  function unanswered(request) {
    if (!throughConsole(request)) return false;
    if (template && throughConsole(template)) {
      template = null;
      snapshot = null;
    }
    send("unavailable", null, "unanswered");
    return true;
  }
  // The dashboard page itself, which sits on CPA's resource tree: CPA checks
  // no key there and dispatches only GET, so a HEAD gets a plain 404 that no
  // ban counts. Null should the page ever sit under a Management API tree
  // (/v0/management, which Settings refuses, or CPA's /v8/management), where
  // a request without the key is itself a refusal: console replays then go
  // without asking first.
  const reachability = (() => {
    try {
      const url = new URL(config.origin + config.path);
      return /\/v\d+\/management\//.test(decodeURIComponent(url.pathname).toLowerCase() + "/") ? null : url.href;
    } catch {
      return null;
    }
  })();
  function project(doc) {
    if (doc?.schemaVersion !== 1 || typeof doc.stale !== "boolean" || !Array.isArray(doc.providers)) throw new Error("Unknown summary");
    const windows = [];
    for (const provider of [...doc.providers].sort((a, b) => a.order - b.order)) {
      if (typeof provider.id !== "string" || typeof provider.title !== "string" || !Array.isArray(provider.rows)) throw new Error("Invalid provider");
      for (const row of [...provider.rows].sort((a, b) => a.order - b.order)) {
        if (typeof row.rowId !== "string" || typeof row.title !== "string" || !row.aggregate) throw new Error("Invalid window");
        const value = row.aggregate.remainingPercent;
        // A Claude session whose every reporting account has spent its weekly
        // limit has no members, yet its 0% is a real reading. Older summaries
        // omit heldOutCount, so their memberless rows still read as missing.
        const reading = row.aggregate.memberCount > 0 || row.aggregate.heldOutCount > 0;
        windows.push({
          selection: { providerID: provider.id, rowID: row.rowId },
          title: `${provider.title} · ${row.title}`,
          remainingPercent: reading && Number.isInteger(value) && value >= 0 && value <= 100 ? value : null,
        });
      }
    }
    if (windows.length > 500) throw new Error("Too many windows");
    return { stale: doc.stale, windows };
  }
  async function observe(response, request) {
    if (response.redirected || new URL(response.url || request.url).origin !== config.origin) {
      if (unanswered(request)) return;
      throw new Error("Unexpected redirect");
    }
    if (response.status === 304 && snapshot && template?.url === request.url
        && template.headers.get("Authorization") === request.headers.get("Authorization")
        && template.credentials === request.credentials
        && template.headers.get("If-None-Match") === request.headers.get("If-None-Match")) {
      send("snapshot", snapshot);
      return;
    }
    if ([401, 403, 429].includes(response.status)) {
      template = null;
      snapshot = null;
    }
    if (!response.ok) throw new Error("Summary unavailable");
    const next = project(await response.json());
    // Copy only reusable request fields. The page's old AbortSignal has a
    // 20-second deadline and must never be carried into a later poll.
    const headers = new Headers(request.headers);
    const etag = response.headers.get("ETag");
    if (etag) headers.set("If-None-Match", etag);
    else headers.delete("If-None-Match");
    template = { url: request.url, headers, credentials: request.credentials };
    snapshot = next;
    send("snapshot", snapshot);
  }

  window.fetch = async function(input, init) {
    let request;
    try {
      // Constructing a Request from another Request can consume its body.
      // Leave dashboard actions untouched before inspecting a GET request.
      const method = init?.method ?? input?.method ?? "GET";
      if (String(method).toUpperCase() !== "GET") return originalFetch(input, init);
      // Relative URLs are valid in browsers, including in Request constructors.
      request = new Request(input, init);
      if (!matches(request)) return originalFetch(input, init);
    } catch {
      return originalFetch(input, init);
    }
    active += 1;
    try {
      const response = await originalFetch(input, init);
      // Never consume or delay the response the dashboard itself needs.
      const copy = response.clone();
      observations = observations.then(() => observe(copy, request)).catch(unavailable);
      return response;
    } catch (error) {
      if (!unanswered(request)) unavailable();
      throw error;
    } finally {
      active -= 1;
    }
  };

  async function refresh() {
    if (pending) return pending;
    if (active > 0) return;
    await observations;
    if (pending) return pending;
    if (!template) { unavailable(); return; }
    pending = (async () => {
      const used = template;
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), 20_000);
      try {
        const request = new Request(used.url, {
          method: "GET", headers: used.headers, credentials: used.credentials,
          cache: "no-store", redirect: "error", signal: controller.signal,
        });
        // The host asks for a reading as soon as the Mac wakes, usually
        // before Wi-Fi or a VPN is back. A console replay sent then would get
        // no answer and be dropped, so it first asks the dashboard page
        // whether the server answers at all. Any answer, whatever its status,
        // means the server is reachable, and the replay goes ahead. None
        // means the key was never sent: the request is kept for the host's
        // quick retries. The resource door is repeated anyway, so it goes
        // without asking and costs no extra request.
        if (throughConsole(request) && reachability) {
          try {
            await originalFetch(reachability, {
              method: "HEAD", cache: "no-store", credentials: "omit", redirect: "manual", signal: controller.signal,
            });
          } catch {
            send("unavailable", null, "network");
            return;
          }
          // The page reads through the console on its own too, as it does when
          // the network comes back, and may have while the check was out. Its
          // answer stands and has been reported: after a refusal or no answer
          // the key is not presented again, and a newer reading needs no
          // replay. Every read already answered is observed first.
          for (let seen; seen !== observations;) await (seen = observations);
          if (active > 0 || template !== used) return;
        }
        let response;
        try {
          response = await originalFetch(request);
        } catch {
          // No answer. The host polls the resource door again within seconds;
          // the console door is never repeated (see unanswered).
          if (!unanswered(request)) send("unavailable", null, "network");
          return;
        }
        await observe(response, request);
      } catch {
        unavailable();
      } finally {
        clearTimeout(timeout);
      }
    })();
    try { await pending; } finally { pending = null; }
  }

  // WebKit closes a modal page dialog on Escape but does not count the key as
  // handled, so it still reaches the app as cancelOperation: and closes the
  // popover too. The bridge closes the dialog itself, as the platform does (a
  // cancelable cancel event, then close), and keeps the key: one Escape closes
  // the dialog, the next the popover. On window, after the page's own
  // handlers, so a key the page handled is left alone.
  window.addEventListener("keydown", (event) => {
    if (!event.isTrusted || event.key !== "Escape" || event.defaultPrevented || event.isComposing) return;
    const dialog = event.target?.closest?.("dialog:modal") ?? [...document.querySelectorAll("dialog:modal")].pop();
    if (!dialog) return;
    event.preventDefault();
    if (dialog.dispatchEvent(new Event("cancel", { cancelable: true }))) dialog.close();
  });
  Object.defineProperty(window, "__quotaGlanceReadout", { value: Object.freeze({ refresh }), configurable: false });
})(__QUOTA_GLANCE_CONFIG__);
