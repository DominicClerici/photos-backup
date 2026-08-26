package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	lib "github.com/go-webauthn/webauthn/webauthn"

	"github.com/dominicclerici/photos-backup/server/internal/webauth"
)

// maxAuthBody is generous for a code, a label, and an attestation object. A
// platform authenticator's response is a few kilobytes; a security key with a
// long certificate chain is larger, and 64KiB clears both.
const maxAuthBody = 64 << 10

// authRoutes are how a browser signs in, and the one part of this server that
// has to answer somebody holding no credential at all.
//
// Everything here is rate limited through one bucket — see Server.signIn — and
// everything here is deliberately vague about why it said no. The three ways to
// fail (unknown code, spent code, expired code) are one answer, for the reason
// devices.ErrBadCode gives: distinguishing them tells a caller something about
// credentials they do not hold.
func (s *Server) authRoutes(mux *http.ServeMux) {
	// Open. What the sign-in page reads to decide whether to offer a passkey
	// prompt or an enrollment field, and what the gallery reads to find out its
	// session has ended.
	mux.HandleFunc("GET /v1/auth/status", s.handleAuthStatus)

	mux.HandleFunc("POST /v1/auth/register/start", s.handleRegisterStart)
	mux.HandleFunc("POST /v1/auth/register/finish", s.handleRegisterFinish)
	mux.HandleFunc("POST /v1/auth/login/start", s.handleLoginStart)
	mux.HandleFunc("POST /v1/auth/login/finish", s.handleLoginFinish)
	mux.HandleFunc("POST /v1/auth/recover", s.handleRecover)

	// Signing out needs a session to sign out of, but is not guarded: a browser
	// holding a cookie the server has already forgotten should still be able to
	// throw it away rather than get a 401 for trying.
	mux.HandleFunc("POST /v1/auth/logout", s.handleLogout)

	// Managing credentials, which needs one.
	mux.HandleFunc("GET /v1/auth/passkeys", s.requireAuth(s.handlePasskeys))
	mux.HandleFunc("DELETE /v1/auth/passkeys/{id}", s.requireAuth(s.handleRevokePasskey))
	mux.HandleFunc("POST /v1/auth/recovery-codes", s.requireAuth(s.handleMintRecovery))
}

type authStatus struct {
	// Enrolled is false on a fresh archive, and is what puts the sign-in page
	// into its bootstrap state. A server in that state is closed, not open:
	// requireAuth refuses everything, and the only thing that can change it is
	// somebody running `photobackup passkey add` at the machine.
	Enrolled bool `json:"enrolled"`
	SignedIn bool `json:"signedIn"`
	// Method and Expires describe the live session, and are absent without one.
	Method  string     `json:"method,omitempty"`
	Expires *time.Time `json:"expires,omitempty"`
	// Recovery is how many recovery codes remain, and is reported only to a
	// signed-in caller: it is a fact about this archive's credentials, and
	// "there are no recovery codes left" is not something to tell a stranger.
	Recovery *int `json:"recoveryRemaining,omitempty"`
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.Web == nil {
		writeError(w, http.StatusServiceUnavailable, "browser sign-in is not configured on this server")
		return
	}

	enrolled, err := s.Web.HasPasskey(r.Context())
	if err != nil {
		s.logger().Error("read passkey state", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	out := authStatus{Enrolled: enrolled}

	if token := cookieValue(r, SessionCookie); token != "" {
		if sess, err := s.Web.Authenticate(r.Context(), token); err == nil {
			deadline := s.Web.Deadline(sess)
			out.SignedIn = true
			out.Method = sess.Method
			out.Expires = &deadline
			if n, err := s.Web.RecoveryRemaining(r.Context()); err == nil {
				out.Recovery = &n
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRegisterStart issues a registration challenge, once it is satisfied
// that whoever asked is allowed to add a credential.
//
// There are exactly two ways to be allowed. An enrollment code, which is minted
// by `photobackup passkey add` and therefore proves filesystem access to the
// database — the same authority `photobackup pair` rests on, and the only one
// available on an archive with no credentials yet. Or a live session, because
// adding the laptop after the phone should not need a trip to the terminal, and
// somebody holding a session can already do strictly more than register a key.
func (s *Server) handleRegisterStart(w http.ResponseWriter, r *http.Request) {
	if !s.webAuthnReady(w) {
		return
	}

	var req struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	if !decodeAuthBody(w, r, &req) {
		return
	}

	enrollCode, ok := s.registrationAuthority(w, r, req.Code)
	if !ok {
		return
	}

	identity, err := s.Web.Identity(r.Context())
	if err != nil {
		s.logger().Error("resolve web identity", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	creation, session, err := s.WebAuthn.BeginRegistration(identity,
		// A discoverable credential, so signing in needs no username: the
		// authenticator offers the key it holds for this archive and that is the
		// whole of the interaction.
		lib.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		// Touch ID or a PIN, every time, not merely "someone was present". This
		// is what makes one gesture two factors — the device, and the person
		// holding it — and it is the reason this design needs no separate
		// password.
		lib.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		}),
		// Already-registered keys are excluded, so touching the same sensor
		// twice adds nothing and says so rather than silently replacing it.
		lib.WithExclusions(lib.Credentials(identity.WebAuthnCredentials()).CredentialDescriptors()),
	)
	if err != nil {
		s.logger().Error("begin passkey registration", "error", err)
		writeError(w, http.StatusInternalServerError, "could not start registration")
		return
	}

	id, err := s.Ceremonies.Put(webauth.KindRegister, enrollCode, session)
	if err != nil {
		s.logger().Error("store registration ceremony", "error", err)
		writeError(w, http.StatusInternalServerError, "could not start registration")
		return
	}
	setCeremony(w, id)
	writeJSON(w, http.StatusOK, creation)
}

// registrationAuthority settles whether this request may add a credential, and
// returns the enrollment code to claim if it was authorised by one.
//
// An empty returned code means "authorised by a live session", which
// handleRegisterFinish reads as the signal to take the codeless path.
func (s *Server) registrationAuthority(w http.ResponseWriter, r *http.Request, raw string) (string, bool) {
	if token := cookieValue(r, SessionCookie); token != "" {
		if _, err := s.Web.Authenticate(r.Context(), token); err == nil {
			return "", true
		}
	}

	// Rate limited before the code is looked at, so a wrong guess costs the
	// guesser an attempt whether or not it was well-formed.
	if retryAfter, ok := s.signIn().allow(clientIP(r)); !ok {
		tooManyAttempts(w, retryAfter)
		return "", false
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		writeError(w, http.StatusUnauthorized,
			"registering a passkey needs an enrollment code; run `photobackup passkey add` on the archive machine")
		return "", false
	}

	switch err := s.Web.CheckEnrollment(r.Context(), raw); {
	case errors.Is(err, webauth.ErrMalformedCode):
		writeError(w, http.StatusBadRequest, "that is not an enrollment code: "+err.Error())
		return "", false
	case errors.Is(err, webauth.ErrBadCode):
		s.logger().Warn("rejected an enrollment code", "remote", clientIP(r))
		writeError(w, http.StatusForbidden,
			"that enrollment code is not valid; it may have expired or already been used")
		return "", false
	case err != nil:
		s.logger().Error("check enrollment code", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return "", false
	}
	return raw, true
}

func (s *Server) handleRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !s.webAuthnReady(w) {
		return
	}

	session, enrollCode, ok := s.Ceremonies.Take(cookieValue(r, CeremonyCookie), webauth.KindRegister)
	clearCeremony(w)
	if !ok {
		writeError(w, http.StatusBadRequest,
			"this registration has expired or was already completed; start again")
		return
	}

	identity, err := s.Web.Identity(r.Context())
	if err != nil {
		s.logger().Error("resolve web identity", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	cred, err := s.WebAuthn.FinishRegistration(identity, session, r)
	if err != nil {
		s.logger().Warn("passkey registration failed", "remote", clientIP(r), "error", err)
		writeError(w, http.StatusBadRequest, "that passkey could not be verified: "+protocolReason(err))
		return
	}

	label := labelFor(r)
	var passkey webauth.Passkey
	if enrollCode == "" {
		passkey, err = s.Web.AddPasskeyAuthorized(r.Context(), cred, label)
	} else {
		passkey, err = s.Web.AddPasskey(r.Context(), cred, label, enrollCode)
	}
	switch {
	case errors.Is(err, webauth.ErrBadCode):
		// The code expired or was claimed by another browser between the two
		// halves of the ceremony. Rare, and the honest answer is to start over.
		writeError(w, http.StatusForbidden,
			"that enrollment code was used or expired while you were registering; mint another")
		return
	case err != nil:
		s.logger().Error("store passkey", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not store that passkey")
		return
	}

	s.logger().Info("registered a passkey", "passkey_id", passkey.ID, "label", passkey.Label,
		"remote", clientIP(r), "via", authorityName(enrollCode))
	writeJSON(w, http.StatusCreated, map[string]string{"passkeyId": passkey.ID, "label": passkey.Label})
}

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if !s.webAuthnReady(w) {
		return
	}

	// Refused before a challenge is issued rather than after an assertion is
	// checked, so an archive with no credentials cannot be probed for one.
	enrolled, err := s.Web.HasPasskey(r.Context())
	if err != nil {
		s.logger().Error("read passkey state", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	if !enrolled {
		writeError(w, http.StatusConflict,
			"no passkey is registered on this archive; run `photobackup passkey add` on the archive machine")
		return
	}

	assertion, session, err := s.WebAuthn.BeginDiscoverableLogin(
		lib.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		s.logger().Error("begin passkey login", "error", err)
		writeError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}

	id, err := s.Ceremonies.Put(webauth.KindLogin, "", session)
	if err != nil {
		s.logger().Error("store login ceremony", "error", err)
		writeError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	setCeremony(w, id)
	writeJSON(w, http.StatusOK, assertion)
}

func (s *Server) handleLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.webAuthnReady(w) {
		return
	}

	// Rate limited on the half that can fail. A challenge costs nothing to
	// issue; an assertion is the thing worth counting.
	if retryAfter, ok := s.signIn().allow(clientIP(r)); !ok {
		tooManyAttempts(w, retryAfter)
		return
	}

	session, _, ok := s.Ceremonies.Take(cookieValue(r, CeremonyCookie), webauth.KindLogin)
	clearCeremony(w)
	if !ok {
		writeError(w, http.StatusBadRequest, "this sign-in has expired; try again")
		return
	}

	identity, err := s.Web.Identity(r.Context())
	if err != nil {
		s.logger().Error("resolve web identity", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	// The handler the library calls once it knows which credential signed. It
	// answers "whose is this" — and since the archive has exactly one identity,
	// the only thing it has to check is that the handle presented is that one.
	// A mismatch is a credential from another archive being offered to this one.
	byHandle := func(_, userHandle []byte) (lib.User, error) {
		if !bytes.Equal(identity.Handle, userHandle) {
			return nil, errors.New("that passkey belongs to a different archive")
		}
		return identity, nil
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	_, cred, err := s.WebAuthn.FinishPasskeyLogin(byHandle, session, r)
	if err != nil {
		s.logger().Warn("rejected a passkey assertion", "remote", clientIP(r), "error", err)
		writeError(w, http.StatusUnauthorized, "that passkey was not accepted")
		return
	}

	passkeyID, err := s.Web.PasskeyIDFor(r.Context(), cred.ID)
	if err != nil {
		// The credential verified against a key this archive no longer holds
		// live — revoked between the challenge and the assertion, or deleted.
		s.logger().Warn("assertion from a passkey that is not registered", "remote", clientIP(r))
		writeError(w, http.StatusUnauthorized, "that passkey was not accepted")
		return
	}

	// Written before the session is minted, so a counter that went backwards is
	// recorded even though it does not stop the sign-in. Apple's synced
	// passkeys report a permanent zero, so a clone warning here is a signal to
	// look at rather than grounds to refuse — refusing on it would lock out
	// every iCloud Keychain user of a hardware-key feature they never used.
	if err := s.Web.RecordLogin(r.Context(), cred); err != nil {
		if errors.Is(err, webauth.ErrCloned) {
			s.logger().Warn("authenticator signature counter went backwards; the credential may be cloned",
				"passkey_id", passkeyID, "remote", clientIP(r))
		} else {
			s.logger().Error("record passkey login", "error", err)
		}
	}

	s.openSession(w, r, passkeyID, webauth.MethodPasskey)
}

// handleRecover spends a recovery code and opens a session.
//
// The session it opens is a full one rather than a restricted "you may only
// register a key" mode, and that is deliberate: the thing somebody is here to
// do is register a replacement passkey, and a session that could do only that
// would still be a session that can be stolen — while being a second code path
// through every authorisation decision in this server. One kind of session,
// recorded as having arrived this way.
func (s *Server) handleRecover(w http.ResponseWriter, r *http.Request) {
	if s.Web == nil {
		writeError(w, http.StatusServiceUnavailable, "browser sign-in is not configured on this server")
		return
	}

	if retryAfter, ok := s.signIn().allow(clientIP(r)); !ok {
		tooManyAttempts(w, retryAfter)
		return
	}

	var req struct {
		Code string `json:"code"`
	}
	if !decodeAuthBody(w, r, &req) {
		return
	}

	switch err := s.Web.RedeemRecovery(r.Context(), req.Code, clientIP(r)); {
	case errors.Is(err, webauth.ErrBadCode):
		s.logger().Warn("rejected a recovery code", "remote", clientIP(r))
		writeError(w, http.StatusForbidden, "that recovery code is not valid")
		return
	case err != nil:
		s.logger().Error("redeem recovery code", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	remaining, _ := s.Web.RecoveryRemaining(r.Context())
	s.logger().Warn("signed in with a recovery code", "remote", clientIP(r), "remaining", remaining)
	s.openSession(w, r, "", webauth.MethodRecovery)
}

// openSession mints the cookie and answers with what the browser needs to know.
func (s *Server) openSession(w http.ResponseWriter, r *http.Request, passkeyID, method string) {
	token, sess, err := s.Web.Mint(r.Context(), passkeyID, method, clientIP(r), r.UserAgent())
	if err != nil {
		s.logger().Error("open session", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not open a session")
		return
	}
	setSession(w, token)
	s.logger().Info("browser signed in", "method", method, "remote", clientIP(r))

	// Read rather than assumed. Signing in does not imply a passkey exists: a
	// recovery code opens an archive whose every passkey has been revoked, and
	// that is precisely the case where the browser needs to be told to register
	// one.
	enrolled, err := s.Web.HasPasskey(r.Context())
	if err != nil {
		s.logger().Error("read passkey state", "error", err)
	}

	writeJSON(w, http.StatusOK, authStatus{
		Enrolled: enrolled,
		SignedIn: true,
		Method:   sess.Method,
		Expires:  ptr(s.Web.Deadline(sess)),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.Web == nil {
		clearSession(w)
		writeJSON(w, http.StatusOK, map[string]bool{"signedIn": false})
		return
	}
	if token := cookieValue(r, SessionCookie); token != "" {
		if err := s.Web.Revoke(r.Context(), token); err != nil {
			s.logger().Error("revoke session", "error", err)
		}
	}
	clearSession(w)
	writeJSON(w, http.StatusOK, map[string]bool{"signedIn": false})
}

func (s *Server) handlePasskeys(w http.ResponseWriter, r *http.Request) {
	if s.Web == nil {
		writeError(w, http.StatusServiceUnavailable, "browser sign-in is not configured on this server")
		return
	}
	list, err := s.Web.Passkeys(r.Context())
	if err != nil {
		s.logger().Error("list passkeys", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}

	type item struct {
		ID         string     `json:"id"`
		Label      string     `json:"label"`
		Transports string     `json:"transports,omitempty"`
		CreatedAt  time.Time  `json:"createdAt"`
		LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
		RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	}
	out := make([]item, 0, len(list))
	for _, p := range list {
		out = append(out, item{p.ID, p.Label, p.Transports, p.CreatedAt, p.LastUsedAt, p.RevokedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": out})
}

// handleRevokePasskey withdraws a credential, refusing to withdraw the last one.
//
// Revoking the only passkey would leave the archive reachable exclusively by
// recovery code, which is a state somebody can get into deliberately from the
// command line and should not be able to get into by accident from a menu.
func (s *Server) handleRevokePasskey(w http.ResponseWriter, r *http.Request) {
	if s.Web == nil {
		writeError(w, http.StatusServiceUnavailable, "browser sign-in is not configured on this server")
		return
	}

	list, err := s.Web.Passkeys(r.Context())
	if err != nil {
		s.logger().Error("list passkeys", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	live := 0
	for _, p := range list {
		if !p.Revoked() {
			live++
		}
	}
	if live <= 1 {
		writeError(w, http.StatusConflict,
			"this is the only passkey registered; add another before revoking it, or use `photobackup passkey revoke`")
		return
	}

	p, killed, err := s.Web.RevokePasskey(r.Context(), r.PathValue("id"))
	if errors.Is(err, webauth.ErrNoSuchPasskey) {
		writeError(w, http.StatusNotFound, "no such passkey")
		return
	}
	if err != nil {
		s.logger().Error("revoke passkey", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not revoke that passkey")
		return
	}

	s.logger().Info("revoked a passkey", "passkey_id", p.ID, "label", p.Label, "sessions_ended", killed)
	writeJSON(w, http.StatusOK, map[string]any{"passkeyId": p.ID, "sessionsEnded": killed})
}

// handleMintRecovery replaces the recovery codes and returns the new set. This
// is the only time they exist outside whoever is reading the screen.
func (s *Server) handleMintRecovery(w http.ResponseWriter, r *http.Request) {
	if s.Web == nil {
		writeError(w, http.StatusServiceUnavailable, "browser sign-in is not configured on this server")
		return
	}
	codes, err := s.Web.MintRecovery(r.Context(), webauth.RecoveryCodeCount)
	if err != nil {
		s.logger().Error("mint recovery codes", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not mint recovery codes")
		return
	}
	s.logger().Info("minted a new set of recovery codes", "count", len(codes), "remote", clientIP(r))
	writeJSON(w, http.StatusCreated, map[string]any{"codes": codes})
}

// webAuthnReady reports whether the ceremony machinery is wired up, answering
// 503 when it is not. Separate from the s.Web nil check because a server can
// have the store and no relying-party config — which is what happens when
// WEB_ORIGIN is unset — and the two are different things to say.
func (s *Server) webAuthnReady(w http.ResponseWriter) bool {
	if s.Web == nil || s.WebAuthn == nil || s.Ceremonies == nil {
		writeError(w, http.StatusServiceUnavailable,
			"browser sign-in is not configured on this server; set WEB_ORIGIN and restart photod")
		return false
	}
	return true
}

func decodeAuthBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAuthBody)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return false
	}
	return true
}

func tooManyAttempts(w http.ResponseWriter, retryAfter time.Duration) {
	w.Header().Set("Retry-After", fmt.Sprint(int(retryAfter.Seconds())+1))
	writeError(w, http.StatusTooManyRequests, "too many attempts; wait and try again")
}

// labelFor names a passkey after the browser that registered it, so
// `photobackup web` lists something more useful than three identical rows.
// Crude on purpose: it is a note to a human, never read by anything.
func labelFor(r *http.Request) string {
	ua := r.UserAgent()
	switch {
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "iPad"):
		return "iPad"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "Macintosh"):
		return "Mac"
	case strings.Contains(ua, "Windows"):
		return "Windows"
	case strings.Contains(ua, "Linux"):
		return "Linux"
	default:
		return "a browser"
	}
}

func authorityName(enrollCode string) string {
	if enrollCode == "" {
		return "an open session"
	}
	return "an enrollment code"
}

// protocolReason unwraps the library's error into something worth showing.
// Its Details field is a short phrase written for this purpose; DevInfo can
// carry the challenge and is deliberately not sent to the browser.
func protocolReason(err error) string {
	var perr *protocol.Error
	if errors.As(err, &perr) && perr.Details != "" {
		return perr.Details
	}
	return "the authenticator's response did not verify"
}

func ptr[T any](v T) *T { return &v }
