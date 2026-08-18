package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/devices"
)

// writeRoutes is every endpoint that can change the archive. The table is the
// test: a route added to the write path without authentication has to be added
// here to be exercised, and a route left off this list is the failure mode this
// guards against.
var writeRoutes = []struct {
	method, path string
}{
	{http.MethodPost, "/v1/sync/check"},
	{http.MethodPost, "/v1/assets"},
	{http.MethodPost, "/v1/uploads"},
	{http.MethodGet, "/v1/uploads/0123456789abcdef0123456789abcdef"},
	{http.MethodPut, "/v1/uploads/0123456789abcdef0123456789abcdef"},
	{http.MethodPost, "/v1/uploads/0123456789abcdef0123456789abcdef/commit"},
	{http.MethodDelete, "/v1/uploads/0123456789abcdef0123456789abcdef"},
}

func TestWritePathRefusesWithoutAToken(t *testing.T) {
	h := newHarness(t)

	for _, route := range writeRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			resp := h.raw(t, route.method, route.path, "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
			}
		})
	}
}

func TestWritePathRefusesAnUnknownToken(t *testing.T) {
	h := newHarness(t)

	for _, route := range writeRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			resp := h.raw(t, route.method, route.path, devices.TokenPrefix+"nope")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// readRoutes is every endpoint the gallery reads. Like writeRoutes, the table is
// the test: a read route added without a guard has to be listed here to be
// exercised, and one left off is the failure this catches.
//
// The asset id is a syntactically valid uuid that names nothing, because the
// point is which answer comes back — 401 before the lookup, not 404 after it.
// Answering 404 to an anonymous caller would confirm the id is unknown, which is
// already more than an unpaired client should learn.
var readRoutes = []struct {
	method, path string
}{
	{http.MethodGet, "/v1/timeline"},
	{http.MethodPost, "/v1/timeline/states"},
	{http.MethodGet, "/v1/assets/1f0d3a94-0000-4000-8000-000000000000"},
	{http.MethodGet, "/v1/assets/1f0d3a94-0000-4000-8000-000000000000/original"},
	{http.MethodGet, "/v1/assets/1f0d3a94-0000-4000-8000-000000000000/thumb"},
	{http.MethodGet, "/v1/assets/1f0d3a94-0000-4000-8000-000000000000/preview"},
	{http.MethodGet, "/v1/assets/1f0d3a94-0000-4000-8000-000000000000/playback"},
	{http.MethodGet, "/v1/jobs"},
}

// Phase 6 closed the read path on the listener the phone dials. The archive is
// no longer readable by anything on the LAN that has not been paired.
func TestReadPathRefusesWithoutAToken(t *testing.T) {
	h := newHarness(t)

	for _, route := range readRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			resp := h.raw(t, route.method, route.path, "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
			}
		})
	}
}

func TestReadPathRefusesARevokedToken(t *testing.T) {
	h := newHarness(t)

	if resp := h.get(t, "/v1/timeline"); resp.StatusCode != http.StatusOK {
		t.Fatalf("timeline before revoking = %d, want 200", resp.StatusCode)
	}
	if _, err := h.devices.Revoke(context.Background(), h.deviceID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	resp := h.get(t, "/v1/timeline")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("timeline after revoking = %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unpaired") {
		t.Errorf("body = %s, want it to say the device was unpaired", body)
	}
}

// A paired device reads the whole gallery. There is one archive and every device
// sees all of it — the token says "paired", not "paired and entitled to these
// rows".
func TestAPairedDeviceReadsTheGallery(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/health", "/v1/timeline", "/v1/jobs"} {
		resp := h.get(t, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

// /health is the exception, and it has to be: the app pings a remembered address
// to see whether it still answers, which happens before pairing and after a
// token has been revoked.
func TestHealthNeedsNoTokenOnEitherListener(t *testing.T) {
	h := newHarness(t)

	if resp := h.raw(t, http.MethodGet, "/health", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health on the TLS listener = %d, want 200", resp.StatusCode)
	}

	plain := h.plaintext(t)
	resp, err := plain.Client().Get(plain.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health on the plaintext listener = %d, want 200", resp.StatusCode)
	}
}

func TestUploadRefusesARevokedToken(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// The token works first, so the test is about revocation rather than about a
	// token that never worked.
	if resp := h.upload(t, loadFixture(t), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload before revoking = %d, want 201", resp.StatusCode)
	}
	if _, err := h.devices.Revoke(ctx, h.deviceID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	resp := h.upload(t, loadNamedFixture(t, "iphone-portrait.heic"), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status after revoking = %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "unpaired") {
		t.Errorf("body = %s, want it to say the device was unpaired", body)
	}
}

// Revoking withdraws access and nothing else. An archive that dropped photos
// when a phone was unpaired would not be an archive.
func TestRevokingKeepsWhatTheDeviceDelivered(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.upload(t, loadFixture(t), nil)
	before := h.blobFiles(t)

	if _, err := h.devices.Revoke(ctx, h.deviceID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if after := h.blobFiles(t); len(after) != len(before) || len(after) == 0 {
		t.Fatalf("blobs = %v, want the %d from before revoking", after, len(before))
	}

	// Read through the gallery's listener, because the revoked token can no
	// longer read through the other one — the archive still holds what the device
	// delivered, which is the property under test.
	plain := h.plaintext(t)
	resp, err := plain.Client().Get(plain.URL + "/v1/timeline")
	if err != nil {
		t.Fatalf("GET /v1/timeline: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("timeline after revoking = %d, want 200", resp.StatusCode)
	}
}

// A session id is derived from its declaration, so a second paired device that
// knew what the first was sending could otherwise resume, commit, or abort it.
func TestASessionIsInvisibleToAnotherDevice(t *testing.T) {
	h := newHarness(t)

	content := loadNamedFixture(t, "clip.mov")
	c := newChunked(h, content)
	session := c.begin(t)
	sent := len(content) / 2
	c.put(t, session.UploadID, 0, sent)

	_, otherToken := h.pair(t, "another phone")
	h.token = otherToken

	for _, route := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/uploads/" + session.UploadID},
		{http.MethodPut, "/v1/uploads/" + session.UploadID},
		{http.MethodPost, "/v1/uploads/" + session.UploadID + "/commit"},
		{http.MethodDelete, "/v1/uploads/" + session.UploadID},
	} {
		t.Run(route.method, func(t *testing.T) {
			resp := h.raw(t, route.method, route.path, otherToken)
			// 404 rather than 403: that the session exists at all is part of what
			// is being withheld.
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
		})
	}
}

// The partial has to survive the other device's attempts, or a hostile abort
// would cost the real uploader a 3GB re-send.
func TestASessionSurvivesAnotherDevicesAttempts(t *testing.T) {
	h := newHarness(t)

	content := loadNamedFixture(t, "clip.mov")
	c := newChunked(h, content)
	session := c.begin(t)
	sent := len(content) / 2
	c.put(t, session.UploadID, 0, sent)

	_, otherToken := h.pair(t, "another phone")
	original := h.token
	h.token = otherToken
	h.raw(t, http.MethodDelete, "/v1/uploads/"+session.UploadID, otherToken)

	h.token = original
	resumed := c.begin(t)
	if resumed.Offset != int64(sent) {
		t.Fatalf("offset = %d, want the %d bytes that were already sent", resumed.Offset, sent)
	}
}

func TestPairingIsSingleUse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	code, _, err := h.devices.CreateCode(ctx, time.Minute, "")
	if err != nil {
		t.Fatalf("create code: %v", err)
	}

	first := h.pairWith(t, code)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first redemption = %d, want 201", first.StatusCode)
	}
	second := h.pairWith(t, code)
	if second.StatusCode != http.StatusForbidden {
		t.Fatalf("second redemption = %d, want 403", second.StatusCode)
	}
}

func TestPairingRefusesAnExpiredCode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	code, _, err := h.devices.CreateCode(ctx, time.Minute, "")
	if err != nil {
		t.Fatalf("create code: %v", err)
	}
	// CreateCode coerces a non-positive TTL to the default, so an already-dead
	// code has to be aged rather than asked for.
	if _, err := h.store.Pool().Exec(ctx,
		`update pairing_codes set expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatalf("expire the code: %v", err)
	}

	if resp := h.pairWith(t, code); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestPairingAcceptsAHumanTypedCode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	code, _, err := h.devices.CreateCode(ctx, time.Minute, "")
	if err != nil {
		t.Fatalf("create code: %v", err)
	}

	// As it is printed, plus the mistakes a printed code invites.
	typed := strings.ToLower(devices.FormatCode(code))
	typed = " " + strings.ReplaceAll(typed, "0", "o") + " "

	resp := h.pairWith(t, typed)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201 for %q: %s", resp.StatusCode, typed, body)
	}
}

func TestPairingRefusesSomethingThatIsNotACode(t *testing.T) {
	h := newHarness(t)

	if resp := h.pairWith(t, "hello"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPairingRateLimitsAttempts(t *testing.T) {
	h := newHarness(t)

	// newHarness already paired once, so this is the remaining budget.
	var limited bool
	for i := 0; i < pairAttemptsPerIP+2; i++ {
		if h.pairWith(t, "AAAA-AAAA").StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("no attempt was rate limited in %d tries", pairAttemptsPerIP+2)
	}
}

// The plaintext listener is the gallery's, and the routing table is what keeps a
// token off it. This asserts the shape rather than a check inside a handler,
// because the shape is what survives PLAINTEXT_ADDR being widened to the LAN.
func TestPlaintextListenerServesNoCredentialPath(t *testing.T) {
	h := newHarness(t)

	plain := h.plaintext(t)

	for _, route := range append(writeRoutes, struct{ method, path string }{http.MethodPost, "/v1/pair"}) {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req, err := http.NewRequest(route.method, plain.URL+route.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			// Sending a valid token, because the point is that it is refused
			// anyway rather than that an anonymous request is.
			req.Header.Set("Authorization", "Bearer "+h.token)
			resp, err := plain.Client().Do(req)
			if err != nil {
				t.Fatalf("%s: %v", route.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUpgradeRequired {
				t.Fatalf("status = %d, want 426", resp.StatusCode)
			}
		})
	}

	// And the read path stays open here, which is the whole reason this listener
	// exists: the browser gallery proxies to it from the same machine rather than
	// teaching Node to trust a private CA. The other listener now wants a token
	// for these same two paths.
	for _, path := range []string{"/health", "/v1/timeline"} {
		resp, err := plain.Client().Get(plain.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 — the gallery reads this listener", path, resp.StatusCode)
		}
	}
}

// A server with no device store must serve no write path at all, rather than an
// unauthenticated one. Forgetting to wire it should take the archive offline, not
// open it.
func TestAMisconfiguredServerFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.srv.Devices = nil

	ts := httptest.NewServer(h.srv.Handler())
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/assets", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestTouchRecordsThatADeviceIsAlive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.upload(t, loadFixture(t), nil)

	list, err := h.devices.List(ctx)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	for _, d := range list {
		if d.ID == h.deviceID {
			if d.LastSeenAt == nil {
				t.Fatal("last_seen_at is still null after an authenticated upload")
			}
			return
		}
	}
	t.Fatalf("device %s is not in the list", h.deviceID)
}

// raw issues a request with an explicit token, for the rejection paths. The body
// is deliberately junk: none of these should get far enough to parse it.
func (h *harness) raw(t *testing.T, method, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return h.do(t, req)
}

func (h *harness) pairWith(t *testing.T, code string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"code": code, "name": "a phone", "platform": "test"})
	if err != nil {
		t.Fatalf("marshal pair request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/pair", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return h.do(t, req)
}
