package mdblist

// IDs holds the cross-provider identifiers MDBList returns for a title. tmdb,
// tvdb, trakt and mal are numeric in the API and may be null; imdb and mdblist
// are strings. Pointers distinguish "absent/null" from a real zero.
type IDs struct {
	IMDB    string `json:"imdb"`
	TMDB    *int64 `json:"tmdb"`
	TVDB    *int64 `json:"tvdb"`
	Trakt   *int64 `json:"trakt"`
	MAL     *int64 `json:"mal"`
	MDBList string `json:"mdblist"`
}

// Rating is one entry from the MDBList ratings array. Value/Score may be null
// (e.g. an unrated source), so they are pointers. Value is the source's native
// scale (IMDb 0-10, Rotten Tomatoes 0-100); Score is MDBList's normalised 0-100.
type Rating struct {
	Source string   `json:"source"`
	Value  *float64 `json:"value"`
	Score  *float64 `json:"score"`
	Votes  *int64   `json:"votes"`
}

// Genre is one entry from the genres array.
type Genre struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// MediaInfo is the response from GET /{provider}/{type}/{id}. Only the fields
// this plugin consumes are modelled; MDBList returns more (budget, revenue,
// streams, watch_providers, awards, commonsense_media, …) but the Silo DB has
// no column for them, so they are intentionally omitted.
type MediaInfo struct {
	Title         string   `json:"title"`
	Year          int      `json:"year"`
	Released      string   `json:"released"`
	Description   string   `json:"description"`
	Runtime       int      `json:"runtime"`
	Type          string   `json:"type"`
	IDs           IDs      `json:"ids"`
	Ratings       []Rating `json:"ratings"`
	Genres        []Genre  `json:"genres"`
	Language      string   `json:"language"`
	Country       string   `json:"country"`
	Certification string   `json:"certification"`
	Poster        string   `json:"poster"`
	Backdrop      string   `json:"backdrop"`

	// Error is populated when MDBList returns an error envelope instead of a
	// title (e.g. unknown id). Empty on success.
	Error string `json:"error"`
}

// ratingValue returns the native Value for the named source, or (0, false) if
// the source is absent or unrated.
func (m *MediaInfo) ratingValue(source string) (float64, bool) {
	for _, r := range m.Ratings {
		if r.Source == source && r.Value != nil {
			return *r.Value, true
		}
	}
	return 0, false
}

// IMDbRating returns the IMDb rating on its native 0-10 scale.
func (m *MediaInfo) IMDbRating() (float64, bool) { return m.ratingValue("imdb") }

// RTCritic returns the Rotten Tomatoes critic ("tomatometer") score, 0-100.
func (m *MediaInfo) RTCritic() (float64, bool) { return m.ratingValue("tomatoes") }

// RTAudience returns the Rotten Tomatoes audience ("popcornmeter") score, 0-100.
// MDBList labels this source "popcorn".
func (m *MediaInfo) RTAudience() (float64, bool) { return m.ratingValue("popcorn") }
