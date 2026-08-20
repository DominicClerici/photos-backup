package mlclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dominicclerici/photos-backup/server/internal/mlclient"
)

// The one distinction this package exists to make. Everything below is a
// variation on it: did the service fail, or did the request?

func TestServiceFailuresAreUnavailable(t *testing.T) {
	// Every one of these is a way photo-ml can be absent rather than a way a
	// photograph can be wrong, and each has to reach the worker as something it
	// will put the job down for rather than spend an attempt on. A 503 is the
	// service saying so; a closed socket is it not saying anything.
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"unloadable model", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"detail":"OSError: could not load the checkpoint"}`, http.StatusServiceUnavailable)
		}},
		{"crashed mid-request", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		{"answering something that is not JSON", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html>a proxy got in the way</html>"))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			_, err := mlclient.New(srv.URL).EmbedTexts(context.Background(), []string{"a dog"})
			if !errors.Is(err, mlclient.ErrUnavailable) {
				t.Fatalf("err = %v, want it to wrap ErrUnavailable", err)
			}
		})
	}
}

func TestNothingListeningIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // the port is now closed, which is what a stopped unit looks like

	if _, err := mlclient.New(url).Health(context.Background()); !errors.Is(err, mlclient.ErrUnavailable) {
		t.Fatalf("Health err = %v, want it to wrap ErrUnavailable", err)
	}
	if _, err := mlclient.New(url).EmbedImages(context.Background(), [][]byte{{1, 2, 3}}); !errors.Is(err, mlclient.ErrUnavailable) {
		t.Fatalf("EmbedImages err = %v, want it to wrap ErrUnavailable", err)
	}
}

// A rendition that is not an image is a fact about that one file, and it has to
// burn attempts and eventually park the job — otherwise a genuinely corrupt
// derivative is retried against a healthy GPU forever.
func TestARefusedRequestIsNotUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"detail": "could not decode the image"})
	}))
	defer srv.Close()

	_, err := mlclient.New(srv.URL).EmbedImages(context.Background(), [][]byte{{0xff}})
	if err == nil {
		t.Fatal("a 400 should be an error")
	}
	if errors.Is(err, mlclient.ErrUnavailable) {
		t.Fatalf("err = %v, want an ordinary error rather than ErrUnavailable", err)
	}
	// The service's own sentence, not the JSON around it: this string ends up
	// in jobs.last_error and on the status page.
	if got := err.Error(); !strings.Contains(got, "could not decode the image") {
		t.Fatalf("err = %q, want it to carry the service's detail", got)
	}
}

func TestImagesAreSentAsBase64AndVectorsComeBackInOrder(t *testing.T) {
	var got struct {
		Images []string `json:"images"`
		Texts  []string `json:"texts"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{
			"model":      "siglip2-so400m-patch14-384",
			"dim":        3,
			"normalized": true,
			"vectors":    [][]float32{{1, 0, 0}, {0, 1, 0}},
		})
	}))
	defer srv.Close()

	out, err := mlclient.New(srv.URL).EmbedImages(context.Background(), [][]byte{{'a'}, {'b'}})
	if err != nil {
		t.Fatalf("EmbedImages: %v", err)
	}
	if len(got.Images) != 2 || got.Images[0] != "YQ==" || got.Images[1] != "Yg==" {
		t.Fatalf("sent %v, want the two bytes base64-encoded in order", got.Images)
	}
	if got.Texts != nil {
		t.Errorf("texts = %v, want the field omitted entirely", got.Texts)
	}
	if out.Model != "siglip2-so400m-patch14-384" || len(out.Vectors) != 2 || out.Vectors[1][1] != 1 {
		t.Fatalf("got %+v, want the service's own model name and both vectors in order", out)
	}
}

// The vectors are matched to frames positionally by the caller, so a batch that
// comes back the wrong length would silently describe frame 3 with frame 4's
// numbers. Cheap to check here; impossible to notice later.
func TestAShortBatchIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"model": "m", "dim": 3, "vectors": [][]float32{{1, 0, 0}},
		})
	}))
	defer srv.Close()

	_, err := mlclient.New(srv.URL).EmbedImages(context.Background(), [][]byte{{'a'}, {'b'}, {'c'}})
	if err == nil {
		t.Fatal("three images answered with one vector should be an error")
	}
	if errors.Is(err, mlclient.ErrUnavailable) {
		t.Fatalf("err = %v, want an ordinary error: the service answered, it just answered wrongly", err)
	}
}

func TestHealthReportsResidency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"ready":false,"device":"cuda","dtype":"float16",
			"models":[{"key":"vision","model":"siglip2-so400m-patch14-384","role":"resident","resident":false}]}`))
	}))
	defer srv.Close()

	h, err := mlclient.New(srv.URL).Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	// ok and ready are different questions, and the pool asks the second one: a
	// service that is listening but still pulling weights would only time out.
	if !h.OK || h.Ready {
		t.Fatalf("ok=%v ready=%v, want a listening service that is not warm yet", h.OK, h.Ready)
	}
	if len(h.Models) != 1 || h.Models[0].Resident {
		t.Fatalf("models = %+v, want the encoder reported as not yet resident", h.Models)
	}
}
