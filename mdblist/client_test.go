package mdblist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

const matrixJSON = `{
  "title": "The Matrix", "year": 1999, "released": "1999-03-31",
  "runtime": 136, "type": "movie", "language": "en", "country": "us",
  "certification": "R",
  "ids": {"imdb": "tt0133093", "trakt": 481, "tmdb": 603, "tvdb": 169, "mal": null, "mdblist": "a2na"},
  "ratings": [
    {"source": "imdb", "value": 8.7, "score": 87, "votes": 2252453},
    {"source": "tomatoes", "value": 83, "score": 83, "votes": 209},
    {"source": "popcorn", "value": 85, "score": 85, "votes": 1307885},
    {"source": "myanimelist", "value": null, "score": null, "votes": null}
  ]
}`

func TestGetMediaSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/imdb/movie/tt0133093" {
			t.Errorf("path = %q", got)
		}
		if got := r.URL.Query().Get("apikey"); got != "key123" {
			t.Errorf("apikey = %q", got)
		}
		fmt.Fprint(w, matrixJSON)
	}))
	defer srv.Close()

	c := NewClient("key123", WithBaseURL(srv.URL))
	info, err := c.GetMedia(context.Background(), "imdb", "movie", "tt0133093")
	if err != nil {
		t.Fatalf("GetMedia: %v", err)
	}
	if info == nil {
		t.Fatal("info is nil")
	}
	if v, ok := info.IMDbRating(); !ok || v != 8.7 {
		t.Errorf("IMDbRating = %v, %v; want 8.7, true", v, ok)
	}
	if v, ok := info.RTCritic(); !ok || v != 83 {
		t.Errorf("RTCritic = %v, %v; want 83, true", v, ok)
	}
	if v, ok := info.RTAudience(); !ok || v != 85 {
		t.Errorf("RTAudience = %v, %v; want 85, true", v, ok)
	}
	if _, ok := info.ratingValue("myanimelist"); ok {
		t.Error("myanimelist should be unrated (null value)")
	}
	if info.IDs.TMDB == nil || *info.IDs.TMDB != 603 {
		t.Errorf("TMDB id = %v; want 603", info.IDs.TMDB)
	}
	if info.IDs.MAL != nil {
		t.Errorf("MAL id = %v; want nil", info.IDs.MAL)
	}
}

func TestGetMediaNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"404", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }},
		{"error envelope", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"error":"not found"}`)
		}},
		{"empty body", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }},
		{"400 invalid id", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"Invalid media_id"}`)
		}},
		{"422", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnprocessableEntity) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			defer srv.Close()
			c := NewClient("k", WithBaseURL(srv.URL))
			info, err := c.GetMedia(context.Background(), "imdb", "movie", "tt0")
			if err != nil {
				t.Fatalf("err = %v; want nil", err)
			}
			if info != nil {
				t.Fatalf("info = %+v; want nil (not found)", info)
			}
		})
	}
}

func TestGetMedia429EntersCooldown(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient("k", WithBaseURL(srv.URL))
	if _, err := c.GetMedia(context.Background(), "imdb", "movie", "tt1"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v; want ErrRateLimited", err)
	}
	// Second call must short-circuit without hitting the server.
	if _, err := c.GetMedia(context.Background(), "imdb", "movie", "tt2"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second err = %v; want ErrRateLimited", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server calls = %d; want 1 (cooldown should short-circuit)", got)
	}
	cooling, until := c.CoolingDown()
	if !cooling {
		t.Error("expected client to be cooling down")
	}
	if d := time.Until(until); d < 100*time.Second || d > 121*time.Second {
		t.Errorf("cooldown ~120s expected, got %v", d)
	}
}

func TestGetMediaRemainingZeroEntersCooldown(t *testing.T) {
	var calls atomic.Int32
	reset := time.Now().Add(90 * time.Second).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		fmt.Fprint(w, matrixJSON)
	}))
	defer srv.Close()

	c := NewClient("k", WithBaseURL(srv.URL))
	// First call succeeds but observes remaining=0 -> cooldown.
	if _, err := c.GetMedia(context.Background(), "imdb", "movie", "tt1"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := c.GetMedia(context.Background(), "imdb", "movie", "tt2"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second err = %v; want ErrRateLimited", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server calls = %d; want 1", got)
	}
}

func TestThrottleSpacesRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, matrixJSON)
	}))
	defer srv.Close()

	c := NewClient("k", WithBaseURL(srv.URL), WithRequestsPerSecond(20)) // 50ms apart
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.GetMedia(context.Background(), "imdb", "movie", "tt"+strconv.Itoa(i)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// 3 calls at 50ms spacing -> at least ~100ms of enforced waiting.
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("elapsed = %v; throttle not enforced", elapsed)
	}
}
