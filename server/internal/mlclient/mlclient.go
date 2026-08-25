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

	"github.com/dominicclerici/photos-backup/server/internal/searchquery"
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

// ParseTimeout bounds the one call on this client that a person is waiting for.
//
// Everything else here is background work where two minutes is a reasonable
// wait for a cold model. /parse is in front of a search box, and a search box
// that hangs is worse than one that does not understand "last summer" — the
// grammar has already produced an answer by the time this is asked, so giving
// up costs a refinement rather than the search.
const ParseTimeout = 8 * time.Second

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

// Description is what a captioner made of one frame: a sentence and a handful
// of words.
//
// One per image, and the joining is this side's job. photo-ml holds no state
// and knows nothing about assets — it is handed some pictures and says what is
// in each of them — so deciding that three frames of a clip are one video is a
// decision for the process that knows what a video is. See worker.runDescribe.
type Description struct {
	Caption string `json:"caption"`
	Tags    []Tag  `json:"tags"`
}

// Tag is one word and how sure the model was. Free-form: ML_IMAGES.md §2 chose
// an open vocabulary over one guessed in advance, and the cleanup is
// tags.canonical_id.
type Tag struct {
	Name       string  `json:"name"`
	Confidence float32 `json:"confidence"`
}

// Descriptions is a batch of them, and the model that wrote them.
//
// Model is echoed back rather than assumed, exactly as /embed echoes it, and
// for the same reason: asset_descriptions.model records what actually produced
// a caption, so a service quietly running a different checkpoint shows up as a
// second model in the table rather than as silently different prose under the
// first one's name.
type Descriptions struct {
	Model   string        `json:"model"`
	Results []Description `json:"results"`
	TookMS  float64       `json:"took_ms"`
}

// Recognition is the text found in one frame.
//
// Text is already assembled and already filtered by confidence, on the far side
// of the socket, because the thresholds belong with the recogniser that
// produced the numbers. Lines carry the boxes for anything that wants to draw
// them; nothing does yet, and asset_ocr stores only the text.
type Recognition struct {
	Text  string    `json:"text"`
	Lines []OCRLine `json:"lines"`
}

type OCRLine struct {
	Text       string     `json:"text"`
	Confidence float32    `json:"confidence"`
	Box        [4]float32 `json:"box"`
}

type Recognitions struct {
	Model   string        `json:"model"`
	Results []Recognition `json:"results"`
	TookMS  float64       `json:"took_ms"`
}

// Describe asks what a set of frames is of, in the order given.
func (c *Client) Describe(ctx context.Context, images [][]byte) (Descriptions, error) {
	var out Descriptions
	body := map[string]any{"images": encode(images)}
	if err := c.post(ctx, "/describe", body, &out); err != nil {
		return out, err
	}
	if len(out.Results) != len(images) {
		return out, fmt.Errorf("photo-ml described %d of %d images", len(out.Results), len(images))
	}
	return out, nil
}

// Recognize asks what a set of frames says.
//
// describeQueueEmpty is a fact about this side that photo-ml cannot know and
// needs: whether there is any captioning work outstanding. It decides whether
// the recogniser may take the card back from an idle captioner, which is the
// one direction residency.py will not evict in on its own — see its _evict_for.
// A fact rather than an instruction, because which model may displace which is
// photo-ml's policy and this service has never seen the queue.
func (c *Client) Recognize(ctx context.Context, images [][]byte, describeQueueEmpty bool) (Recognitions, error) {
	var out Recognitions
	body := map[string]any{"images": encode(images), "describe_queue_empty": describeQueueEmpty}
	if err := c.post(ctx, "/ocr", body, &out); err != nil {
		return out, err
	}
	if len(out.Results) != len(images) {
		return out, fmt.Errorf("photo-ml read %d of %d images", len(out.Results), len(images))
	}
	return out, nil
}

// Judgement is one word of the tag vocabulary, judged.
//
// Score is P(junk) rather than a bit, and it is stored: ML_IMAGES.md §9's
// cleanup is reviewed by a person, and the order to read a thousand verdicts in
// is "what was the model surest about", because a confident wrong answer is the
// one worth catching.
type Judgement struct {
	Word  string  `json:"word"`
	Junk  bool    `json:"junk"`
	Score float32 `json:"score"`
}

// Judgements is a batch of them, and the model that made them.
type Judgements struct {
	Model   string      `json:"model"`
	Results []Judgement `json:"results"`
	TookMS  float64     `json:"took_ms"`
}

// MaxBatch is how many items go in one request to any of the list routes.
//
// It mirrors photo-ml's PHOTO_ML_MAX_BATCH default, which every route there is
// bounded by. Raising it on the service without raising it here costs nothing;
// the reverse is a 413, which mlclient reports as an ordinary error because it
// is one — a batch too large is a caller's mistake and not an outage.
const MaxBatch = 32

// Triage asks whether these words are worth keeping — ML_IMAGES.md §9, the
// stage before the merge.
//
// A claim rather than an answer, in the way Parse is: the Go side writes these
// verdicts only where nobody has given one, and a person confirms them. See
// db.PutTriage.
//
// Answers come back in the order the words went in, and the count is checked
// for the reason Describe checks it: a service that answered about thirty-one
// of thirty-two words would otherwise have its verdicts written against the
// wrong words from that point on, silently.
func (c *Client) Triage(ctx context.Context, words []string) (Judgements, error) {
	var out Judgements
	if len(words) == 0 {
		return out, nil
	}
	if err := c.post(ctx, "/triage", map[string]any{"words": words}, &out); err != nil {
		return out, err
	}
	if len(out.Results) != len(words) {
		return out, fmt.Errorf("photo-ml judged %d of %d words", len(out.Results), len(words))
	}
	for i, r := range out.Results {
		if r.Word != words[i] {
			return out, fmt.Errorf("photo-ml answered about %q where %q was asked", r.Word, words[i])
		}
	}
	return out, nil
}

// Parse asks the small instruct model what a typed sentence was asking for.
//
// The answer is a claim, not a parse. internal/searchquery owns the grammar
// that actually decides, and Merge is where everything below is checked against
// what this archive contains before any of it is believed — a person who is not
// in asset_people, a date that does not read, a phrase nobody typed. See §11 for
// why that asymmetry is the whole design.
//
// today is passed rather than assumed because the model has no clock, and
// "last summer" is unanswerable without one. people is passed because it is
// eleven names and it is the one thing the model can do that the grammar
// cannot: notice that "the ski trip with Chris" mentions somebody, when the
// grammar only ever finds a name spelled the way the archive spells it.
func (c *Client) Parse(ctx context.Context, query string, today time.Time, people []string) (searchquery.ModelQuery, error) {
	var out searchquery.ModelQuery
	body := map[string]any{
		"query":  query,
		"today":  today.UTC().Format(time.DateOnly),
		"people": people,
	}
	ctx, cancel := context.WithTimeout(ctx, ParseTimeout)
	defer cancel()

	err := c.post(ctx, "/parse", body, &out)
	return out, err
}

func encode(images [][]byte) []string {
	encoded := make([]string, len(images))
	for i, img := range images {
		encoded[i] = base64.StdEncoding.EncodeToString(img)
	}
	return encoded
}

// post is embed's transport, generalised: the same 5xx-versus-4xx rule, which
// is the whole of what this package exists to get right.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail := readDetail(resp.Body)
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w: %s answered %s: %s", ErrUnavailable, path, resp.Status, detail)
		}
		return fmt.Errorf("photo-ml refused the request (%s): %s", resp.Status, detail)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: unreadable %s response: %w", ErrUnavailable, path, err)
	}
	return nil
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
