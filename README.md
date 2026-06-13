# silo-plugin-mdblist

A [Silo](https://siloserver.org) metadata-provider plugin that enriches media
items with **ratings** and a few **gap-fill** fields from the
[MDBList API](https://api.mdblist.com). It is the successor to `silo-plugin-omdb`
and is intended to be the **only** ratings provider Silo runs.

- **Plugin ID:** `silo.mdblist`
- **Capabilities:** two `metadata_provider.v1` capabilities — `imdb` and `tmdb`
- **API:** `https://api.mdblist.com`, authenticated with an `apikey` query parameter
- **Data flow:** pull-based — Silo calls `GetMetadata`; the plugin looks the title
  up on MDBList by an existing provider ID and returns ratings (+ optional gap-fill)

---

## Why MDBList (vs OMDb)

OMDb gave us the IMDb rating and the Rotten Tomatoes **critic** score. MDBList
returns those **plus the Rotten Tomatoes audience score**, from one richer API,
on a 100k/day Plus key. Everything the Silo database can actually store for
ratings now comes from a single source.

MDBList returns far more (Metacritic, Trakt, Letterboxd, Roger Ebert, MyAnimeList,
budget, revenue, streaming availability, awards, parental ratings, …) — but the
Silo schema has **no column** for any of it, so this plugin deliberately ignores
those fields. See *What Silo can store* below.

---

## What Silo can store (the contract)

Silo ingests a plugin's `MetadataItem` in
`internal/metadata/plugin_provider.go`. Two facts drive this plugin's entire
design:

1. **Ratings** are read from a `google.protobuf.Struct` and only **four float
   keys** are recognised:

   | Struct key     | Silo DB column      | Scale | MDBList source |
   |----------------|---------------------|-------|----------------|
   | `imdb`         | `rating_imdb`       | 0–10  | `imdb`         |
   | `tmdb`         | `rating_tmdb`       | 0–10  | *(skipped — TMDB owns it; MDBList reports it 0–100)* |
   | `rt_critic`    | `rating_rt_critic`  | 0–100 | `tomatoes`     |
   | `rt_audience`  | `rating_rt_audience`| 0–100 | `popcorn`      |

   Any other key (metacritic, trakt, …) is silently dropped by Silo.

2. From the generic `metadata` struct, Silo reads **only** `keywords`. MDBList's
   title endpoint provides no keywords, so we emit none — nothing else from a
   generic metadata blob would be persisted anyway.

The remaining `MetadataItem` scalar/array fields *are* persisted (title,
overview, genres, runtime, content_rating, countries, language, dates, images,
people, provider IDs). This plugin populates only the **gap-fill subset** below,
and never title/overview/genres/images/people, which TMDB and TVDB own.

---

## Non-destructive by construction

Silo merges providers with `MergeFillEmpty` (`internal/metadata/merge.go`): the
**first non-empty value wins**, and later providers only fill blanks. Provider
IDs always **accumulate**. Consequences:

- TMDB/TVDB don't supply IMDb or Rotten Tomatoes *ratings*, so MDBList fills
  `rating_imdb`, `rating_rt_critic`, `rating_rt_audience` — pure gain.
- Gap-fill scalars (content rating, language, country, year, release/first-air
  date, movie runtime) only land where TMDB/TVDB left a blank. They can never
  overwrite richer data, regardless of provider order.
- Cross-provider IDs (`tvdb`, `trakt`, `mal`, `mdblist`, …) are backfilled onto
  the item — also pure gain.

Gap-fill can be turned off entirely (ratings-only) with the **Gap-fill** switch
in config.

---

## Two capabilities, one request

The plugin declares two `metadata_provider.v1` capabilities, `imdb` and `tmdb`.
A Silo metadata provider is only consulted for an item that already carries a
provider ID **matching the capability ID**. Declaring both means the plugin
fires for any item with *either* an IMDb or a TMDB ID — i.e. essentially the
whole library.

Because both capabilities can fire for the same title in a single refresh, a
short-lived (5 min) in-process cache keyed by every ID in the response collapses
the two back-to-back lookups into **one** MDBList request. This halves quota
spend during a full library refresh.

`GetMetadata` selects which ID to look up by, in preference order:
`imdb → tmdb → tvdb → trakt → mal`. Silo's item type `movie`/`series` maps to
MDBList's `movie`/`show`.

---

## Rate limiting & graceful degradation

The Plus key allows 100k requests/day, but the account will be **downgraded to a
cheaper tier after the initial ingest**, so the client is built to degrade
cleanly:

- **Header-aware.** Reads `X-RateLimit-Remaining` / `X-RateLimit-Reset` on every
  response; honours `Retry-After` on `429`.
- **Cooldown gate.** When remaining hits 0 or a `429` lands, the client records a
  cooldown until the reset time and **short-circuits further calls without
  touching the network** — a depleted quota costs zero wasted requests.
- **Throttle.** An optional `requests_per_second` setting spaces outbound calls
  so a backfill spreads across the daily budget instead of bursting through it.
- **Retriable vs terminal.**
  - Rate-limit / transient → `GetMetadata` returns gRPC `Unavailable`. Silo logs
    it as a provider error and skips it for this pass; the item's ratings fill on
    a later refresh once the quota resets (cheap to re-run thanks to
    `MergeFillEmpty`).
  - `404` / `400` / `422` (missing or malformed ID) → terminal "not found"
    (`nil`), so the item is not retried forever.
  - `401` / `403` / `5xx` → surfaced as an error so it is visible/retried.

> **Note on queuing:** a metadata provider is *pull-based* — it cannot push
> results back to Silo asynchronously. "Queuing" is therefore achieved by (a) the
> throttle + cooldown avoiding wasted calls, and (b) Silo's metadata
> refresh-debt scheduler (and/or a manual re-run) re-refreshing un-filled items
> after the quota resets.

---

## Configuration

Global config key `mdblist`:

| Field                 | Control   | Default | Purpose |
|-----------------------|-----------|---------|---------|
| `api_key`             | password  | —       | MDBList API key from mdblist.com/preferences/#api (required). |
| `gap_fill`            | switch    | `true`  | Also fill content rating, language, country, year, release/first-air date, and (movie) runtime where another provider left them empty. Off = ratings only. |
| `requests_per_second` | number    | `0`     | Throttle outbound calls. `0` = unlimited. Raise the spacing after downgrading to a cheaper tier. |

---

## Project layout

```
main.go              Runtime + MetadataProvider servers, Configure, GetMetadata,
                     the per-title cache, manifest loading.
mapping.go           MDBList MediaInfo -> Silo MetadataItem mapping, ID selection,
                     item-type and ratings mapping.
mdblist/types.go     API response models + rating accessors.
mdblist/client.go    Rate-limit-aware HTTP client (apikey query auth, cooldown,
                     throttle, 1 MiB body cap, 15s timeout).
manifest.json        Two metadata_provider.v1 capabilities + config schema.
*_test.go            Unit tests (client behaviour, mapping, cache, manifest).
Makefile             build / test / lint / build-all (linux amd64+arm64, darwin arm64).
.github/workflows/   CI (test) and Release (tag-driven multi-arch build + manifest checksum).
```

---

## Build & test

```sh
make test        # go test ./...
make build       # local binary
make build-all   # dist/ binaries for all supported platforms
```

Releases are tag-driven (`v*`): the workflow builds each platform, injects the
binary's SHA-256 into `manifest.json` (`__CHECKSUM__`), and publishes a GitHub
release. CI fetches the (private) SDK via `GOPROXY=direct` + `GOPRIVATE`, and
**rejects** any committed machine-local `replace` of the SDK.

---

## Migrating from silo-plugin-omdb

1. Install and configure `silo-plugin-mdblist` (set the API key).
2. Disable/remove the OMDb plugin.
3. Trigger a metadata refresh. MDBList backfills `rating_imdb`,
   `rating_rt_critic`, and the new `rating_rt_audience` across the library
   (existing values are preserved; only blanks are filled unless you force a
   full overwrite refresh).
