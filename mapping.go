package main

import (
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/zerodayz1/silo-plugin-mdblist/mdblist"
)

// providerPreference is the order in which we pick an ID to look up by. IMDb
// first because it is the most universal and is one of our two capabilities;
// tmdb second (the other capability); then the rest as fallbacks.
var providerPreference = []string{"imdb", "tmdb", "tvdb", "trakt", "mal"}

// selectLookup chooses the (provider, id) to query from the available IDs,
// falling back to the firing capability's ProviderId when the struct is sparse.
func selectLookup(ids map[string]string, fallbackID string) (provider, id string) {
	for _, p := range providerPreference {
		if v := ids[p]; v != "" {
			return p, v
		}
	}
	// No recognised ID in the struct; the capability ID is imdb or tmdb, but we
	// can't tell which from the request alone. IMDb IDs are unmistakable
	// (tt-prefixed); anything else we treat as a TMDB numeric ID.
	if fallbackID != "" {
		if strings.HasPrefix(fallbackID, "tt") {
			return "imdb", fallbackID
		}
		return "tmdb", fallbackID
	}
	return "", ""
}

// mediaTypeFor maps a Silo item_type to an MDBList media_type. Silo uses
// "movie" and "series"; MDBList uses "movie" and "show".
func mediaTypeFor(itemType string) string {
	switch strings.ToLower(strings.TrimSpace(itemType)) {
	case "series", "show", "tv", "tvshow":
		return "show"
	default:
		return "movie"
	}
}

// cacheKey builds the in-process cache key for a lookup.
func cacheKey(provider, mediaType, id string) string {
	return provider + "/" + mediaType + "/" + id
}

// responseIDs extracts the cross-provider IDs present in a response, so a fetch
// keyed by (say) tmdb can also satisfy a later imdb lookup of the same title.
func responseIDs(info *mdblist.MediaInfo) map[string]string {
	out := make(map[string]string, 5)
	if info.IDs.IMDB != "" {
		out["imdb"] = info.IDs.IMDB
	}
	if info.IDs.TMDB != nil {
		out["tmdb"] = strconv.FormatInt(*info.IDs.TMDB, 10)
	}
	if info.IDs.TVDB != nil {
		out["tvdb"] = strconv.FormatInt(*info.IDs.TVDB, 10)
	}
	if info.IDs.Trakt != nil {
		out["trakt"] = strconv.FormatInt(*info.IDs.Trakt, 10)
	}
	if info.IDs.MAL != nil {
		out["mal"] = strconv.FormatInt(*info.IDs.MAL, 10)
	}
	return out
}

// buildMetadataItem maps an MDBList response to a Silo MetadataItem. It always
// emits the ratings and the accumulated provider IDs; when gapFill is true it
// also fills a handful of scalar fields TMDB/TVDB sometimes lack. It never sets
// title/overview/genres/images, which richer providers own. Returns nil if
// there is nothing worth contributing.
func buildMetadataItem(info *mdblist.MediaInfo, itemType string, gapFill bool) *pluginv1.MetadataItem {
	ratings := ratingsStruct(info)
	providerIDs := providerIDStruct(info)

	item := &pluginv1.MetadataItem{
		ItemType:    itemType,
		ProviderIds: providerIDs,
		Ratings:     ratings,
	}
	// ProviderId carries the canonical MDBList id for traceability.
	if info.IDs.MDBList != "" {
		item.ProviderId = info.IDs.MDBList
	} else if info.IDs.IMDB != "" {
		item.ProviderId = info.IDs.IMDB
	}

	hasContribution := ratings != nil

	if gapFill {
		if info.Certification != "" {
			item.ContentRating = info.Certification
			hasContribution = true
		}
		if lang := strings.TrimSpace(info.Language); lang != "" {
			item.OriginalLanguage = strings.ToLower(lang)
			hasContribution = true
		}
		if c := strings.TrimSpace(info.Country); c != "" {
			item.Countries = []string{strings.ToUpper(c)}
			hasContribution = true
		}
		if info.Year > 0 {
			item.Year = int32(info.Year)
		}
		if date := strings.TrimSpace(info.Released); date != "" {
			if mediaTypeFor(itemType) == "show" {
				item.FirstAirDate = date
			} else {
				item.ReleaseDate = date
			}
		}
		// Runtime only for movies; the show endpoint reports an aggregate that
		// would be misleading as a per-episode runtime.
		if mediaTypeFor(itemType) == "movie" && info.Runtime > 0 {
			item.Runtime = int32(info.Runtime)
		}
	}

	if !hasContribution {
		return nil
	}
	return item
}

// ratingsStruct maps MDBList ratings onto the four keys the Silo DB persists:
// imdb (0-10), tmdb (skipped — TMDB owns it), rt_critic and rt_audience (0-100).
// silo-server's ratingsFromStruct reads only these float keys.
func ratingsStruct(info *mdblist.MediaInfo) *structpb.Struct {
	values := make(map[string]any, 3)
	if v, ok := info.IMDbRating(); ok && v > 0 {
		values["imdb"] = v
	}
	if v, ok := info.RTCritic(); ok && v > 0 {
		values["rt_critic"] = v
	}
	if v, ok := info.RTAudience(); ok && v > 0 {
		values["rt_audience"] = v
	}
	if len(values) == 0 {
		return nil
	}
	s, err := structpb.NewStruct(values)
	if err != nil {
		return nil
	}
	return s
}

// providerIDStruct emits every cross-provider ID MDBList returned. silo-server
// always accumulates provider IDs (never overwrites), so this is a pure-win
// backfill of missing imdb/tmdb/tvdb/trakt/mal cross-references.
func providerIDStruct(info *mdblist.MediaInfo) *structpb.Struct {
	ids := responseIDs(info)
	if info.IDs.MDBList != "" {
		ids["mdblist"] = info.IDs.MDBList
	}
	if len(ids) == 0 {
		return nil
	}
	converted := make(map[string]any, len(ids))
	for k, v := range ids {
		converted[k] = v
	}
	s, err := structpb.NewStruct(converted)
	if err != nil {
		return nil
	}
	return s
}

// stringMapFromStruct flattens a structpb.Struct of string values to a map.
func stringMapFromStruct(value *structpb.Struct) map[string]string {
	result := make(map[string]string)
	if value == nil {
		return result
	}
	for key, raw := range value.AsMap() {
		switch v := raw.(type) {
		case string:
			if v != "" {
				result[key] = v
			}
		case float64:
			// Some callers pass numeric IDs as numbers.
			result[key] = strconv.FormatInt(int64(v), 10)
		}
	}
	return result
}
