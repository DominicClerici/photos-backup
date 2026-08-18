package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/websession"
)

// The browser's way in: a shared password, a cookie, and the gallery served
// from this listener so that the cookie is same-origin with the photographs.
//
// Everything the feature adds to this package is in this file. In api.go it
// needs exactly four lines, each marked "browser gate", and the whole of what
// undoing it means is: delete this file, put `s.requireToken` back in those two
// readRoutes/galleryRoutes calls, drop the sessionRoutes call and the
// withWebApp wrapper, and delete internal/websession. See that package's doc
// comment for the rest of the removal list.
//
// # Why the cookie is enough, and why the app shell is not behind it
//
// Only /v1 is guarded. The Next bundle, the HTML and the CSS are served to
// anyone on the network who asks, because they are not the archive: every byte
// of every photograph, and every row of every timeline, is behind /v1 and
// therefore behind the guard. Gating the shell too would buy nothing except a
// redirect to write and a login page to serve twice.
//
// The cookie is what makes this work at all. <img> and <video> cannot carry a
// bearer token, which is the obstacle that kept the gallery unauthenticated
// through Phase 6 — but a browser attaches a same-origin cookie to a subresource
// without being asked. Serving the app and the media from one origin is
// therefore not a convenience here; it is the mechanism. It is also why no
// signed media URLs were needed: there is no URL that is itself a secret.

// sessionCookie is the browser's credential. The name is prefixed rather than
// bare so it cannot be confused with anything the Next app sets.
const sessionCookie = "photobackup_session"

// maxSessionBody is generous for a password and nothing else.
const maxSessionBody = 8 << 10

// sessionStatus is what the sign-in form polls.
//
// Required is the interesting field. It is false on the plaintext listener —
// which needs no session, being on loopback — so that the development gallery
// at localhost:3000 never draws a password prompt it has no use for.
type sessionStatus struct {
	Required bool `json:"required"`
	SignedIn bool `json:"signed_in"`
	// ExpiresAt is when the session lapses if nothing touches it. Every guarded
	// request pushes it out, so this is a floor rather than a deadline.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// sessionRoutes are the sign-in endpoints, served only where the password can
// be sent safely.
//
// They are absent from the plaintext listener's routing table for the same
// reason pairing is — a credential must not be able to travel in the clear, and
// that is settled by which routes a listener has rather than by a check inside a
// handler. openSessionStatus below is the one exception, and it carries nothing.
func (s *Server) sessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/session", s.handleSessionStatus)
	mux.HandleFunc("POST /v1/session", s.handleSignIn)
	mux.HandleFunc("DELETE /v1/session", s.handleSignOut)
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	status := sessionStatus{Required: s.Sessions.Configured()}
	if expires, ok := s.Sessions.Valid(sessionToken(r)); ok {
		status.SignedIn = true
		status.ExpiresAt = &expires
	}
	writeJSON(w, http.StatusOK, status)
}

// openSessionStatus answers the plaintext listener's GET /v1/session.
//
// "No session is required here, and you may consider yourself signed in" is the
// truth on a listener that is open to anyone who can reach it, and it keeps the
// gallery's sign-in state a single question with a single answer rather than
// something the client has to know the deployment to interpret.
func openSessionStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, sessionStatus{Required: false, SignedIn: true})
}

func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSessionBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}

	token, expires, err := s.Sessions.Create(req.Password, clientIP(r))
	switch {
	case errors.Is(err, websession.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable,
			"this server has no gallery password; set GALLERY_PASSWORD and restart photod")
		return
	case errors.Is(err, websession.ErrTooManyAttempts):
		w.Header().Set("Retry-After", fmt.Sprint(int((5 * time.Minute).Seconds())))
		writeError(w, http.StatusTooManyRequests, "too many attempts; wait five minutes and try again")
		return
	case errors.Is(err, websession.ErrBadPassword):
		// Logged, because ten of these in a row is the only sign this server
		// will ever give that somebody on the network is trying.
		s.logger().Warn("rejected a gallery password", "remote", clientIP(r))
		writeError(w, http.StatusUnauthorized, "that is not the gallery password")
		return
	case err != nil:
		s.logger().Error("create a browser session", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not sign in")
		return
	}

	s.logger().Info("signed a browser in", "remote", clientIP(r))
	http.SetCookie(w, s.newSessionCookie(r, token))
	writeJSON(w, http.StatusCreated, sessionStatus{Required: true, SignedIn: true, ExpiresAt: &expires})
}

func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	s.Sessions.End(sessionToken(r))
	// Cleared with the same attributes it was set with, because a cookie is
	// only overwritten by one whose name, path and domain all match.
	cookie := s.newSessionCookie(r, "")
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) newSessionCookie(r *http.Request, token string) *http.Cookie {
	return &http.Cookie{
		Name:  sessionCookie,
		Value: token,
		Path:  "/",
		// HttpOnly: no script needs to read this, and the gallery renders
		// filenames and captions that came off a phone.
		HttpOnly: true,
		// Secure follows the connection rather than being hardcoded, so that a
		// TLS_DISABLED development server can still sign in — a browser drops a
		// Secure cookie arriving over http, and the failure is silent.
		Secure: r.TLS != nil,
		// Lax, which is what does the CSRF work here: a cross-site form post or
		// a hostile <img> pointed at this server carries no cookie, so the
		// delete and purge endpoints cannot be driven from another page. Strict
		// would add nothing except a first click that appears signed out.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.Sessions.TTL().Seconds()),
	}
}

func sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// browserOrDevice guards a route that either a paired device or a signed-in
// browser may have.
//
// The order matters only for cost: a cookie is a map lookup and a device token
// is a query, so the browser's credential is tried first and a scrolling gallery
// never touches Postgres to prove itself. A request carrying neither gets the
// device path's answer, which is the useful one — "pair this device" is what a
// phone needs to hear, and a browser is going to render its own sign-in form
// from GET /v1/session regardless of what the 401 says.
//
// It is a strict widening of requireToken: with no gallery password configured,
// Sessions.Valid is false for every request and this is requireToken exactly.
func (s *Server) browserOrDevice(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.Sessions.Valid(sessionToken(r)); ok {
			next(w, r)
			return
		}
		s.requireToken(next)(w, r)
	}
}

// withWebApp puts the gallery and the API on one origin, by serving the Next app
// for everything that is not the API.
//
// Only when a web app is configured: with WebApp nil this returns the API
// unchanged, which is what a server that nothing browses should be.
//
// The /api prefix is an alias for the API's own routes, not a second copy of
// them. It exists because the Next app addresses photod as /api/v1/... — in
// development that prefix is rewritten away by next.config.ts on its way to the
// plaintext listener, and here it is stripped on arrival instead. The client is
// then identical in both, which is the point: one build of the gallery works
// whether it is being served by `next dev` or by this.
func (s *Server) withWebApp(api http.Handler) http.Handler {
	if s.WebApp == nil {
		return api
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", api)
	mux.Handle("/health", api)
	mux.Handle("/api/", http.StripPrefix("/api", api))
	mux.Handle("/", s.WebApp)
	return mux
}
