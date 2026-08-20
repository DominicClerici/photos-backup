// Package mlclient talks to photo-ml, and is careful about the difference
// between "this photograph is broken" and "the GPU service is away".
//
// That distinction is the whole of PROJECT.md §4's hard rule expressed in Go.
// photo-ml is optional forever: it can be down, mid-model-swap, or saturating
// the card, and none of those may cost the archive anything. A client that
// returned one undifferentiated error would make an outage look like sixty
// thousand corrupt files — the vision jobs would spend their five attempts
// against a closed socket and park themselves as permanently failed, and the
// only way back would be a hand-written UPDATE.
//
// So every failure that is about the service rather than about the bytes wraps
// ErrUnavailable, and the worker puts such a job down without spending an
// attempt on it. Everything else — a rendition that is not an image, a batch
// larger than the service will take — is an ordinary error and burns attempts
// the way any other bad file does.
package mlclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnavailable means the service could not be reached or could not answer:
// nothing was learned about the request, and asking again later is the correct
// response. Every transport failure and every 5xx wraps it.
var ErrUnavailable = errors.New("photo-ml is unavailable")

// DefaultTimeout is generous on purpose. A warm batch of six frames is under a
// second, but the first request after a start can be waiting on 1.8GB of
// weights coming off a mirror, and timing that out would turn a cold service
// into a queue of failures.
const DefaultTimeout = 2 * time.Minute

// HealthTimeout is much shorter, and deliberately not DefaultTimeout. /health
// answers from photo-ml's first moment on purpose — that is how "warming up"
// and "gone" are told apart — so anything slow here is a service that is not
// healthy, whatever it eventually says. It also bounds how long the vision
// pool's shared probe can hold its lock: a hung service should leave the pool
// idle, not leave its workers blocked on each other for two minutes.
const HealthTimeout = 5 * time.Second

// errorBodyLimit bounds how much of a failure response is read into an error
// message. Enough for FastAPI's JSON detail, not enough for a stack trace to
// end up in the jobs table.
const errorBodyLimit = 4 << 10

type Client struct {
	// BaseURL is where photo-ml is listening, e.g. http://127.0.0.1:8789.
	BaseURL string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: DefaultTimeout},
	}
}

// Health is what GET /health answers: up, warm, and on what.
type Health struct {
	OK bool `json:"ok"`
	// Ready is false while a resident model is still loading, and it is the
	// field that matters. The service answers /health from its first moment —
	// deliberately, so that warming up and being gone are distinguishable — and
	// handing work to a service that is listening but has no weights yet would
	// only produce a timeout.
	Ready  bool          `json:"ready"`
	Device string        `json:"device"`
	Dtype  string        `json:"dtype"`
	Models []ModelStatus `json:"models"`
}

// ModelStatus is one row of §6's residency table, as the service currently sees
// it.
type ModelStatus struct {
	Key      string `json:"key"`
	Model    string `json:"model"`
	Role     string `json:"role"`
	Resident bool   `json:"resident"`
	InUse    int    `json:"in_use"`
	Error    string `json:"error"`
}

// Embeddings is a batch of unit vectors and the model that produced them.
//
// Model is echoed back rather than assumed, and it is what gets stored on the
// row: asset_embeddings.model records what actually made a vector, so a service
// quietly running a different checkpoint shows up as a second model in the
// table rather than as silently incomparable numbers under the first one's
// name.
type Embeddings struct {
	Model      string      `json:"model"`
	Dim        int         `json:"dim"`
	Normalized bool        `json:"normalized"`
	Vectors    [][]float32 `json:"vectors"`
	TookMS     float64     `json:"took_ms"`
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	ctx, cancel := context.WithTimeout(ctx, HealthTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health", nil)
	if err != nil {
		return h, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return h, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return h, fmt.Errorf("%w: /health answered %s", ErrUnavailable, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return h, fmt.Errorf("%w: unreadable /health: %w", ErrUnavailable, err)
	}
	return h, nil
}

// EmbedImages turns rendition bytes into vectors, in the order given.
func (c *Client) EmbedImages(ctx context.Context, images [][]byte) (Embeddings, error) {
	encoded := make([]string, len(images))
	for i, img := range images {
		encoded[i] = base64.StdEncoding.EncodeToString(img)
	}
	return c.embed(ctx, embedRequest{Images: encoded})
}

// EmbedTexts turns query phrases into vectors in the same space as the images.
//
// One model with two towers is the property the whole search feature rests on:
// "at the beach" and a photograph of one land near each other, which is how a
// beach nobody wrote the word "beach" about becomes findable.
func (c *Client) EmbedTexts(ctx context.Context, texts []string) (Embeddings, error) {
	return c.embed(ctx, embedRequest{Texts: texts})
}

type embedRequest struct {
	Images []string `json:"images,omitempty"`
	Texts  []string `json:"texts,omitempty"`
}

func (c *Client) embed(ctx context.Context, body embedRequest) (Embeddings, error) {
	var out Embeddings

	payload, err := json.Marshal(body)
	if err != nil {
		return out, fmt.Errorf("encode embed request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/embed", bytes.NewReader(payload))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail := readDetail(resp.Body)
		// 5xx is the service failing and 4xx is this request being wrong, and
		// they are treated as different kinds of thing all the way up: the
		// first is retried for free, the second spends an attempt and
		// eventually parks the job with the reason kept verbatim.
		if resp.StatusCode >= 500 {
			return out, fmt.Errorf("%w: /embed answered %s: %s", ErrUnavailable, resp.Status, detail)
		}
		return out, fmt.Errorf("photo-ml refused the request (%s): %s", resp.Status, detail)
	}

	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("%w: unreadable /embed response: %w", ErrUnavailable, err)
	}
	if len(out.Vectors) != count(body) {
		return out, fmt.Errorf("photo-ml returned %d vectors for %d items", len(out.Vectors), count(body))
	}
	return out, nil
}

func count(body embedRequest) int {
	if body.Images != nil {
		return len(body.Images)
	}
	return len(body.Texts)
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: DefaultTimeout}
}

// readDetail pulls FastAPI's `{"detail": "..."}` out of a failure, falling back
// to the raw body. What ends up here ends up in jobs.last_error and on the
// status page, so it is worth it being the sentence the service wrote rather
// than the JSON it wrapped it in.
func readDetail(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, errorBodyLimit))
	if err != nil || len(raw) == 0 {
		return "no detail"
	}
	var wrapped struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Detail) > 0 {
		var s string
		if json.Unmarshal(wrapped.Detail, &s) == nil {
			return s
		}
		return string(wrapped.Detail)
	}
	return strings.TrimSpace(string(raw))
}
