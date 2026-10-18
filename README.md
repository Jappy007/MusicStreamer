# DebridSubsonic

Ultra-lightweight Subsonic-compatible music server (Go 1.22+, stdlib-only,
<30 MB RAM) that keeps a local disk cache of everything it plays and fills it
on demand from Soulseek (slskd).

Native clients (Symfonium, Feishin) connect as if it were Navidrome/Airsonic.

## Resolution pipeline (`stream.view`, first hit wins)

1. **Local disk cache** (`CACHE_DIR`, LRU-capped at `CACHE_SIZE_GB`) — served
   with full `Range` / `206 Partial Content` seeking.
2. **Soulseek via slskd** — search, download, serve the completed file the
   instant it lands and promote it into the cache in the background.

That's the whole pipeline: cache → Soulseek. No AllDebrid account, no external
indexers, no scrape tier. A track that is already cached is instant; a track
that is not is fetched from Soulseek and cached for next time. Concurrent
requests for the same track share one fetch (the others wait on the cache).

Song IDs are reversible `base64url(artist\x1falbum\x1ftitle)` — stateless, no DB.

## Quickstart

```bash
cp .env.example .env   # set SUBSONIC_USER/PASS
docker compose up --build -d
```

This starts DebridSubsonic (`:4533`) plus slskd (`:5030`). Then:

1. Open slskd at `http://<host>:5030` (login `slskd`/`slskd`, change it),
   enter your Soulseek login under Settings. Then set the API key **in
   `docker-compose.yml`** (slskd service → uncomment `SLSKD_API_KEY`,
   16+ chars), copy the same value to `SLSKD_API_KEY` in `.env`, and
   `docker compose up -d` again. Key-in-compose survives recreates;
   hand-edits to slskd.yml inside the container do not (and
    `SLSKD_REMOTE_CONFIGURATION` is not needed for API use). Without a
    Soulseek login nothing can be fetched — only already-cached tracks play.
2. Point Symfonium/Feishin at `http://<host>:4533`, user/pass from env,
    `?f=json` is honored automatically.

## Endpoints

| Route | Notes |
|---|---|
| `/rest/ping.view` | auth check |
| `/rest/getLicense.view` | static valid license |
| `/rest/search3.view?query=&songCount=&songOffset=` | query empty → flat list of cached tracks (paginated); query set → cached matches first, then Deezer → MusicBrainz |
| `/rest/getRandomSongs.view?size=` | flat list of every cached track (one entry per file) |
| `/rest/getSong.view?id=` | decode ID + best-effort enrich |
| `/rest/stream.view?id=` | cache → Soulseek pipeline |
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
| `/rest/getPlaylists`, `/rest/getGenres`, `/rest/getStarred`, `/rest/getStarred2` | empty-library stubs (no persistent library; tracks resolve via cache + Soulseek) |
| `/healthz` | container healthcheck |

Auth: `t`/`s` token, `p` plaintext, `p=enc:<hex>` all accepted.

## Library view

There is no database. The disk cache **is** the library: every cached file is
named with its reversible song ID, so the tracks are reconstructed from the
cache keys and always reflect what is downloaded right now.

Tracks are exposed **one per cached file** (a flat, track-by-track list), which
is what clients paginate for a "Songs" view: `search3` with an empty query
(`songCount`/`songOffset`) and `getRandomSongs` return every cached track as an
individual song entry. `getArtists`/`getIndexes`, `getAlbumList`/`getAlbumList2`,
`getArtist`, `getAlbum`, `getSong` additionally group the same tracks as
artist → album → track for Albums/Artists browsing. Entries evicted by the LRU
simply disappear; tracks not yet cached still resolve on demand through the
tier pipeline.

Cached tracks also report their real **file type**, **sample rate** and
**bit depth** (and channels/bitrate where known): the bytes are sniffed by
`pkg/audioinfo` (FLAC, MP3, WAV, AIFF, Ogg Vorbis, Opus, MP4/M4A AAC & ALAC,
WMA), exposed through the OpenSubsonic `suffix`, `contentType`,
`samplingRate`, `bitDepth` and `channelCount` song attributes, plus `size`.
Results are memoized per cache entry (keyed by id + size), so browsing stays
cheap — a file is read at most once.

## Layout

- `main.go` — env config, cache + cleaner, HTTP server, graceful shutdown
- `pkg/audioinfo/` — header-only audio format / sample-rate / bit-depth sniffing
- `pkg/cache/` — RWMutex-guarded LRU, atomic tmp→rename writes, background purge
- `pkg/metadata/` — Deezer/MusicBrainz → canonical `Song`
- `pkg/soulseek/` — slskd client (search, queue, presence-based completion, fetch)
- `pkg/subsonic/` — auth, XML/JSON responses, handlers, router, cache catalog

