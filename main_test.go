package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Bloem-Studios/bloem-community-zerodayz1-mdblist/mdblist"
)

const matrixJSON = `{
  "title": "The Matrix", "year": 1999, "type": "movie",
  "ids": {"imdb": "tt0133093", "tmdb": 603, "mdblist": "a2na"},
  "ratings": [{"source": "imdb", "value": 8.7}, {"source": "popcorn", "value": 85}]
}`

// The imdb and tmdb capabilities both fire for the same title in one refresh.
// The cache must collapse them into a single upstream request.
func TestLookupCacheCollapsesCapabilities(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, matrixJSON)
	}))
	defer srv.Close()

	client := mdblist.NewClient("k", mdblist.WithBaseURL(srv.URL))
	s := &metadataServer{}

	// First: imdb capability.
	if _, err := s.lookup(context.Background(), client, "imdb", "movie", "tt0133093", nil); err != nil {
		t.Fatalf("imdb lookup: %v", err)
	}
	// Then: tmdb capability for the same title — should hit cache (response
	// carried tmdb=603, cached under that key too).
	info, err := s.lookup(context.Background(), client, "tmdb", "movie", "603", nil)
	if err != nil {
		t.Fatalf("tmdb lookup: %v", err)
	}
	if info == nil {
		t.Fatal("expected cached info")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls = %d; want 1 (cache should collapse)", got)
	}
}

func TestLoadManifest(t *testing.T) {
	m, err := loadManifest()
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if m.GetPluginId() != "silo.mdblist" {
		t.Errorf("plugin_id = %q", m.GetPluginId())
	}
	if len(m.GetCapabilities()) != 2 {
		t.Fatalf("want 2 capabilities, got %d", len(m.GetCapabilities()))
	}
	ids := map[string]bool{}
	for _, c := range m.GetCapabilities() {
		ids[c.GetId()] = true
	}
	if !ids["imdb"] || !ids["tmdb"] {
		t.Errorf("capability ids = %v; want imdb+tmdb", ids)
	}
}
