# Background menu-bar readout: Apple API notes

Verified against Apple's live documentation on 2026-09-20. These are design
findings, not a claim that background behavior has been tested on macOS.

## Persistent page and native polling

Keep the same `WKWebView` and loaded document when the popover closes. There
is no documented requirement to reload a web view when its containing popover
opens. JavaScript variables belong to the current page; navigation replaces
them. Keeping the view alive avoids deliberately replacing that state, but
does not guarantee the WebKit content process will never terminate.
[WKContentWorld](https://developer.apple.com/documentation/webkit/wkcontentworld)
documents the page lifetime of variables;
[webViewWebContentProcessDidTerminate(_:)](https://developer.apple.com/documentation/webkit/wknavigationdelegate/webviewwebcontentprocessdidterminate(_:))
documents the independent content process and its termination callback.

Use a native timer to request a data refresh, without navigating/reloading the
page. A 60-second interval is an application choice, not an Apple guarantee.
[Timer](https://developer.apple.com/documentation/foundation/timer) explicitly
is not a real-time mechanism, can fire late, and recommends tolerance of at
least 10% for repeating timers. A timer added to the main run loop's common
modes avoids depending only on its default mode. A
[DispatchSourceTimer](https://developer.apple.com/documentation/dispatch/dispatchsourcetimer)
is another native scheduling option; dispatch the WebKit call to the main
actor. Neither native timer nor page JavaScript timers can guarantee polling
while the Mac sleeps.

`callAsyncJavaScript(_:arguments:in:in:completionHandler:)` is available from
macOS 11, so it is compatible with the app's macOS 13 minimum. Its Swift
signature accepts a function-body string, an arguments dictionary defaulting
to `[:]`, an optional frame defaulting to `nil`, a required content world,
and an optional completion handler defaulting to `nil`. `nil` frame means
main frame. The function body can use `await`; WebKit waits for a returned
thenable to resolve or reject. Use a completion handler to detect errors and
release the native in-flight guard. Explicitly return a serializable value
such as `true` rather than an internal request/response object.
[Apple method documentation](https://developer.apple.com/documentation/webkit/wkwebview/callasyncjavascript(_:arguments:in:in:completionhandler:))

Apple's API documentation does **not** promise a precise background cadence
or that every hidden web view stays runnable indefinitely. Current upstream
[WebPageProxy.cpp](https://github.com/WebKit/WebKit/blob/main/Source/WebKit/UIProcess/WebPageProxy.cpp),
in `runJavaScriptInFrameInScriptWorld`, takes a foreground activity where
RunningBoard and the page client permit it, and retains that activity until
the asynchronous reply. This supports preferring a native call that awaits
the fetch over launching an unobserved page timer. It is implementation
evidence, not an OS-version-independent public contract. Handle errors,
content-process termination, wake, and stale data; verify closed-popover
updates on a real Mac for multiple polling intervals.

## App Nap and energy

Apple's [App Nap guide](https://developer.apple.com/library/archive/documentation/Performance/Conceptual/power_efficiency_guidelines_osx/AppNap.html)
states that App Nap can throttle an app's timers and I/O. Moving the timer
to native code alone does not exempt the app from App Nap.

If the selected visible menu-bar readout requires updates while the popover
is closed, the minimal relevant activity option is
[`userInitiatedAllowingIdleSystemSleep`](https://developer.apple.com/documentation/foundation/processinfo/activityoptions/userinitiatedallowingidlesystemsleep).
It describes user-requested work while allowing idle system sleep. Retain the
token returned by
[`beginActivity(options:reason:)`](https://developer.apple.com/documentation/foundation/processinfo/beginactivity(options:reason:))
and balance it with `endActivity`. Scope any sustained activity to an enabled
readout, and end it when disabled or no longer useful. Do not add latency
critical, display-sleep-disabling, or idle-system-sleep-disabling flags for
quota polling.

This is a product tradeoff: Apple's
[ProcessInfo guidance](https://developer.apple.com/documentation/foundation/processinfo)
warns that long-running activities can harm performance and energy use, and
user preferences may override requests. A per-fetch activity minimizes its
duration but does not prevent timer throttling between fetches. A persistent
activity while the user has selected a readout is stronger against App Nap,
at an energy cost. It is not a promise that WebKit's separate content process
will never be suspended or terminated. Leave the Mac free to sleep and fetch
fresh data after wake.

## Page bridge and trust boundary

Install a `WKUserScript` at `.atDocumentStart`, `forMainFrameOnly: true`, in
`.page`, before the first page load. Document-start injection runs after
creation of the document element and before other content loads. Page world
is necessary to wrap the page's own global `fetch`; an isolated content world
has separate JavaScript variable bindings and cannot replace the page's
`window.fetch` by assigning its own binding. All worlds share the DOM, which
is a separate matter.
[User-script initializer](https://developer.apple.com/documentation/webkit/wkuserscript/init(source:injectiontime:formainframeonly:in:)),
[injection time](https://developer.apple.com/documentation/webkit/wkuserscriptinjectiontime/atdocumentstart),
[page world](https://developer.apple.com/documentation/webkit/wkcontentworld/page),
[content-world isolation](https://developer.apple.com/documentation/webkit/wkcontentworld).

Capture only the dashboard's recognized same-origin summary request. Keep
request headers and credentials inside the page closure; a native refresh
call can invoke that closure and receive only the parsed provider/window
readout. This is an application design recommendation, not an Apple-defined
quota API. Reusing the page's fetch context avoids introducing a second
credential/session owner in `URLSession`. The wrapper must preserve the page's
original fetch behavior and reject unrelated request paths or origins.

Register the message handler in `.page` too. Apple says the handler is
available in **all frames** in its content world, even when the injected
script itself is main-frame-only. Check `message.webView`,
`message.frameInfo.isMainFrame`, and the frame's trusted security origin
(scheme, host, and normalized port) against the configured dashboard. Also
validate message shape and numeric ranges. Do not accept a message-supplied
origin string as proof of origin. Page world means trusted page scripts can
also invoke this handler; keep its capability limited to displaying quota
state.
[Handler registration](https://developer.apple.com/documentation/webkit/wkusercontentcontroller/add(_:contentworld:name:)),
[message frame](https://developer.apple.com/documentation/webkit/wkscriptmessage/frameinfo),
[frame security origin](https://developer.apple.com/documentation/webkit/wkframeinfo/securityorigin).

`isInspectable` is unrelated to background updating: it enables Safari Web
Inspector access and defaults to `false`. There is no reason to enable it
for this feature.
[Apple property documentation](https://developer.apple.com/documentation/webkit/wkwebview/isinspectable)
