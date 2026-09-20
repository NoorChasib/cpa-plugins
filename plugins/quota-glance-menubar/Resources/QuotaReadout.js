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

  const send = (kind, value) => handler.postMessage({ session: config.session, kind, snapshot: value ?? null });
  const unavailable = () => send("unavailable");
  const matches = (request) => {
    const url = new URL(request.url);
    return request.method === "GET" && url.origin === config.origin && config.summaryPaths.includes(url.pathname);
  };
  function project(doc) {
    if (doc?.schemaVersion !== 1 || typeof doc.stale !== "boolean" || !Array.isArray(doc.providers)) throw new Error("Unknown summary");
    const windows = [];
    for (const provider of [...doc.providers].sort((a, b) => a.order - b.order)) {
      if (typeof provider.id !== "string" || typeof provider.title !== "string" || !Array.isArray(provider.rows)) throw new Error("Invalid provider");
      for (const row of [...provider.rows].sort((a, b) => a.order - b.order)) {
        if (typeof row.rowId !== "string" || typeof row.title !== "string" || !row.aggregate) throw new Error("Invalid window");
        const value = row.aggregate.remainingPercent;
        windows.push({
          selection: { providerID: provider.id, rowID: row.rowId },
          title: `${provider.title} · ${row.title}`,
          remainingPercent: row.aggregate.memberCount > 0 && Number.isInteger(value) && value >= 0 && value <= 100 ? value : null,
        });
      }
    }
    if (windows.length > 500) throw new Error("Too many windows");
    return { stale: doc.stale, windows };
  }
  async function observe(response, request) {
    if (response.redirected || new URL(response.url || request.url).origin !== config.origin) throw new Error("Unexpected redirect");
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
      unavailable();
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
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), 20_000);
      try {
        const request = new Request(template.url, {
          method: "GET", headers: template.headers, credentials: template.credentials,
          cache: "no-store", redirect: "error", signal: controller.signal,
        });
        await observe(await originalFetch(request), request);
      } catch {
        unavailable();
      } finally {
        clearTimeout(timeout);
      }
    })();
    try { await pending; } finally { pending = null; }
  }
  Object.defineProperty(window, "__quotaGlanceReadout", { value: Object.freeze({ refresh }), configurable: false });
})(__QUOTA_GLANCE_CONFIG__);
