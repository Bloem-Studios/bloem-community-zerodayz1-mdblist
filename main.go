// Command silo-plugin-mdblist is a Silo metadata-provider plugin that enriches
// media items with ratings and a few gap-fill fields from the MDBList API
// (https://api.mdblist.com). It replaces the OMDb plugin: MDBList sources the
// same IMDb + Rotten Tomatoes critic scores OMDb gave us, plus the Rotten
// Tomatoes audience score, from one richer API.
//
// Design summary (see README.md for the full rationale):
//   - Two metadata_provider.v1 capabilities, "imdb" and "tmdb", so the plugin
//     fires for any item carrying either ID. A short-lived in-process cache
//     collapses the two back-to-back calls for the same title into one request.
//   - Non-destructive: silo-server merges providers with MergeFillEmpty (first
//     non-empty value wins), so emitting ratings + optional gap-fill fields only
//     ever fills blanks left by TMDB/TVDB — it never overwrites richer data.
//   - Rate-limit aware: honours MDBList's X-RateLimit-* / Retry-After headers,
//     self-throttles (requests_per_second), and short-circuits while a depleted
//     quota is cooling down — so a downgraded key after ingest degrades cleanly.
package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/zerodayz1/silo-plugin-mdblist/mdblist"
)

// version is set at build time via -ldflags "-X main.version=...".
var version string

// cacheTTL bounds how long a fetched (or not-found) lookup is reused. It exists
// purely to collapse the two capability calls (imdb + tmdb) for the same item
// in a single refresh into one API call; it is deliberately short.
const cacheTTL = 5 * time.Minute

//go:embed manifest.json
var manifestJSON []byte

type runtimeServer struct {
	pluginv1.UnimplementedRuntimeServer
	manifest *pluginv1.PluginManifest

	mu       sync.RWMutex
	client   *mdblist.Client // nil until Configure provides an API key
	gapFill  bool
}

type metadataServer struct {
	pluginv1.UnimplementedMetadataProviderServer
	runtime *runtimeServer

	cacheMu sync.Mutex
	cache   map[string]cacheEntry
}

type cacheEntry struct {
	info      *mdblist.MediaInfo // nil = negative (not found) result
	expiresAt time.Time
}

func (s *runtimeServer) GetManifest(_ context.Context, _ *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

// Configure (re)builds the MDBList client and options from admin config. It is
// safe to call repeatedly; an empty api_key leaves any existing client intact.
func (s *runtimeServer) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	for _, entry := range req.GetConfig() {
		if entry.GetKey() != "mdblist" {
			continue
		}
		fields := entry.GetValue().GetFields()

		apiKey := strings.TrimSpace(fields["api_key"].GetStringValue())
		gapFill := true
		if v, ok := fields["gap_fill"]; ok {
			gapFill = v.GetBoolValue()
		}
		rps := fields["requests_per_second"].GetNumberValue()

		s.mu.Lock()
		s.gapFill = gapFill
		if apiKey != "" {
			s.client = mdblist.NewClient(apiKey, mdblist.WithRequestsPerSecond(rps))
		} else if s.client != nil {
			// Key unchanged but throttle may have: rebuild only the throttle is
			// not possible without the key, so leave the existing client as-is.
		}
		s.mu.Unlock()
	}
	return &pluginv1.ConfigureResponse{}, nil
}

// Search returns empty: MDBList lookups require a known provider ID, and Silo
// always has one (tmdb/imdb/tvdb) by the time this plugin is consulted.
func (s *metadataServer) Search(_ context.Context, _ *pluginv1.SearchMetadataRequest) (*pluginv1.SearchMetadataResponse, error) {
	return &pluginv1.SearchMetadataResponse{}, nil
}

// GetSeasons / GetEpisodes are not provided — MDBList's per-title endpoint has
// no season/episode breakdown, and TVDB/TMDB own that data.
func (s *metadataServer) GetSeasons(_ context.Context, _ *pluginv1.GetSeasonsRequest) (*pluginv1.GetSeasonsResponse, error) {
	return &pluginv1.GetSeasonsResponse{}, nil
}

func (s *metadataServer) GetEpisodes(_ context.Context, _ *pluginv1.GetEpisodesRequest) (*pluginv1.GetEpisodesResponse, error) {
	return &pluginv1.GetEpisodesResponse{}, nil
}

func (s *metadataServer) GetMetadata(ctx context.Context, req *pluginv1.GetMetadataRequest) (*pluginv1.GetMetadataResponse, error) {
	s.runtime.mu.RLock()
	client := s.runtime.client
	gapFill := s.runtime.gapFill
	s.runtime.mu.RUnlock()

	if client == nil {
		return &pluginv1.GetMetadataResponse{}, nil
	}

	ids := stringMapFromStruct(req.GetProviderIds())
	provider, id := selectLookup(ids, req.GetProviderId())
	if provider == "" || id == "" {
		return &pluginv1.GetMetadataResponse{}, nil
	}

	mediaType := mediaTypeFor(req.GetItemType())

	info, err := s.lookup(ctx, client, provider, mediaType, id, ids)
	if err != nil {
		// Transient (rate limit / network). Surface as Unavailable so it shows
		// in silo's provider-error log; the item's ratings fill on a later
		// refresh once the quota resets.
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if info == nil {
		return &pluginv1.GetMetadataResponse{}, nil
	}

	item := buildMetadataItem(info, req.GetItemType(), gapFill)
	if item == nil {
		return &pluginv1.GetMetadataResponse{}, nil
	}
	return &pluginv1.GetMetadataResponse{Item: item}, nil
}

// lookup wraps the client with the short-lived cache so the imdb and tmdb
// capabilities don't double-bill the same title within one refresh.
func (s *metadataServer) lookup(ctx context.Context, client *mdblist.Client, provider, mediaType, id string, ids map[string]string) (*mdblist.MediaInfo, error) {
	key := cacheKey(provider, mediaType, id)
	if entry, ok := s.cacheGet(key); ok {
		return entry, nil
	}

	info, err := client.GetMedia(ctx, provider, mediaType, id)
	if err != nil {
		return nil, err
	}

	// Cache under the requested key plus every ID the response carries, so the
	// sibling capability (and future items sharing an ID) hit the cache.
	s.cachePut(key, info)
	if info != nil {
		for p, v := range responseIDs(info) {
			s.cachePut(cacheKey(p, mediaType, v), info)
		}
	}
	return info, nil
}

func (s *metadataServer) cacheGet(key string) (*mdblist.MediaInfo, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entry, ok := s.cache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		if ok {
			delete(s.cache, key)
		}
		return nil, false
	}
	return entry.info, true
}

func (s *metadataServer) cachePut(key string, info *mdblist.MediaInfo) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache == nil {
		s.cache = make(map[string]cacheEntry)
	}
	s.cache[key] = cacheEntry{info: info, expiresAt: time.Now().Add(cacheTTL)}
}

func main() {
	manifest, err := loadManifest()
	if err != nil {
		panic(err)
	}

	rs := &runtimeServer{manifest: manifest, gapFill: true}

	runtime.Serve(runtime.ServeConfig{
		Servers: runtime.CapabilityServers{
			Runtime:          rs,
			MetadataProvider: &metadataServer{runtime: rs},
		},
	})
}

func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}
	if version != "" {
		manifest.Version = version
	}
	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryData, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, fmt.Errorf("read executable %q: %w", executablePath, err)
	}
	checksum := sha256.Sum256(binaryData)
	manifest.Checksum = hex.EncodeToString(checksum[:])
	return manifest, nil
}
