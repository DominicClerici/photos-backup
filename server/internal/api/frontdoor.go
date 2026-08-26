package api

import (
	_ "embed"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// SignInPath is where an unauthenticated browser is sent, and the only page
// this server draws itself.
const SignInPath = "/signin"

//go:embed signin.html
var signInPage string

// FrontDoor is the whole archive on one origin: the API, the media, and the
// gallery, behind one guard.
//
// One origin is not a convenience. Phase 12 established the constraint the hard
// way and PROJECT.md records it: a browser attaches a same-origin cookie to an
// <img>, and will not attach a bearer header to one. Any arrangement that puts
// the thumbnails on a different origin from the session has to solve that
// again, with signed URLs or a second credential or a cookie scoped wide enough
// to cover both. Serving everything from here means there is nothing to solve.
//
// The Next app sits behind this rather than in front of it, which is what lets
// the guard be one function in Go rather than one in Go and one in TypeScript —
// and what means an unauthenticated visitor is handed a sign-in page instead of
// the application bundle.
//
// app may be nil, in which case the gallery is unavailable and the API is not.
// That is the right degradation for a photod whose Next process is being
// restarted: the phone keeps backing up.
func (s *Server) FrontDoor(app *url.URL) http.Handler {
	api := s.Handler()
	mux := http.NewServeMux()

	// The API, under both prefixes it is reached by.
	//
	// /v1 is what the phone has always used. /api/v1 is what the browser uses,
	// because the gallery's client was written against a Next rewrite that put
	// the API under /api — see web/src/lib/api.ts. Serving both costs one
	// StripPrefix and means neither client had to be rewritten to land on one
	// origin.
	mux.Handle("/v1/", api)
	mux.Handle("/health", api)
	mux.Handle("/api/", http.StripPrefix("/api", api))

	mux.HandleFunc("GET "+SignInPath, s.handleSignInPage)

	if app != nil {
		mux.Handle("/", s.requirePage(newAppProxy(app)))
	} else {
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusServiceUnavailable,
				"the gallery is not running; photod is serving the API only")
		}))
	}

	return securityHeaders(mux)
}

// newAppProxy forwards to the Next process.
//
// Everything about the request is passed through, including the Upgrade
// handshake Next's dev server uses for hot reload — Go's ReverseProxy has
// handled that since 1.20, so `next dev` behind this behaves like `next dev` in
// front of nothing.
func newAppProxy(app *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(app)

	// The app is a local process reached over loopback, so the interesting
	// failure is "it is not running yet", which is common during a deploy and
	// is not worth a stack trace in the log.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, http.StatusBadGateway, "the gallery is not answering; it may still be starting")
	}
	return proxy
}

// requirePage guards the gallery itself.
//
// It is requireAuth's sibling and differs in exactly one way: what it does to
// somebody who is not signed in. A fetch wants 401 and JSON, so that the
// gallery's client can notice and send the browser to the sign-in page. A
// top-level navigation wants a redirect, because a person who typed the address
// should get a page rather than a line of JSON.
//
// Only a session is accepted, not a device token. A page is for a browser; the
// phone reads /v1 directly and never asks for one.
func (s *Server) requirePage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Web == nil {
			writeError(w, http.StatusServiceUnavailable, "browser sign-in is not configured on this server")
			return
		}

		token := cookieValue(r, SessionCookie)
		if token == "" {
			redirectToSignIn(w, r)
			return
		}
		sess, err := s.Web.Authenticate(r.Context(), token)
		if err != nil {
			clearSession(w)
			redirectToSignIn(w, r)
			return
		}
		s.Web.Touch(r.Context(), sess)
		next.ServeHTTP(w, r)
	})
}

// redirectToSignIn sends a browser to the sign-in page, remembering where it was
// going.
//
// The destination is carried as a path and validated as one on the way back in
// — see safeNext. An open redirect here would be a way to borrow this archive's
// name for somebody else's page.
func redirectToSignIn(w http.ResponseWriter, r *http.Request) {
	to := SignInPath
	if dest := r.URL.RequestURI(); dest != "" && dest != "/" {
		to += "?next=" + url.QueryEscape(dest)
	}
	// 303 rather than 302: whatever the method was, what follows is a GET of a
	// different resource.
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// handleSignInPage draws the only page this server owns.
//
// It is served by Go rather than by Next on purpose. It means the gallery's
// bundle is behind the guard — an unauthenticated visitor receives no
// application code at all, only this — and it means signing in still works when
// the Next process is down, being deployed, or has never been started.
func (s *Server) handleSignInPage(w http.ResponseWriter, r *http.Request) {
	nonce, err := scriptNonce()
	if err != nil {
		s.logger().Error("generate script nonce", "error", err)
		writeError(w, http.StatusInternalServerError, "could not render the sign-in page")
		return
	}

	// A strict CSP, which this page can have because it is one file with one
	// script and no external anything. The app's is necessarily looser; see
	// securityHeaders.
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"script-src 'nonce-" + nonce + "'",
		"style-src 'nonce-" + nonce + "'",
		"connect-src 'self'",
		"img-src 'self' data:",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; "))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	page := strings.ReplaceAll(signInPage, "{{nonce}}", nonce)
	page = strings.ReplaceAll(page, "{{next}}", safeNext(r.URL.Query().Get("next")))
	_, _ = w.Write([]byte(page))
}

// safeNext reduces a remembered destination to something that can only point
// back into this archive.
//
// It must start with a single slash and must not start with two, because "//"
// is a protocol-relative URL and would send the browser to another host. Nothing
// else is trusted from it: the value is interpolated into a JS string literal,
// so it is also stripped of anything that could close one.
func safeNext(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	if strings.ContainsAny(raw, "\"'\\<>\n\r") {
		return "/"
	}
	return raw
}

func scriptNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}

// securityHeaders is what every response out of this server carries.
//
// These are cheap and they are the difference between a bug being a bug and a
// bug being a way in. The archive is reachable only over the tailnet, which
// makes the network hostile-by-default assumption weaker than it would be on
// the public internet — and none of that is a reason to let a page frame this
// one or let a browser guess at a content type.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// The archive is HTTPS-only and has been since Phase 5. Two years, and
		// preload is deliberately absent: this is a private hostname, and
		// submitting it to a browser-vendor list would publish it.
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// Nothing here needs a camera, a microphone, or a location — least of
		// all a gallery of photographs that already know where they were taken.
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")

		// The sign-in page sets its own, stricter than this one can be: Next
		// ships inline bootstrap scripts and inline styles, so 'unsafe-inline'
		// is the price of not running a nonce through the framework's own
		// rendering. What this still buys is real — no external script, no
		// external connection, and no frame.
		if h.Get("Content-Security-Policy") == "" {
			h.Set("Content-Security-Policy", strings.Join([]string{
				"default-src 'self'",
				"script-src 'self' 'unsafe-inline'",
				"style-src 'self' 'unsafe-inline'",
				// blob: for the video the viewer assembles, data: for the
				// placeholder tiles the grid draws before a thumbnail arrives.
				"img-src 'self' data: blob:",
				"media-src 'self' blob:",
				// next/font self-hosts at build time, so no font CDN is needed.
				"font-src 'self' data:",
				"connect-src 'self'",
				"base-uri 'none'",
				"form-action 'self'",
				"frame-ancestors 'none'",
			}, "; "))
		}

		next.ServeHTTP(w, r)
	})
}
