# ReSpool

ReSpool serves a folder of audiobooks and other audio files as podcast feeds,
so you can listen to them in any podcast app that lets you add a feed by URL.

Point it at a folder. Every book in it gets its own feed. Open the web page,
copy a feed URL and paste it into your podcast app.

The name is RSS + Spool: it was made for following local audiobooks in the
Spool podcast app, and it works with any app that can add a feed by URL.

- Zero configuration and no database. Your folder layout and file tags are
  the source of truth.
- Reads ID3 and MP4 tags, embedded cover art and m4b chapters (Nero and
  QuickTime), and works out durations without ffmpeg.
- Serves byte ranges, so players can stream and seek.
- Optionally, add audio from the web page: uploads are checked, filed into a
  book folder of their own and show up as a feed right away.
- One small static binary in a distroless image for amd64, arm64 and armv7
  (Synology, Unraid, Raspberry Pi…).

## Quick start

```sh
docker run -d --name respool -p 8080:8080 \
  -v /path/to/audiobooks:/media:ro \
  -v respool-uploads:/uploads -e RESPOOL_UPLOAD_DIR=/uploads \
  -e RESPOOL_TOKEN="$(openssl rand -hex 16)" \
  ghcr.io/jeffchimney/respool:latest

docker logs respool   # shows the index URL, including your token
```

Open `http://<server-ip>:8080/<token>/` and copy a feed into your podcast app.
There's also a [docker-compose.yml](docker-compose.yml). Leave out the
uploads lines if you only want to serve the media folder.

## How folders become feeds

```text
/media
├── Andy Weir/
│   └── Project Hail Mary/        → one feed, one episode with chapters
│       ├── Project Hail Mary.m4b
│       └── cover.jpg
├── The Hobbit/                   → one feed, episodes in order
│   ├── CD1/01 An Unexpected Party.mp3
│   ├── CD1/02 Roast Mutton.mp3
│   ├── CD2/01 A Short Rest.mp3
│   └── description.txt
└── Dune.m4b                      → loose files in the root get a feed each
```

- **A folder containing audio files is a feed.** Nested folders are fine;
  each folder that directly holds audio is its own feed.
- **Disc folders** named like `CD1`, `Disc 2` or `disk_03` are merged into
  their parent book.
- **Episode order:** track and disc tags when every file has a distinct track
  number, otherwise natural filename order (`Part 2` before `Part 10`). Feeds
  are marked as serial and episodes are numbered, so apps play them in order.
- **Titles:** the album tag (or the folder name) for the feed, the title tag
  (or the filename) for each episode. If every file has the same title tag,
  filenames are used instead.
- **Overrides:** a `respool.json` in a book's folder beats the tags, e.g.
  `{"title": "Dune", "author": "Frank Herbert", "description": "…"}`. Any
  field can be left out. The feed URL doesn't change.
- **Cover art:** `cover`, `folder`, `front` or `poster` `.jpg`/`.png` in the
  book's folder, then any single image there, then art embedded in the audio,
  then a generated placeholder. A loose `Dune.m4b` uses `Dune.jpg`.
- **Description:** `description.txt` or `desc.txt` in the folder (`Dune.txt`
  for a loose file), then the file's description or comment tag.
- **Chapters** in m4b/m4a files are published as
  [Podcasting 2.0 chapters](https://github.com/Podcastindex-org/podcast-namespace/blob/main/chapters/jsonChapters.md);
  most apps also read them straight from the file.
- Supported formats: mp3, m4a, m4b, aac, flac, ogg, opus and wav. Hidden
  files and NAS folders such as `@eaDir` and `#recycle` are ignored.

Feed URLs are derived from each book's path, so they survive restarts and
rescans. Renaming or moving a book's folder gives it a new feed URL.

## Adding audio from the web page

Set `RESPOOL_UPLOAD_DIR` to a writable folder and the page gets an **Add
audio** button (you can also drop files anywhere on the page). Your media
folder stays read-only: uploads live in their own folder, which ReSpool scans
alongside it.

- **One upload is one book.** Give it a title and author, or leave them blank
  to take them from the files' tags (album and artist). Uploads are filed as
  `Author/Title/` inside the uploads folder, and anything you type is saved
  in the book's `respool.json`, so it wins over the tags.
- **Add to an existing book** by picking it in the dialog, or with **Add
  audio** in the book's edit dialog, for example to add the next part. Files
  with names that are already taken get a number.
- **Include a JPEG or PNG** to use it as the book's cover.
- **Edit uploaded books** with the **⋯** button on their card: change the
  title, author, description and cover, remove single files, or delete the
  whole book. Renaming only changes what podcast apps show; the folder stays
  put, so the feed URL and existing subscriptions keep working. Deleting is
  permanent and asks first.
- Books in your media folder can't be changed from the page, since ReSpool
  never writes there. Use a `respool.json` (see above) to fix their details.
- Every file is checked against its extension's format before anything is
  saved, so a mislabelled or partial upload is rejected as a whole.
- Supported formats are the same as above. ReSpool doesn't convert audio, so
  upload a format your podcast app can play.

Uploads arrive in a hidden `.incoming` folder and are only moved into place
once complete, so a scan never picks up half a file.

## Configuration

Every setting is an environment variable (or the matching flag, see
`respool -help`).

| Variable | Default | |
| --- | --- | --- |
| `RESPOOL_MEDIA_DIR` | `/media` | Folder to serve. |
| `RESPOOL_ADDR` | `:8080` | Address to listen on. |
| `RESPOOL_TOKEN` | *(none)* | Secret that must prefix every URL: `http://host:8080/<token>/…`. Letters, digits and `._~-`. Strongly recommended. |
| `RESPOOL_BASE_URL` | *(from request)* | Public URL to put in feeds, e.g. `https://respool.example.ts.net`. By default ReSpool uses the address each client connected to, and honours `X-Forwarded-Proto`/`X-Forwarded-Host`. |
| `RESPOOL_SCAN_INTERVAL` | `5m` | How often to look for new files. `0` disables periodic scans; the Rescan button still works. |
| `RESPOOL_LANGUAGE` | `en` | Language code written into feeds. |
| `RESPOOL_LOG_LEVEL` | `info` | `debug` logs every request, including range requests. |
| `RESPOOL_UPLOAD_DIR` | *(none)* | Writable folder for audio added from the web page. Unset, uploads are off. In Docker, use `/uploads` and mount a volume there. |
| `RESPOOL_UPLOAD_LIMIT_MB` | `8192` | Largest upload accepted, in MiB. |

## Listening away from home

Podcast apps that refresh feeds on their own servers can't reach a server on
your home network, but apps that fetch feeds on the device will find new
episodes whenever you're on the same network as ReSpool. To listen from
anywhere, put ReSpool behind something that gives it a reachable address:

- **[Tailscale](https://tailscale.com):** install it on the server and your
  phone, then use `http://<server>.<tailnet>.ts.net:8080/<token>/`, or
  `tailscale serve` for HTTPS.
- **Cloudflare Tunnel** or a reverse proxy such as Caddy, with
  `RESPOOL_BASE_URL` set to the public URL.

Some iOS apps refuse plain `http://` URLs. HTTPS through Tailscale or a
reverse proxy avoids that.

## Security

ReSpool is meant for your own library on your own network. Without
`RESPOOL_TOKEN`, anyone who can reach the port can list and download every
file, and upload files if uploads are on. With a token, URLs are
unguessable, but anyone you share a feed URL with has the token, so they can
download the whole library and upload to it. The media folder can, and
should, be mounted read-only. Feeds carry `itunes:block` and `podcast:locked`
so directories and podcast hosts won't list or import them.

Uploads only accept the audio and image formats listed above, checked by
content as well as extension, and must come from ReSpool's own page: requests
without its `X-Requested-With: ReSpool` header are refused, so other websites
can't upload through your browser.

## Development

```sh
go test ./...
go run ./cmd/respool -media ~/Audiobooks -log-level debug
docker build -t respool .
```

The audio fixtures in `internal/media/testdata` are a few kilobytes each; see
the README there for the ffmpeg commands that generate them.

## License

[MIT](LICENSE)
