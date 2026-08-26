package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dominicclerici/photos-backup/server/internal/devices"
	"github.com/dominicclerici/photos-backup/server/internal/webauth"
)

// SessionCookie is what a signed-in browser carries.
//
// The `__Host-` prefix is not decoration. A browser refuses to store a cookie
// under it unless the cookie is Secure, has Path=/, and carries no Domain
// attribute — which together mean it cannot be set by a sibling subdomain and
// cannot be scoped wider than this exact origin. It turns three properties this
// code has to get right into three the browser enforces.
const SessionCookie = "__Host-photobackup_session"

// CeremonyCookie names the half-finished WebAuthn exchange. It lives for two
// minutes and is cleared the moment the ceremony completes or fails.
const CeremonyCookie = "__Host-photobackup_ceremony"

// setSession writes the session cookie.
//
// No Max-Age and no Expires, deliberately: that makes it a session cookie, which
// the browser drops when it closes. Signing in once per browser session is the
// stated requirement for the dashboard, and it is the cheapest possible way to
// get it — the server's own idle window and absolute cap are what actually
// enforce the lifetime, and this just means a closed laptop is signed out
// without waiting for either.
func setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		// Strict rather than Lax: nothing links into this archive from anywhere
		// else, so there is no navigation this breaks, and it means no
		// cross-site request carries the cookie at all — which is most of CSRF
		// dealt with before the Origin check below is reached.
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

func setCeremony(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CeremonyCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(webauth.CeremonyTTL.Seconds()),
	})
}

func clearCeremony(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CeremonyCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// requireAuth guards everything the archive will show or let somebody change.
//
// This is the replacement for openToAnyone, which is what the plaintext listener
// used to pass here — and the reason that listener is gone. Until now the whole
// gallery, including deleting and purging and unlocking the vault, was reachable
// by anyone who could open the port, and the only thing making that safe was
// that the port was bound to 127.0.0.1. It is now bound to an address the phone
// and the laptop can both reach, so "who is asking" had to become a question
// this server can answer.
//
// Fails closed. A server wired up without either credential store serves
// nothing rather than serving everything.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Devices == nil && s.Web == nil {
			s.logger().Error("guarded route reached with no credential store configured")
			writeError(w, http.StatusServiceUnavailable, "authentication is not configured on this server")
			return
		}

		// A bearer token wins when both are present. It is the narrower
		// credential — it names one device — and a browser never sends one, so
		// the only way to arrive with both is a client that meant the token.
		if token, ok := bearerToken(r); ok {
			if s.Devices == nil {
				unauthorized(w, "this server does not accept device tokens")
				return
			}
			device, err := s.Devices.Authenticate(r.Context(), token)
			switch {
			case errors.Is(err, devices.ErrRevoked):
				unauthorized(w, "this device has been unpaired; pair it again")
				return
			case errors.Is(err, devices.ErrBadToken):
				s.logger().Warn("rejected an unrecognised device token", "remote", clientIP(r))
				unauthorized(w, "this device token is not recognised; pair the device again")
				return
			case err != nil:
				s.logger().Error("authenticate device", "error", err)
				writeError(w, http.StatusServiceUnavailable, "database unavailable")
				return
			}
			s.Devices.Touch(r.Context(), device.ID)
			next(w, r)
			return
		}

		if s.Web == nil {
			unauthorized(w, "this server does not accept browser sessions")
			return
		}
		token := cookieValue(r, SessionCookie)
		if token == "" {
			unauthorized(w, "sign in to open this archive, or pair this device")
			return
		}

		sess, err := s.Web.Authenticate(r.Context(), token)
		switch {
		case errors.Is(err, webauth.ErrBadSession):
			// Cleared rather than left in place, so a browser holding a session
			// that has idled out stops sending it and the sign-in page does not
			// have to reason about a cookie that means nothing.
			clearSession(w)
			unauthorized(w, "this session has ended; sign in again")
			return
		case err != nil:
			s.logger().Error("authenticate session", "error", err)
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}

		// CSRF. SameSite=Strict already means no cross-site context sends this
		// cookie, so this is the second of two independent defences rather than
		// the only one — which is the right number for a check that depends on
		// the browser having told the truth about where a request came from.
		//
		// Only the unsafe methods, and only for a cookie: a bearer token is not
		// attached by the browser to anything, so nothing it authenticates can
		// be forged this way.
		if !safeMethod(r.Method) && !sameOrigin(r) {
			s.logger().Warn("rejected a cross-origin write", "remote", clientIP(r),
				"origin", r.Header.Get("Origin"), "path", r.URL.Path)
			writeError(w, http.StatusForbidden, "this request did not come from the archive's own pages")
			return
		}

		s.Web.Touch(r.Context(), sess)
		next(w, r)
	}
}

// safeMethod reports whether a method only reads. These are exempt from the
// origin check because a cross-site GET carries no cookie under SameSite=Strict
// anyway, and because <img> and <video> are GETs that legitimately arrive
// without an Origin header.
func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// sameOrigin decides whether a state-changing request came from this archive's
// own pages.
//
// Sec-Fetch-Site is the primary signal and is sent by every browser that has
// shipped in years; it is computed by the browser and cannot be set by script.
// Origin is the fallback for anything that does not send it. A request with
// neither is refused rather than allowed: the clients that reach this path are
// the archive's own pages and the phone, and the phone authenticates with a
// bearer token and never gets here.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
		// Fall through to Origin.
	default:
		return false
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	host := origin
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	return strings.EqualFold(host, r.Host)
}
