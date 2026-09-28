package httpapi

import "net/http"

// Cross-origin access for the ingest endpoint, and only for it.
//
// Every other client in the matrix opens a socket and writes. A browser asks
// permission first: the page lives on the application's origin, the endpoint
// lives on this one, and without a policy the browser refuses to let the SDK
// read the response. The request itself still arrives — a POST with a
// text/plain body needs no preflight — so the failure is invisible from here:
// events are stored, the server logs 200s, and the SDK cannot tell whether
// anything worked.
//
// What that costs is not cosmetic. The protocol's backpressure lives in the
// response: X-Sentry-Rate-Limits is how a server tells an SDK to stop sending
// a category, and it is the entire reason a switched-off subsystem costs
// nothing on the wire rather than merely nothing on disk (ADR 005). A browser
// SDK that cannot read that header keeps sending a refused category forever.
// Nothing but a real browser could have found this.
const (
	// The headers a browser is allowed to read off the response. Everything
	// else is invisible to the SDK even when this endpoint sends it.
	corsExposedHeaders = "X-Sentry-Rate-Limits, Retry-After, X-Sentry-Error"
	// The headers an SDK is allowed to send. sentry-trace and baggage are the
	// distributed-tracing pair; the rest are how the several SDK transports
	// present the key and the body encoding.
	corsAllowedHeaders = "Content-Type, Content-Encoding, X-Sentry-Auth, " +
		"Authorization, X-Requested-With, sentry-trace, baggage"
	// A day. The preflight answer only changes when this server is upgraded,
	// and a short lifetime would put an extra round trip in front of error
	// reports from every page load.
	corsMaxAgeSeconds = "86400"
)

// allowCrossOriginIngest permits any origin to post events and read the answer.
//
// Any origin is correct here rather than lax, for the same reason the CSRF
// header is not required on this endpoint (see the router): there is no
// ambient credential to abuse. Authentication is a key the caller must present
// explicitly, and that key already ships inside public browser bundles, so an
// origin allowlist would protect nothing and would break the ordinary case of
// one installation receiving events from several applications.
//
// Credentials are deliberately not allowed. "Allow-Origin: *" and
// "Allow-Credentials: true" are incompatible by specification, and asking for
// credentials would mean this endpoint could be reached with somebody's
// session cookie attached — which is exactly the attack the panel's CSRF
// header exists to stop.
func allowCrossOriginIngest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Access-Control-Allow-Origin", "*")
		header.Set("Access-Control-Expose-Headers", corsExposedHeaders)
		next.ServeHTTP(w, r)
	})
}

// handleIngestPreflight answers the OPTIONS request a browser sends before a
// request it considers unsafe.
//
// Most SDK transports avoid provoking one: a fetch with a plain body is a
// simple request and goes straight out. But an SDK configured with a custom
// header, one sending an explicit content type, or one routing through a
// tunnel will preflight, and a 405 here would stop every event from that page
// without any of them reaching this server to be logged.
func handleIngestPreflight(w http.ResponseWriter, _ *http.Request) {
	header := w.Header()
	header.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	header.Set("Access-Control-Allow-Headers", corsAllowedHeaders)
	header.Set("Access-Control-Max-Age", corsMaxAgeSeconds)
	w.WriteHeader(http.StatusOK)
}
