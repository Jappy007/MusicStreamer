# DebridSubsonic

Ultra-lightweight Subsonic-compatible music server (Go 1.22+, stdlib-only,
<30 MB RAM) fronting AllDebrid as a cloud music locker.

Native clients (Symfonium, Feishin) connect as if it were Navidrome/Airsonic.

## Resolution pipeline (`stream.view`, first hit wins)

0. **Local library** (`MUSIC_DIR`, e.g. Lidarr's output) — files Lidarr
   already downloaded play instantly, no cloud involved.
1. **Local disk cache** (`CACHE_DIR`, LRU-capped at `CACHE_SIZE_GB`) — served
   with full `Range` / `206 Partial Content` seeking.
2. **AllDebrid cloud** — `POST /v4.1/magnet/status` fuzzy-matches the album
   container, one batched `POST /v4/magnet/files` call resolves file trees,
   `POST /v4/link/unlock`, proxies bytes while Tee-spilling to cache.
   (Bearer auth; the legacy `GET /v4/magnet/status` is discontinued upstream.)
3. **Soulseek via slskd** when configured (music-native, often fastest for
   audio) — search, download, serve from the shared download dir.
4. **Debrid scrape, last resort** — Prowlarr, then the built-in public DHT
   index, each fully resolved (search → upload → Ready) before the next
   is tried, so one dead source never blocks the others.
   `magnet:` URIs go to `POST /v4/magnet/upload`; index `.torrent` links are
   fetched locally (they're LAN-only/key-gated) and pushed to
   `POST /v4/magnet/upload/file`. Then poll to `Ready`, unlock, proxy + cache.
   Album releases are preferred throughout: track IDs naming a single release
   are resolved to every plausible album first (ID album unless single-like,
   plus distinct lookup albums), each tried cleaned + FLAC; candidates named
   like the album outrank single-track rips (one upload then serves every
   track via the cloud tier).
   Candidates are tried seeder-desc (Prowlarr rows below `MIN_SEEDERS` are
   skipped when set); a torrent AllDebrid reports with zero activity aborts
   early (~20s with no seeders, ~48s otherwise) so a dead swarm can't burn
   the full wait budget before the next candidate is tried.
   (AllDebrid has no search API — it can only download magnets an index found.)

Song IDs are reversible `base64url(artist\x1falbum\x1ftitle)` — stateless, no DB.

## Quickstart

```bash
cp .env.example .env   # fill ALLDEBRID_API_KEY, SUBSONIC_USER/PASS
docker compose up --build -d
```

This starts DebridSubsonic (`:4533`) plus slskd (`:5030`). Then:

1. Open slskd at `http://<host>:5030` (login `slskd`/`slskd`, change it),
   enter your Soulseek login under Settings. Then set the API key **in
   `docker-compose.yml`** (slskd service → uncomment `SLSKD_API_KEY`,
   16+ chars), copy the same value to `SLSKD_API_KEY` in `.env`, and
   `docker compose up -d` again. Key-in-compose survives recreates;
   hand-edits to slskd.yml inside the container do not (and
   `SLSKD_REMOTE_CONFIGURATION` is not needed for API use). No Soulseek
   login = Soulseek tier stays idle; everything else works.
2. Point Symfonium/Feishin at `http://<host>:4533`, user/pass from env,
   `?f=json` is honored automatically.
3. Optional: uncomment the `prowlarr`/`lidarr` services in
   `docker-compose.yml` if you want them in this stack, or keep your
   existing instances and just set `PROWLARR_URL`/`PROWLARR_API_KEY`.

## Endpoints

| Route | Notes |
|---|---|
| `/rest/ping.view` | auth check |
| `/rest/getLicense.view` | static valid license |
| `/rest/search3.view?query=&songCount=` | Deezer → MusicBrainz fallback |
| `/rest/getSong.view?id=` | decode ID + best-effort enrich |
| `/rest/stream.view?id=` | 3-tier pipeline |
| `/rest/getUser.view` | login probe (full stream/playlist roles) |
| `/rest/getAlbum.view?id=` | cached tracks for the album if present, else metadata-search track list; every song streamable |
| `/rest/getCoverArt.view?id=` | proxies upstream artwork (any song/album/artist ID) |
| `/rest/getAlbumList2.view` | albums present in the disk cache |
| `/rest/getArtist.view?id=` | cached albums for the artist, else grouped from metadata search |
| `/rest/jukeboxControl.view` | stub: empty stopped queue |
| `/rest/scrobble.view` | accepted and ignored (no history stored) |
| `/rest/getMusicFolders`, `/rest/getOpenSubsonicExtensions` | compat stubs |
| `/rest/getIndexes`, `/rest/getArtists` | artists present in the disk cache |
| `/rest/getAlbumList` | same as `getAlbumList2` (cache-backed) |
| `/rest/getPlaylists`, `/rest/getGenres`, `/rest/getStarred`, `/rest/getStarred2` | empty-library stubs (no persistent library; tracks resolve via search+debrid) |
| `/healthz` | container healthcheck |

Auth: `t`/`s` token, `p` plaintext, `p=enc:<hex>` all accepted.

## Library view

There is no database. The disk cache **is** the library: every cached file is
named with its reversible song ID, so `getArtists`/`getIndexes`,
`getAlbumList`/`getAlbumList2`, `getArtist`, `getAlbum`, `getSong` and
`search3` reconstruct artist → album → track from the cache keys and always
reflect what is downloaded right now (cached tracks surface first and play
instantly). Entries evicted by the LRU simply disappear from the browse tree;
tracks not yet cached still resolve on demand through the tier pipeline.

## Layout

- `main.go` — env config, cache + cleaner, HTTP server, graceful shutdown
- `pkg/cache/` — RWMutex-guarded LRU, atomic tmp→rename writes, background purge
- `pkg/debrid/` — AllDebrid v4 client (context timeouts) + Prowlarr search
- `pkg/metadata/` — Deezer/MusicBrainz → canonical `Song`
- `pkg/subsonic/` — auth, XML/JSON responses, handlers, router

