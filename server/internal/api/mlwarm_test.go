package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/mlclient"
)

// leaseStub is photo-ml's /lease route and nothing else.
//
// Stubbed rather than run, because what is under test here is not what the GPU
// decides — it is what photod does with a grant, with a refusal, and with a
// service that has never heard of a lease, and only the first of those is a
// state a real photo-ml will hold on request.
type leaseStub struct {
	*httptest.Server
	mu sync.Mutex
	// grants is whether the lease is given. ready is whether the checkpoints
	// have arrived, which is a separate question by construction: a grant is
	// immediate and 3GB of weights are not.
	grants bool
	ready  bool
	reason string
	status int
	calls  int
	ttls   []float64
}

func newLeaseStub(t *testing.T) *leaseStub {
	t.Helper()
	s := &leaseStub{grants: true, ready: true, reason: "taken"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lease", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Group      string  `json:"group"`
			TTLSeconds float64 `json:"ttl_seconds"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		s.mu.Lock()
		s.calls++
		s.ttls = append(s.ttls, req.TTLSeconds)
		grants, ready, reason, status := s.grants, s.ready, s.reason, s.status
		s.mu.Unlock()

		if status != 0 {
			http.Error(w, `{"detail":"no such route"}`, status)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"group": req.Group, "held": grants, "ready": grants && ready,
			"reason": reason, "budget_bytes": 8 << 30,
		})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *leaseStub) set(grants, ready bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants, s.ready, s.reason = grants, ready, reason
}

func (s *leaseStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// A gallery that is open is what puts the models in VRAM. Before this, they
// were loaded when photo-ml started and never given back — about 3GB held all
// day for a search box nobody had open.
func TestOpeningTheGalleryTakesTheSearchLease(t *testing.T) {
	stub := newLeaseStub(t)
	s := &Server{ML: mlclient.New(stub.URL)}

	grant := s.searchGrant(context.Background())
	if !grant.Held || !grant.Ready {
		t.Fatalf("grant = %+v, want held and ready", grant)
	}
	if stub.count() != 1 {
		t.Fatalf("asked %d times, want once", stub.count())
	}
	if len(stub.ttls) != 1 || stub.ttls[0] != defaultSearchIdle.Seconds() {
		t.Errorf("ttl = %v, want the configured idle window", stub.ttls)
	}
}

// A grid scrolling past four hundred thumbnails is four hundred requests inside
// a few seconds, and each one is a fact about the same gallery being open. One
// lease call, not four hundred.
func TestGalleryTrafficDoesNotBecomeLeaseTraffic(t *testing.T) {
	stub := newLeaseStub(t)
	s := &Server{ML: mlclient.New(stub.URL)}

	for range 50 {
		s.warmSearch()
		s.searchGrant(context.Background())
	}
	for range 100 {
		if stub.count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := stub.count(); n != 1 {
		t.Errorf("asked %d times inside one renewal interval, want once", n)
	}
}

// Held is not the same question as ready, and the search path has to be able to
// see the difference: a query typed in the twenty seconds after a page load
// ranks by words rather than waiting behind a checkpoint.
func TestAWarmingLeaseIsHeldButNotReady(t *testing.T) {
	stub := newLeaseStub(t)
	stub.set(true, false, "taken")
	s := &Server{ML: mlclient.New(stub.URL)}

	grant := s.searchGrant(context.Background())
	if !grant.Held {
		t.Fatal("the card is ours")
	}
	if grant.Ready {
		t.Error("the weights are not on it yet")
	}
}

// A refusal is an answer, not a failure: an ingestion pass has the card, or
// something outside this archive is holding most of it. It arrives as a 200 and
// carries the sentence the page draws.
func TestARefusalIsCarriedThroughToTheSearchPage(t *testing.T) {
	stub := newLeaseStub(t)
	stub.set(false, false, "the ingest lease holds the card")
	s := &Server{ML: mlclient.New(stub.URL)}

	grant := s.searchGrant(context.Background())
	if grant.Held {
		t.Fatal("photo-ml refused")
	}
	note := holdingNote(grant.Reason)
	if note == "" || note == holdingNote("") {
		t.Errorf("note = %q, want photo-ml's own reason in it", note)
	}
}

// A photo-ml that has never heard of a lease must not mean a search box that
// silently ranks by words for the rest of the process's life. The service's own
// residency rules are still in place; what is lost is the pinning.
func TestAPhotoMLWithNoLeaseRouteStillGetsAsked(t *testing.T) {
	stub := newLeaseStub(t)
	stub.mu.Lock()
	stub.status = http.StatusNotFound
	stub.mu.Unlock()
	s := &Server{ML: mlclient.New(stub.URL)}

	grant := s.searchGrant(context.Background())
	if !grant.Held || !grant.Ready {
		t.Errorf("grant = %+v, want a version skew treated as consent", grant)
	}
}

// A service that is gone is not consent. The search path is about to fail
// against the same socket and already knows how to say so.
func TestAnUnreachableServiceIsNotConsent(t *testing.T) {
	stub := newLeaseStub(t)
	stub.Close()
	s := &Server{ML: mlclient.New(stub.URL)}

	if grant := s.searchGrant(context.Background()); grant.Held {
		t.Errorf("grant = %+v, want a closed socket to hold nothing", grant)
	}
}

func TestNoPhotoMLHoldsNothing(t *testing.T) {
	s := &Server{}
	s.warmSearch() // must not panic or dial anything
	if grant := s.searchGrant(context.Background()); grant.Held {
		t.Error("an archive with no GPU service holds no lease")
	}
}
