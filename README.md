# MusicStreamer

Ultra-lightweight Subsonic/OpenSubsonic music server (Go 1.22+, stdlib-only,
<30 MB RAM) that keeps a local disk cache of everything it plays, filling it
on demand from Soulseek (slskd) with an instant YouTube preview while it waits.

It **presents itself as Navidrome** and implements the client-facing API
surface, so players built for Navidrome (Feishin, Symfonium, substreamer,
DSub, play:Sub, ...) get full functionality: persistent playlists,
favourites, ratings, scrobbling and now-playing, play-queue sync, browsing and
search — while tracks still resolve on demand through the cache/preview/
Soulseek pipeline.

## Resolution pipeline (`stream.view`)

Fresh load (no `Range`, or `bytes=0-`), tried in order:

1. **Full disk cache** (`CACHE_DIR`, LRU-capped at `CACHE_SIZE_GB`) — the
   lossless/canonical file, served instantly with `Range`/`206` seeking.
2. **Preview cache / YouTube** — a small lossy m4a (yt-dlp `140`) streams
   immediately; the canonical file is fetched in the background. Previews live
   in a separate store (`CACHE_DIR/preview`, `PREVIEW_SIZE_GB`, LRU-evicted)
   and never appear in the browsable library.
3. **Soulseek (blocking)** — last resort when no preview can be resolved.

**Background fetch** (one per track): **Soulseek** lossless first, then
**YouTube best audio** as the canonical file.

**Pre-resolution while browsing:** when a client lists songs (`search3`,
`getAlbum`, `getRandomSongs`, `getSong`), the server resolves YouTube URLs for
the first few tracks in the background. A play then starts immediately without
paying yt-dlp's lookup latency in the stream request — important because some
players abort a stream that is slow to send its first byte, then retry. Up to
`prewarmPerPage` (6) tracks are warmed per list, two lookups at a time; failures
back off for 10 minutes so list refreshes don't hammer yt-dlp. Resolved URLs
are memoized for 10 minutes and shared by racing/retried requests.

### Upgrading a track to lossless

An in-flight stream can't be swapped mid-playback (HTTP range offsets are tied
to the file), so the upgrade lands on the **next fresh load** of the track:
restart it, skip away and back, or reopen the app. To keep seeks valid, a
connection is **pinned to the variant the track started on** for mid-session
`Range` requests; only a fresh load picks up the lossless file. Pausing and
pressing play resumes from the client's buffer and does **not** re-contact the
server, so it keeps the preview until the track is re-opened.

Song IDs are reversible `base64url(artist\x1falbum\x1ftitle)` — stateless, no DB.

> **yt-dlp speed and JS runtimes:** the server defaults to YouTube's
> `android_sdkless` player client, which needs **no JavaScript runtime** and
> resolves roughly twice as fast as the web client's JS-challenge path
> (`--extractor-args youtube:player_client=android_sdkless`, with an automatic
> retry on the normal JS path if it fails). `nodejs` is still installed in the
> image so that fallback works. YouTube also changes often and may challenge the
> server's IP: pass extra flags with `YTDLP_ARGS`, mount cookies via
> `YTDLP_COOKIES`, or point `YTDLP_PATH` at a newer binary. Diagnosis:
> `docker compose logs musicstreamer | grep youtube` — resolve failures include
> yt-dlp's own stderr. Without a working `yt-dlp`, playback falls back to
> **blocking Soulseek**.
>
> The first YouTube lookup after boot pays a one-time cost (loading the JS
> runtime and challenge solver); a **startup warm-up** (`YOUTUBE_WARMUP`, on by
> default) hides it. If a client gives up mid-lookup, the resolve keeps going
> and its preview is downloaded in the background, so the retry is instant.

## Quickstart

```bash
cp .env.example .env   # set SUBSONIC_USER/PASS
docker compose up --build -d
```

This starts MusicStreamer (`:4533`) plus slskd (`:5030`). Then:

1. Open slskd at `http://<host>:5030` (login `slskd`/`slskd`, change it),
   enter your Soulseek login under Settings. Then set the API key **in
   `docker-compose.yml`** (slskd service → uncomment `SLSKD_API_KEY`,
   16+ chars), copy the same value to `SLSKD_API_KEY` in `.env`, and
   `docker compose up -d` again. Key-in-compose survives recreates;
   hand-edits to slskd.yml inside the container do not (and
    `SLSKD_REMOTE_CONFIGURATION` is not needed for API use). Without a
    Soulseek login nothing can be fetched — only already-cached tracks play.
2. Point Symfonium/Feishin (or any Navidrome client) at `http://<host>:4533`,
    user/pass from env. `?f=json` is honored automatically.

## Navidrome compatibility

The server identifies as `navidrome` and implements the endpoints Navidrome
clients rely on. There is no separate music folder and no catalog database:
browsing reflects the disk cache, and any track you press play on still
resolves through the on-demand pipeline.

Persistent state is a small JSON file (`STATE_DIR/library.json`):

- **Playlists**: `getPlaylists`, `getPlaylist`, `createPlaylist`,
  `updatePlaylist`, `deletePlaylist`
- **Favourites**: `star`, `unstar`, `getStarred`, `getStarred2`
- **Ratings**: `setRating` (also returned as `userRating` on cached entries)
- **Playback**: `scrobble` (now-playing + completed plays), `getNowPlaying`,
  `getPlayQueue`, `savePlayQueue`
- **Scanner**: `getScanStatus`, `getScanStatus2`, `startScan` (reports the
  cache size; there is nothing to rescan)
- **Info**: `getArtistInfo`, `getArtistInfo2`, `getAlbumInfo`, `getAlbumInfo2`
  (auth-bearing cover-art URLs; biography/notes are empty)
- **Lyrics**: `getLyrics`, `getLyricsBySongId` (empty; no lyric source)

Empty-but-valid (no backing store): genres, songs-by-genre, top/similar songs,
shares, bookmarks, internet radio, videos, podcasts, chat.

Playlists and favourites store reversible song IDs, so a track evicted by the
cache LRU still appears in a playlist and re-resolves on demand when played.

Identity and storage are configurable:

| Env | Default | Notes |
|---|---|---|
| `SERVER_TYPE` | `navidrome` | The `type` reported to clients |
| `SERVER_VERSION` | `0.55.0` | The `serverVersion` reported to clients |
| `STATE_DIR` | `<CACHE_DIR>/state` | Playlists/favourites/etc. (JSON) |

## Endpoints

| Route | Notes |
|---|---|
| `/rest/ping.view` | auth check |
| `/rest/getLicense.view` | static valid license |
| `/rest/search3.view?query=&songCount=&songOffset=` | query empty → flat list of cached tracks (paginated); query set → cached matches first, then Deezer → MusicBrainz |
| `/rest/getRandomSongs.view?size=` | flat list of every cached track (one entry per file) |
| `/rest/getSong.view?id=` | decode ID + best-effort enrich |
| `/rest/stream.view?id=` | cache → YouTube preview → background fetch (see pipeline) |
| `/rest/getUser.view` | login probe (full stream/playlist roles) |
| `/rest/getAlbum.view?id=` | cached tracks for the album if present, else metadata-search track list; every song streamable |
| `/rest/getCoverArt.view?id=` | proxies upstream artwork (any song/album/artist ID) |
| `/rest/getAlbumList2.view?type=&size=&offset=` | albums present in the disk cache (`starred`/`random`/alphabetical ordering) |
| `/rest/getArtist.view?id=` | cached albums for the artist, else grouped from metadata search |
| `/rest/getMusicDirectory.view?id=` | simulated artist → album → track tree |
| `/rest/download.view?id=` | cached file as an attachment, else streams via the pipeline |
| `/rest/search2.view` | same results as `search3` (pre-ID3 envelope) |
| `/rest/getPlaylists`, `/rest/getPlaylist`, `/rest/createPlaylist`, `/rest/updatePlaylist`, `/rest/deletePlaylist` | persistent playlists (`STATE_DIR`) |
| `/rest/star`, `/rest/unstar`, `/rest/getStarred`, `/rest/getStarred2` | persistent favourites |
| `/rest/setRating` | persistent ratings |
| `/rest/scrobble`, `/rest/getNowPlaying` | now-playing / completed-play tracking |
| `/rest/getPlayQueue`, `/rest/savePlayQueue` | client play-queue sync |
| `/rest/getScanStatus`, `/rest/getScanStatus2`, `/rest/startScan` | cache-backed scan status |
| `/rest/getArtistInfo(2)`, `/rest/getAlbumInfo(2)`, `/rest/getLyrics`, `/rest/getLyricsBySongId` | info/lyrics (valid, mostly empty) |
| `/rest/jukeboxControl.view` | stub: empty stopped queue |
| `/rest/getMusicFolders`, `/rest/getOpenSubsonicExtensions` | compat |
| `/rest/getIndexes`, `/rest/getArtists` | cached artists grouped by initial |
| `/rest/getAlbumList` | same as `getAlbumList2` (cache-backed) |
| `/rest/getGenres`, `/rest/getSongsByGenre`, `/rest/getTopSongs`, `/rest/getSimilarSongs(2)` | empty-but-valid |
| `/rest/getShares`, `/rest/getBookmarks`, `/rest/getInternetRadioStations`, `/rest/getVideos`, `/rest/getPodcasts`, `/rest/getChatMessages` | empty-but-valid |
| `/healthz` | container healthcheck |

Auth: `t`/`s` token, `p` plaintext, `p=enc:<hex>` all accepted.

## Library view

There is no catalog database. The disk cache **is** the library: every cached
file is named with its reversible song ID, so the tracks are reconstructed from
the cache keys and always reflect what is downloaded right now. The only
persistent store is the small `STATE_DIR/library.json` file holding user state
(playlists, favourites, ratings, play counts, play queue).

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
- `pkg/library/` — JSON-file store for playlists, stars, ratings, plays, queue
- `pkg/metadata/` — Deezer/MusicBrainz → canonical `Song`
- `pkg/soulseek/` — slskd client (search, queue, presence-based completion, fetch)
- `pkg/subsonic/` — auth, XML/JSON responses, handlers, router, cache catalog,
  Navidrome-compatible endpoints (`compat.go`)
- `pkg/ytaudio/` — yt-dlp resolver (direct audio URL for instant playback)

