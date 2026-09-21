package main

import (
	"testing"

	"github.com/Bloem-Studios/bloem-community-zerodayz1-mdblist/mdblist"
)

func i64(v int64) *int64       { return &v }
func f64(v float64) *float64   { return &v }

func sampleMovie() *mdblist.MediaInfo {
	return &mdblist.MediaInfo{
		Title: "The Matrix", Year: 1999, Released: "1999-03-31", Runtime: 136,
		Type: "movie", Language: "en", Country: "us", Certification: "R",
		IDs: mdblist.IDs{IMDB: "tt0133093", TMDB: i64(603), TVDB: i64(169), Trakt: i64(481), MDBList: "a2na"},
		Ratings: []mdblist.Rating{
			{Source: "imdb", Value: f64(8.7)},
			{Source: "tomatoes", Value: f64(83)},
			{Source: "popcorn", Value: f64(85)},
			{Source: "tmdb", Value: f64(82)},
			{Source: "metacritic", Value: f64(73)},
		},
	}
}

func TestRatingsStruct(t *testing.T) {
	s := ratingsStruct(sampleMovie())
	if s == nil {
		t.Fatal("ratingsStruct returned nil")
	}
	m := s.AsMap()
	if m["imdb"] != 8.7 {
		t.Errorf("imdb = %v; want 8.7", m["imdb"])
	}
	if m["rt_critic"] != float64(83) {
		t.Errorf("rt_critic = %v; want 83", m["rt_critic"])
	}
	if m["rt_audience"] != float64(85) {
		t.Errorf("rt_audience = %v; want 85", m["rt_audience"])
	}
	if _, ok := m["tmdb"]; ok {
		t.Error("tmdb rating should be skipped (TMDB owns it)")
	}
	if _, ok := m["metacritic"]; ok {
		t.Error("metacritic has no Silo column and must be dropped")
	}
}

func TestRatingsStructNilWhenNoRatings(t *testing.T) {
	info := &mdblist.MediaInfo{Title: "x", IDs: mdblist.IDs{IMDB: "tt1"}}
	if ratingsStruct(info) != nil {
		t.Error("expected nil ratings struct when there are no usable ratings")
	}
}

func TestBuildMetadataItemGapFill(t *testing.T) {
	item := buildMetadataItem(sampleMovie(), "movie", true)
	if item == nil {
		t.Fatal("item is nil")
	}
	if item.GetContentRating() != "R" {
		t.Errorf("ContentRating = %q; want R", item.GetContentRating())
	}
	if item.GetOriginalLanguage() != "en" {
		t.Errorf("OriginalLanguage = %q; want en", item.GetOriginalLanguage())
	}
	if got := item.GetCountries(); len(got) != 1 || got[0] != "US" {
		t.Errorf("Countries = %v; want [US]", got)
	}
	if item.GetRuntime() != 136 {
		t.Errorf("Runtime = %d; want 136", item.GetRuntime())
	}
	if item.GetReleaseDate() != "1999-03-31" {
		t.Errorf("ReleaseDate = %q", item.GetReleaseDate())
	}
	if item.GetFirstAirDate() != "" {
		t.Errorf("FirstAirDate = %q; want empty for movie", item.GetFirstAirDate())
	}
	// Provider IDs accumulate, including the mdblist id.
	ids := item.GetProviderIds().AsMap()
	if ids["tmdb"] != "603" || ids["tvdb"] != "169" || ids["mdblist"] != "a2na" {
		t.Errorf("provider ids = %v", ids)
	}
	// Title/overview must NOT be set (richer providers own them).
	if item.GetTitle() != "" {
		t.Errorf("Title = %q; want empty (non-destructive)", item.GetTitle())
	}
}

func TestBuildMetadataItemGapFillDisabled(t *testing.T) {
	item := buildMetadataItem(sampleMovie(), "movie", false)
	if item == nil {
		t.Fatal("item is nil")
	}
	if item.GetContentRating() != "" || item.GetRuntime() != 0 || len(item.GetCountries()) != 0 {
		t.Error("gap-fill disabled but scalar fields were set")
	}
	if item.GetRatings() == nil {
		t.Error("ratings should still be emitted with gap-fill off")
	}
}

func TestBuildMetadataItemShowUsesFirstAirDate(t *testing.T) {
	info := sampleMovie()
	info.Type = "show"
	item := buildMetadataItem(info, "series", true)
	if item.GetFirstAirDate() != "1999-03-31" {
		t.Errorf("FirstAirDate = %q; want 1999-03-31", item.GetFirstAirDate())
	}
	if item.GetReleaseDate() != "" {
		t.Errorf("ReleaseDate = %q; want empty for series", item.GetReleaseDate())
	}
	if item.GetRuntime() != 0 {
		t.Error("runtime must not be emitted for series (aggregate is misleading)")
	}
}

func TestBuildMetadataItemNilWhenNothing(t *testing.T) {
	info := &mdblist.MediaInfo{Title: "x", IDs: mdblist.IDs{}} // no ids, no ratings
	if buildMetadataItem(info, "movie", false) != nil {
		t.Error("expected nil item when there is nothing to contribute")
	}
}

func TestSelectLookup(t *testing.T) {
	cases := []struct {
		name         string
		ids          map[string]string
		fallback     string
		wantProvider string
		wantID       string
	}{
		{"prefers imdb", map[string]string{"imdb": "tt1", "tmdb": "5"}, "", "imdb", "tt1"},
		{"falls to tmdb", map[string]string{"tmdb": "5"}, "", "tmdb", "5"},
		{"fallback imdb by prefix", map[string]string{}, "tt9", "imdb", "tt9"},
		{"fallback tmdb numeric", map[string]string{}, "603", "tmdb", "603"},
		{"empty", map[string]string{}, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, id := selectLookup(tc.ids, tc.fallback)
			if p != tc.wantProvider || id != tc.wantID {
				t.Errorf("selectLookup = (%q,%q); want (%q,%q)", p, id, tc.wantProvider, tc.wantID)
			}
		})
	}
}

func TestMediaTypeFor(t *testing.T) {
	cases := map[string]string{"movie": "movie", "series": "show", "show": "show", "tv": "show", "": "movie"}
	for in, want := range cases {
		if got := mediaTypeFor(in); got != want {
			t.Errorf("mediaTypeFor(%q) = %q; want %q", in, got, want)
		}
	}
}
