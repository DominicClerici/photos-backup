package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/devices"
)

// maxPairBody is generous for a code and a device name.
const maxPairBody = 8 << 10

// deviceHandler is a handler that only ever runs for an authenticated device.
//
// The identity is a parameter rather than something fished out of the request
// context, so a handler on the write path cannot be written without receiving it
// and cannot read it from the wrong place. Every device id that reaches the
// database comes from here.
type deviceHandler func(http.ResponseWriter, *http.Request, devices.Device)

// requireDevice resolves the bearer token and refuses the request if it does not
// name a live device.
func (s *Server) requireDevice(next deviceHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Devices == nil {
			// Fails closed. A server wired up without a device store serves no
			// write path at all, rather than an unauthenticated one.
			s.logger().Error("write path reached with no device store configured")
			writeError(w, http.StatusServiceUnavailable, "device authentication is not configured on this server")
			return
		}

		token, ok := bearerToken(r)
		if !ok {
			unauthorized(w, "a device token is required; pair this device with `photobackup pair`")
			return
		}

		device, err := s.Devices.Authenticate(r.Context(), token)
		switch {
		case errors.Is(err, devices.ErrRevoked):
			unauthorized(w, "this device has been unpaired; pair it again")
			return
		case errors.Is(err, devices.ErrBadToken):
			// Logged because a token that nothing recognises is either a phone
			// left over from a wiped database or somebody guessing, and both are
			// worth being able to see afterwards.
			s.logger().Warn("rejected an unrecognised device token", "remote", clientIP(r))
			unauthorized(w, "this device token is not recognised; pair the device again")
			return
		case err != nil:
			s.logger().Error("authenticate device", "error", err)
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}

		s.Devices.Touch(r.Context(), device.ID)
		next(w, r, device)
	}
}

// handlePair exchanges a pairing code for a device token. It is served only on
// the TLS listener, so the token it returns cannot be handed out in the clear.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if s.Devices == nil {
		writeError(w, http.StatusServiceUnavailable, "device authentication is not configured on this server")
		return
	}

	var req struct {
		Code     string `json:"code"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPairBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}

	// Rate limited before the code is looked at, so a wrong guess costs the
	// guesser an attempt whether or not it was well-formed.
	if retryAfter, ok := s.pairing().allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(retryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many pairing attempts; wait and try again")
		return
	}

	device, token, err := s.Devices.Pair(r.Context(), req.Code, strings.TrimSpace(req.Name),
		strings.TrimSpace(req.Platform), clientIP(r))
	switch {
	case errors.Is(err, devices.ErrMalformedCode):
		writeError(w, http.StatusBadRequest, "that is not a pairing code: "+err.Error())
		return
	case errors.Is(err, devices.ErrBadCode):
		s.logger().Warn("rejected a pairing code", "remote", clientIP(r))
		writeError(w, http.StatusForbidden, "that pairing code is not valid; it may have expired or already been used")
		return
	case err != nil:
		s.logger().Error("pair device", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not pair this device")
		return
	}

	host, _ := os.Hostname()
	s.logger().Info("paired a device", "device_id", device.ID, "name", device.Name, "remote", clientIP(r))
	writeJSON(w, http.StatusCreated, map[string]string{
		"deviceId":   device.ID,
		"token":      token,
		"serverName": host,
	})
}

// refuseInsecure answers the write endpoints on the plaintext listener.
//
// They are not merely absent there: a 404 on POST /v1/assets would read as a
// server too old to accept uploads, which is a bad half hour with a phone in
// hand. This says what is actually wrong.
func refuseInsecure(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Upgrade", "TLS/1.2")
	writeError(w, http.StatusUpgradeRequired,
		"this endpoint is only served over HTTPS; a device token is never accepted on an unencrypted connection")
}

// ownedBy reports whether an upload session belongs to the device asking about
// it, answering 404 when it does not.
//
// A session id is derived from its declaration, so a second paired device that
// knew what the first was uploading could otherwise resume, abort, or commit it.
// 404 rather than 403 because confirming that a session exists is itself the
// thing being withheld.
func (s *Server) ownedBy(w http.ResponseWriter, declaredDevice string, device devices.Device) bool {
	if declaredDevice == device.ID {
		return true
	}
	s.logger().Warn("device asked about another device's upload session",
		"device_id", device.ID, "session_device_id", declaredDevice)
	writeError(w, http.StatusNotFound, "no such upload session")
	return false
}

// deviceIDFor settles which device id a request is acting as.
//
// The authenticated identity always wins. A client-supplied id is accepted only
// when it agrees, because a client that thinks it is someone else is either a
// stale install or an attempt to write into another device's mappings, and
// quietly overriding it would hide both.
func (s *Server) deviceIDFor(w http.ResponseWriter, claimed string, device devices.Device) (string, bool) {
	claimed = strings.TrimSpace(claimed)
	if claimed == "" || claimed == device.ID {
		return device.ID, true
	}
	writeError(w, http.StatusForbidden,
		"this token belongs to a different device than the request claims; pair this device again")
	return "", false
}

func bearerToken(r *http.Request) (string, bool) {
	raw := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(raw, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="photobackup"`)
	writeError(w, http.StatusUnauthorized, msg)
}

// clientIP is the peer address, for a log line and a rate-limit key. Nothing
// proxies photod, so no forwarding header is trusted — honouring one would let a
// caller pick its own rate-limit bucket.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Pairing attempt limits. A 40-bit code is not guessable in any number of
// requests a network will carry, so this is not what makes pairing safe — it is
// what keeps a stuck client from filling the log and what makes a real attempt
// visible in it.
const (
	pairAttemptsPerIP = 10
	pairAttemptsTotal = 40
	pairWindow        = 5 * time.Minute
	// pairMaxKeys bounds the map. Reached only by something spraying from many
	// addresses, and at that point the global limit is doing the work anyway.
	pairMaxKeys = 1024
)

func (s *Server) pairing() *attemptLimiter {
	s.pairOnce.Do(func() { s.pairLimiter = &attemptLimiter{} })
	return s.pairLimiter
}

type attemptLimiter struct {
	mu     sync.Mutex
	perIP  map[string][]time.Time
	global []time.Time
}

// allow records an attempt and reports whether it may proceed, with how long to
// wait if not.
func (l *attemptLimiter) allow(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-pairWindow)
	if l.perIP == nil {
		l.perIP = make(map[string][]time.Time)
	}
	if len(l.perIP) > pairMaxKeys {
		l.perIP = make(map[string][]time.Time)
	}

	l.global = recent(l.global, cutoff)
	mine := recent(l.perIP[ip], cutoff)

	if len(mine) >= pairAttemptsPerIP {
		return pairWindow - now.Sub(mine[0]), false
	}
	if len(l.global) >= pairAttemptsTotal {
		return pairWindow - now.Sub(l.global[0]), false
	}

	l.perIP[ip] = append(mine, now)
	l.global = append(l.global, now)
	return 0, true
}

func recent(times []time.Time, cutoff time.Time) []time.Time {
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}
