package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/websession"
)

// These tests build the Server by hand rather than through the harness in
// helper_test.go: signing in touches no database, and the point of most of them
// is what happens on a server that has no store wired at all.

const galleryPassword = "a gallery password"

func gated() *Server {
	return &Server{Sessions: websession.New(galleryPassword, time.Hour)}
}

func statusOf(t *testing.T, h http.Handler, req *http.Request) (int, sessionStatus) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var status sessionStatus
	if rec.Code < 400 && rec.Code != http.StatusNoContent {
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, status
}

func signIn(t *testing.T, h http.Handler, password string) (*http.Cookie, int) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/session", strings.NewReader(`{"password":`+quote(password)+`}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie, rec.Code
		}
	}
	return nil, rec.Code
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestSessionStatusWithoutAPassword(t *testing.T) {
	// The state every existing deployment is in: no gallery password, so the
	// browser gate is not merely closed but absent.
	srv := &Server{Sessions: websession.New("", time.Hour)}
	code, status := statusOf(t, srv.Handler(), httptest.NewRequest("GET", "/v1/session", nil))
	if code != http.StatusOK {
		t.Fatalf("GET /v1/session = %d, want 200", code)
	}
	if status.Required || status.SignedIn {
		t.Fatalf("status = %+v, want neither required nor signed in", status)
	}

	if _, code := signIn(t, srv.Handler(), "anything"); code != http.StatusServiceUnavailable {
		t.Fatalf("POST /v1/session with no password configured = %d, want 503", code)
	}
}

func TestSignInAndOut(t *testing.T) {
	srv := gated()
	h := srv.Handler()

	code, status := statusOf(t, h, httptest.NewRequest("GET", "/v1/session", nil))
	if code != http.StatusOK || !status.Required || status.SignedIn {
		t.Fatalf("status before signing in = %d %+v, want 200 required and signed out", code, status)
	}

	if cookie, code := signIn(t, h, "not the password"); code != http.StatusUnauthorized || cookie != nil {
		t.Fatalf("wrong password = %d, cookie %v; want 401 and no cookie", code, cookie)
	}

	cookie, code := signIn(t, h, galleryPassword)
	if code != http.StatusCreated {
		t.Fatalf("POST /v1/session = %d, want 201", code)
	}
	if cookie == nil {
		t.Fatal("signing in set no session cookie")
	}
	// The flags are the security properties, so they are asserted rather than
	// assumed: HttpOnly keeps the cookie away from script, and SameSite=Lax is
	// what stops another page from driving the delete endpoints.
	if !cookie.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}
	if cookie.Value == "" {
		t.Error("the session cookie is empty")
	}

	req := httptest.NewRequest("GET", "/v1/session", nil)
	req.AddCookie(cookie)
	code, status = statusOf(t, h, req)
	if code != http.StatusOK || !status.SignedIn {
		t.Fatalf("status while signed in = %d %+v, want signed in", code, status)
	}
	if status.ExpiresAt == nil || !status.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires_at = %v, want a time in the future", status.ExpiresAt)
	}

	out := httptest.NewRequest("DELETE", "/v1/session", nil)
	out.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, out)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /v1/session = %d, want 204", rec.Code)
	}

	req = httptest.NewRequest("GET", "/v1/session", nil)
	req.AddCookie(cookie)
	if _, status = statusOf(t, h, req); status.SignedIn {
		t.Fatal("the cookie still worked after signing out")
	}
}

func TestGuardAcceptsACookie(t *testing.T) {
	srv := gated()
	guarded := srv.browserOrDevice(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cookie, _ := signIn(t, srv.Handler(), galleryPassword)
	req := httptest.NewRequest("GET", "/v1/timeline", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	guarded(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("a signed-in browser got %d, want the handler to run", rec.Code)
	}

	// No cookie, no token: the device path answers, which on a server with no
	// device store means 503 rather than an open door. Fails closed either way.
	rec = httptest.NewRecorder()
	guarded(rec, httptest.NewRequest("GET", "/v1/timeline", nil))
	if rec.Code == http.StatusNoContent {
		t.Fatal("an unauthenticated request reached a guarded handler")
	}

	// A cookie that was never issued is not a cookie.
	rec = httptest.NewRecorder()
	forged := httptest.NewRequest("GET", "/v1/timeline", nil)
	forged.AddCookie(&http.Cookie{Name: sessionCookie, Value: "made up"})
	guarded(rec, forged)
	if rec.Code == http.StatusNoContent {
		t.Fatal("a forged session cookie reached a guarded handler")
	}
}

func TestGuardIsRequireTokenWithoutAPassword(t *testing.T) {
	// The widening is strict: with no gallery password, no request can take the
	// browser branch, so the guard is the one that was there before.
	srv := &Server{Sessions: websession.New("", time.Hour)}
	guarded := srv.browserOrDevice(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest("GET", "/v1/timeline", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "anything at all"})
	rec := httptest.NewRecorder()
	guarded(rec, req)
	if rec.Code == http.StatusNoContent {
		t.Fatalf("a cookie was honoured on a server with no gallery password")
	}
}

func TestPlaintextListenerRefusesTheSignIn(t *testing.T) {
	srv := gated()
	h := srv.PlaintextHandler()

	// The password may not cross a listener that is in the clear — the same
	// rule pairing is held to, and enforced the same way: by absence from the
	// routing table.
	if _, code := signIn(t, h, galleryPassword); code != http.StatusUpgradeRequired {
		t.Fatalf("POST /v1/session in the clear = %d, want 426", code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/v1/session", nil))
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("DELETE /v1/session in the clear = %d, want 426", rec.Code)
	}

	// Status is served, and says the gallery on this listener needs no session.
	// Anything else would put a password prompt in front of the development
	// gallery, which has nothing to prompt for.
	code, status := statusOf(t, h, httptest.NewRequest("GET", "/v1/session", nil))
	if code != http.StatusOK {
		t.Fatalf("GET /v1/session in the clear = %d, want 200", code)
	}
	if status.Required || !status.SignedIn {
		t.Fatalf("status = %+v, want not required and signed in", status)
	}
}

func TestWebAppSharesTheOrigin(t *testing.T) {
	srv := gated()
	srv.WebApp = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", "gallery")
		io.WriteString(w, r.URL.Path)
	})
	h := srv.Handler()

	// The app answers for anything that is not the API.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/collections", nil))
	if rec.Header().Get("X-Served-By") != "gallery" {
		t.Fatalf("GET /collections was not served by the gallery: %d %q", rec.Code, rec.Body.String())
	}

	// /api/v1/... is the same routing table as /v1/..., which is what lets one
	// build of the gallery work both here and behind `next dev`.
	for _, path := range []string{"/v1/session", "/api/v1/session"} {
		code, status := statusOf(t, h, httptest.NewRequest("GET", path, nil))
		if code != http.StatusOK || !status.Required {
			t.Fatalf("GET %s = %d %+v, want the session status", path, code, status)
		}
	}

	// And the API is still the API: the app must not be able to shadow a route
	// that carries photographs.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/session", strings.NewReader(`{"password":"wrong"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/v1/session with a wrong password = %d, want 401", rec.Code)
	}
	if rec.Header().Get("X-Served-By") == "gallery" {
		t.Fatal("an API route was served by the gallery")
	}
}

func TestWebAppUnsetLeavesTheAPIAlone(t *testing.T) {
	srv := gated()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/collections", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /collections with no gallery configured = %d, want 404", rec.Code)
	}
}
