package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/dominicclerici/photos-backup/server/internal/mlclient"
)

// photod's half of the search lease: the part that knows somebody is looking.
//
// photo-ml holds no state, opens no files and has never seen a browser. It
// cannot tell an archive nobody has opened in a week from one being scrolled
// right now, and until this file existed it did not have to — the encoder and
// the query parser were loaded when it started and never given back, so an idle
// machine held about 3GB of weights and a CUDA context all day for a search box
// nobody had open.
//
// The fact it was missing is one photod has in abundance and throws away
// several hundred times a minute: a request arrived for a page of the timeline,
// so there is a gallery open. So that is what is sent. Every authenticated
// gallery request renews a lease, page loads and app foregrounds ask for one
// explicitly, and photo-ml lets go a few minutes after the last one — which is
// the same "five minutes idle" the residency reaper was already guessing at,
// except that it is now a measurement rather than a guess.
//
// Deliberately not the upload path. A phone backing up its camera roll
// overnight is not somebody searching, and every one of those requests carries a
// device token through requireDevice rather than through the guard below — so
// the distinction is already drawn in the routing table and this only has to
// stand on the right side of it.

// searchRenewInterval bounds how often a lease is renewed, however much traffic
// arrives. A grid scrolling past four hundred thumbnails is four hundred
// requests inside a few seconds, and each one is a fact about the same gallery
// being open.
//
// It costs a little precision at the end: the lease can lapse up to this long
// after the last request, or up to this long before the last renewal would have
// suggested. Thirty seconds against a five-minute term is not worth a round
// trip per thumbnail to correct.
const searchRenewInterval = 30 * time.Second

// searchWarmTimeout bounds the one lease call somebody is waiting behind — the
// synchronous one in front of a search. Generous against a loopback POST that
// does no model work of its own, and short enough that a photo-ml which has
// wedged costs a search two seconds rather than a search.
const searchWarmTimeout = 2 * time.Second

// defaultSearchIdle is how long the models stay up after the last gallery
// request when nothing configured otherwise. See config.MLSearchIdle.
const defaultSearchIdle = 5 * time.Minute

// searchLease is the last answer photo-ml gave, and when.
//
// Cached rather than asked per request because the answer is about the archive
// rather than about the request: whether the card is ours is the same question
// for every visitor, and asking it once every searchRenewInterval is what keeps
// a scrolling grid from turning into a lease call per tile.
type searchLease struct {
	mu    sync.Mutex
	grant mlclient.Grant
	asked time.Time
	// inFlight stops a scrolling grid from queueing a goroutine per tile behind
	// the one call that is already going out.
	inFlight bool
	// flight serialises the call itself, so that the fire-and-forget renewal a
	// thumbnail started and the synchronous one a search needs collapse into
	// one request rather than two. Held across a loopback POST, which is why it
	// is not `mu`: `mu` is taken on every gallery request and must never be
	// held across anything that can wait.
	flight sync.Mutex
}

// warmSearch renews the lease if it has not been renewed recently, without
// making the caller wait.
//
// Fire-and-forget on purpose. This is called from the middleware in front of
// every gallery read, including the thumbnails, and a person waiting on a
// picture must never be waiting on a question about VRAM. The answer it fetches
// is for the search that has not been typed yet.
func (s *Server) warmSearch() {
	if s.ML == nil {
		return
	}
	s.search.mu.Lock()
	if s.search.inFlight || time.Since(s.search.asked) < searchRenewInterval {
		s.search.mu.Unlock()
		return
	}
	s.search.inFlight = true
	s.search.mu.Unlock()

	go func() {
		// Deliberately not the request's context. The request that noticed the
		// gallery was open is about to finish, and cancelling the lease renewal
		// with it would mean the lease was only ever renewed by requests that
		// outlived a loopback round trip.
		ctx, cancel := context.WithTimeout(context.Background(), searchWarmTimeout)
		defer cancel()
		s.renewSearchLease(ctx)
	}()
}

// searchGrant is what the search path asks: may this query use the models, and
// are they actually on the card.
//
// Synchronous when the cached answer is stale, because a search is a person
// waiting for an answer and the alternative is ranking their query by text
// while a perfectly good encoder sits one loopback call away. The call itself
// does no model work — photo-ml grants a lease immediately and loads the
// checkpoints on its own thread — so this is a couple of milliseconds in the
// ordinary case.
func (s *Server) searchGrant(ctx context.Context) mlclient.Grant {
	if s.ML == nil {
		return mlclient.Grant{}
	}
	s.search.mu.Lock()
	fresh := time.Since(s.search.asked) < searchRenewInterval
	grant := s.search.grant
	s.search.mu.Unlock()
	if fresh {
		return grant
	}

	ctx, cancel := context.WithTimeout(ctx, searchWarmTimeout)
	defer cancel()
	return s.renewSearchLease(ctx)
}

func (s *Server) renewSearchLease(ctx context.Context) mlclient.Grant {
	s.search.flight.Lock()
	defer s.search.flight.Unlock()

	// Asked again now that this call is the only one going out. A search that
	// arrived a millisecond behind a thumbnail's renewal has been waiting for
	// that renewal rather than racing it, and its answer is already here.
	s.search.mu.Lock()
	fresh, grant := time.Since(s.search.asked) < searchRenewInterval, s.search.grant
	s.search.inFlight = false
	s.search.mu.Unlock()
	if fresh {
		return grant
	}

	grant, err := s.ML.Lease(ctx, mlclient.LeaseSearch, s.searchIdle())

	if err != nil && !errors.Is(err, mlclient.ErrUnavailable) {
		// A 4xx, which means this photo-ml does not arbitrate leases at all —
		// an older build, or one started with a models list that registers
		// neither search model. Treated as consent rather than as a refusal,
		// because the alternative is a search box that silently ranks by words
		// alone for the rest of the process's life over a version skew. The
		// service's own residency reaper is still there and still correct; what
		// is lost is the pinning, not the models.
		grant = mlclient.Grant{Group: mlclient.LeaseSearch, Held: true, Ready: true, Reason: "this photo-ml does not arbitrate leases"}
		err = nil
	}
	if err != nil {
		// Unreachable. Not consent: the search path is about to fail against
		// the same socket, and it already knows how to say so.
		grant = mlclient.Grant{Group: mlclient.LeaseSearch, Reason: err.Error()}
	}

	s.search.mu.Lock()
	was := s.search.grant
	s.search.grant = grant
	s.search.asked = time.Now()
	s.search.mu.Unlock()

	// Logged on the edges only, the way the worker logs photo-ml going away. A
	// backfill that holds the card for four hours should be two lines in the
	// journal rather than four hundred and eighty.
	if was.Held != grant.Held {
		if grant.Held {
			s.logger().Info("photo-ml is holding the search models for the gallery",
				"reason", grant.Reason, "idle", s.searchIdle())
		} else {
			s.logger().Info("photo-ml will not hold the search models; searches will rank by words alone",
				"reason", grant.Reason)
		}
	}
	return grant
}

func (s *Server) searchIdle() time.Duration {
	if s.MLSearchIdle <= 0 {
		return defaultSearchIdle
	}
	return s.MLSearchIdle
}

// watching is the guard decorator that turns gallery traffic into a lease.
//
// It wraps rather than replaces the credential guard, and the order is the
// point: authenticate first, warm second. An unauthenticated request is not
// evidence that anybody has the gallery open — it is evidence that something
// found the port — and loading three gigabytes of weights for one would be a
// stranger's way of making this archive do work.
func (s *Server) watching(allow guard) guard {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return allow(func(w http.ResponseWriter, r *http.Request) {
			s.warmSearch()
			next(w, r)
		})
	}
}

// handleSearchWarm is the page load and the app coming to the foreground,
// said out loud.
//
// Everything it does, the middleware above would have done a moment later on
// the first timeline request — but a moment later is the difference between the
// checkpoints arriving while the first screen of thumbnails does and arriving
// after somebody has already typed. It also covers the app opening to a cached
// grid, where there may be no request for a while and the first thing that
// happens is a search.
//
// The grant comes back rather than an empty 204, because there is something
// worth saying with it: a gallery that knows the search models are not coming
// can draw its search box without promising more than the archive can do.
func (s *Server) handleSearchWarm(w http.ResponseWriter, r *http.Request) {
	if s.ML == nil {
		writeJSON(w, http.StatusOK, searchWarmResponse{
			Available: false,
			Note:      "photo-ml is not configured; searches rank by captions, tags, recognised text, filenames and place names",
		})
		return
	}
	grant := s.searchGrant(r.Context())
	writeJSON(w, http.StatusOK, searchWarmResponse{
		Available: grant.Held,
		Ready:     grant.Ready,
		Note:      grant.Reason,
	})
}

type searchWarmResponse struct {
	// Available is whether the archive expects to be able to rank by what a
	// photograph looks like at all right now.
	Available bool `json:"available"`
	// Ready is whether it can this second. False for the twenty seconds after a
	// page load while the checkpoints arrive, which is a real state and not an
	// error: searches in that window rank by words and say so.
	Ready bool `json:"ready"`
	// Note is photo-ml's own sentence about why, for the status page.
	Note string `json:"note,omitempty"`
}
