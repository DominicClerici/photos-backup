package api

import (
	"context"
	"encoding/json"
	"fmt"
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
func TestHealthNeedsNoCredential(t *testing.T) {
	h := newHarness(t)

	if resp := h.raw(t, http.MethodGet, "/health", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", resp.StatusCode)
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

	// Read back as the browser, because the revoked token can no longer read
	// anything — the archive still holds what the device delivered, which is the
	// property under test, and a second credential is how it is observed.
	gallery := h.gallery(t)
	resp, err := gallery.Client().Get(gallery.URL + "/v1/timeline")
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

// galleryRoutes is a sample of what the browser reaches: two reads and two
// writes. Not the whole table — that is what the guard is for — but enough that
// "the gallery is authenticated" is asserted rather than assumed.
var galleryRoutes = []struct {
	method, path string
}{
	{http.MethodGet, "/v1/timeline"},
	{http.MethodGet, "/v1/collections"},
	{http.MethodPost, "/v1/trash"},
	{http.MethodPost, "/v1/vault/unlock"},
}

// Until this phase the whole of this table was served, unauthenticated, to
// anyone who could open PLAINTEXT_ADDR — including the deletes and the vault.
// The listener that did it is gone, and this is the property that replaced it.
func TestGalleryRoutesRefuseAnAnonymousCaller(t *testing.T) {
	h := newHarness(t)

	for _, route := range galleryRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			resp := h.raw(t, route.method, route.path, "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 — this route is reachable without a credential", resp.StatusCode)
			}
		})
	}
}

// The other half: a session cookie is a credential these routes accept, so the
// refusal above is a guard rather than a route that stopped working.
func TestGalleryRoutesAcceptASessionCookie(t *testing.T) {
	h := newHarness(t)
	gallery := h.gallery(t)

	for _, path := range []string{"/v1/timeline", "/v1/collections"} {
		resp, err := gallery.Client().Get(gallery.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

// A device token reaches the same routes, because the phone reads the archive
// through them too. One guard, two credentials.
func TestGalleryRoutesAcceptADeviceToken(t *testing.T) {
	h := newHarness(t)

	if resp := h.raw(t, http.MethodGet, "/v1/timeline", h.token); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/timeline with a device token = %d, want 200", resp.StatusCode)
	}
}

// CSRF. SameSite=Strict is the first defence and this is the second: a write
// that the browser says came from somewhere else is refused even though it
// carried a live cookie.
func TestGalleryWriteNeedsSameOrigin(t *testing.T) {
	h := newHarness(t)
	gallery := h.gallery(t)

	req, err := http.NewRequest(http.MethodPost, gallery.URL+"/v1/trash", strings.NewReader(`{"ids":[]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	resp, err := gallery.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v1/trash: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a cross-site write was accepted", resp.StatusCode)
	}
}

// A read is exempt, and has to be: <img> and <video> issue GETs that carry no
// Origin, and every thumbnail in the grid is one of them.
func TestGalleryReadIsExemptFromTheOriginCheck(t *testing.T) {
	h := newHarness(t)
	gallery := h.gallery(t)

	req, err := http.NewRequest(http.MethodGet, gallery.URL+"/v1/timeline", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	resp, err := gallery.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/timeline: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// Signing out is a real revocation rather than a discarded cookie: the row is
// dead on the server, so the cookie the browser still holds stops working.
func TestAnEndedSessionStopsWorking(t *testing.T) {
	h := newHarness(t)
	gallery := h.gallery(t)

	if err := h.web.Revoke(context.Background(), h.session); err != nil {
		t.Fatalf("revoke session: %v", err)
	}

	resp, err := gallery.Client().Get(gallery.URL + "/v1/timeline")
	if err != nil {
		t.Fatalf("GET /v1/timeline: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 after signing out", resp.StatusCode)
	}
}

// Registering a passkey needs authority: an enrollment code, or a session. An
// anonymous caller with neither cannot start the ceremony, which is what stops
// a fresh archive from being claimed by whoever reaches it first.
func TestRegisteringAPasskeyNeedsAuthority(t *testing.T) {
	h := newHarness(t)

	resp := h.postAnon(t, "/v1/auth/register/start", `{"label":"laptop"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without an enrollment code", resp.StatusCode)
	}

	resp = h.postAnon(t, "/v1/auth/register/start", `{"code":"AAAA-AAAA"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a code this archive does not hold", resp.StatusCode)
	}
}

// A recovery code opens a session, once. The second attempt with the same code
// is refused, which is the whole of what "single use" means here.
func TestARecoveryCodeIsSpentOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	codes, err := h.web.MintRecovery(ctx, 3)
	if err != nil {
		t.Fatalf("mint recovery codes: %v", err)
	}

	body := fmt.Sprintf(`{"code":%q}`, codes[0])
	resp := h.postAnon(t, "/v1/auth/recover", body)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("first redemption = %d (%s), want 200", resp.StatusCode, raw)
	}
	if !hasCookie(resp, SessionCookie) {
		t.Error("no session cookie was set by a successful recovery")
	}

	if resp := h.postAnon(t, "/v1/auth/recover", body); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("second redemption = %d, want 403 — the code was reusable", resp.StatusCode)
	}
}

// The status endpoint is the one thing a stranger may read, and it says only
// whether this archive has been set up. It must not leak how many recovery
// codes are left, which is a fact about the credentials rather than about
// whether to show a sign-in button.
func TestAuthStatusTellsAStrangerNothingButWhetherItIsSetUp(t *testing.T) {
	h := newHarness(t)

	resp := h.raw(t, http.MethodGet, "/v1/auth/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got authStatus
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SignedIn {
		t.Error("an anonymous caller was reported as signed in")
	}
	if got.Recovery != nil {
		t.Errorf("recoveryRemaining = %d, want it withheld from an anonymous caller", *got.Recovery)
	}
}

// The credential-management endpoints, which nothing else in this package
// touches and which are therefore the easy ones to break.
//
// It exists because they were broken: the first version of migration 0024 had
// no `transports` column and every query here reads one, so `GET
// /v1/auth/passkeys` failed against a real schema while the whole suite stayed
// green. Reading the columns back is the point of the test.
func TestThePasskeyEndpointsAnswer(t *testing.T) {
	h := newHarness(t)
	gallery := h.gallery(t)

	resp, err := gallery.Client().Get(gallery.URL + "/v1/auth/passkeys")
	if err != nil {
		t.Fatalf("GET /v1/auth/passkeys: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (%s), want 200", resp.StatusCode, raw)
	}

	var listed struct {
		Passkeys []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"passkeys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Minting is the other half, and it is what the status endpoint's
	// recoveryRemaining is read from.
	req, err := http.NewRequest(http.MethodPost, gallery.URL+"/v1/auth/recovery-codes", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	minted, err := gallery.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v1/auth/recovery-codes: %v", err)
	}
	defer minted.Body.Close()
	if minted.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(minted.Body)
		t.Fatalf("status = %d (%s), want 201", minted.StatusCode, raw)
	}

	var out struct {
		Codes []string `json:"codes"`
	}
	if err := json.NewDecoder(minted.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Codes) == 0 {
		t.Fatal("no recovery codes were returned")
	}

	// And a signed-in caller is told how many are left, where an anonymous one
	// is not.
	status, err := gallery.Client().Get(gallery.URL + "/v1/auth/status")
	if err != nil {
		t.Fatalf("GET /v1/auth/status: %v", err)
	}
	defer status.Body.Close()
	var got authStatus
	if err := json.NewDecoder(status.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Recovery == nil || *got.Recovery != len(out.Codes) {
		t.Errorf("recoveryRemaining = %v, want %d", got.Recovery, len(out.Codes))
	}
}

// postAnon issues a POST with no credential of any kind.
func (h *harness) postAnon(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return h.do(t, req)
}

func hasCookie(resp *http.Response, name string) bool {
	for _, c := range resp.Cookies() {
		if c.Name == name && c.Value != "" {
			return true
		}
	}
	return false
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
