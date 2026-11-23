# ReSpool

ReSpool serves a folder of audiobooks, lectures, courses and other long-form
audio as podcast feeds, so you can listen to them in any podcast app that
lets you add a feed by URL.

Point it at a folder. Every show in it, whether that's an audiobook, a
course or a series, gets its own feed. Open the web page, copy a feed URL
and paste it into your podcast app.

The name is RSS + Spool: it was made for following local audio in the Spool
podcast app, and it works with any app that can add a feed by URL.

- Zero configuration and no database. Your folder layout and file tags are
  the source of truth.
- Reads ID3 and MP4 tags, embedded cover art and m4b chapters (Nero and
  QuickTime), and works out durations without ffmpeg.
- Serves byte ranges, so players can stream and seek.
- Optionally, add audio from the web page: uploads are checked, filed into a
  folder of their own and show up as a feed right away.
- One small static binary in a distroless image for amd64, arm64 and armv7
  (Synology, Unraid, Raspberry Pi…).

## Quick start

You need Docker and a folder of audio files.

1. **Make a token**, a secret that every ReSpool URL starts with. Write it
   down and reuse it: if the token changes, every feed you've subscribed to
   stops working.

   ```sh
   openssl rand -hex 16
   ```

2. **Start ReSpool** with your folder and token:

   ```sh
   docker run -d --name respool --restart unless-stopped -p 8080:8080 \
     -v /path/to/audio:/media:ro \
     -v respool-uploads:/uploads -e RESPOOL_UPLOAD_DIR=/uploads \
     -e RESPOOL_TOKEN=your-token \
     ghcr.io/jeffchimney/respool:latest
   ```

   Leave out the `respool-uploads` line if you only want to serve the media
   folder. To require a password for the page, see
   [Signing in](#signing-in).

3. **Open `http://<server-ip>:8080/<your-token>/`**, where `<server-ip>` is
   the address of the machine running Docker. On a computer, the page shows a
   QR code you can scan to open it on your phone.

4. **Subscribe:** copy a show's feed URL and add it to your podcast app. Most
   apps have an option like "Add by URL", or let you paste it into search.

If the page is empty or won't load, `docker logs respool` shows what went
wrong, such as a folder ReSpool can't read (see
[File permissions](#file-permissions)).

### Docker Compose

Download [docker-compose.yml](docker-compose.yml), set your media folder and
token in it, then start it. To require a password for the page, also
uncomment `RESPOOL_LOGIN` (see [Signing in](#signing-in)).

```sh
curl -O https://raw.githubusercontent.com/jeffchimney/ReSpool/main/docker-compose.yml
docker compose up -d
```

### Updating

Pull the new image and recreate the container with the **same token** and
the same volumes. Uploads are kept in the `respool-uploads` volume, and your
account, if you've turned on [signing in](#signing-in), in `respool-data`.

```sh
docker pull ghcr.io/jeffchimney/respool:latest
docker rm -f respool
```

Then run your `docker run` command again: the one from step 2, or the one
from [Signing in](#signing-in). With Compose, run
`docker compose pull && docker compose up -d`.

### File permissions

ReSpool runs as an unprivileged user (uid 65532), not root. It has to be
able to read your media folder and, if they're on, write to the uploads and
data folders.

- **Media folder:** if your audio files aren't readable by everyone, run
  ReSpool as a user who can read them. Add `--user 1000:1000` to
  `docker run` (or `user: "1000:1000"` in Compose), using the user and group
  IDs that `id` prints for that user.
- **Uploads folder:** the `respool-uploads` volume works as is, unless you
  use `--user`. To keep uploads in a folder on the host instead, mount it
  with `-v /path/to/uploads:/uploads` and give the user ReSpool runs as write
  access, for example `sudo chown 65532:65532 /path/to/uploads`. With
  `--user`, chown it to that user instead.
- **Data folder:** the same goes for `respool-data` and `/data`, which holds
  the account when [signing in](#signing-in) is on. If ReSpool can't write
  there, it won't start.

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
├── Intro to Astronomy/           → a course or series works the same way
│   ├── Lecture 01.mp3
│   └── Lecture 02.mp3
└── Dune.m4b                      → loose files in the root get a feed each
```

- **A folder containing audio files is a feed.** Nested folders are fine;
  each folder that directly holds audio is its own feed.
- **Disc folders** named like `CD1`, `Disc 2` or `disk_03` are merged into
  their parent show.
- **Episode order:** track and disc tags when every file has a distinct track
  number, otherwise natural filename order (`Part 2` before `Part 10`). Feeds
  are marked as serial and episodes are numbered, so apps play them in order.
- **Titles:** the album tag (or the folder name) for the feed, the title tag
  (or the filename) for each episode. If every file has the same title tag,
  filenames are used instead.
- **Overrides:** a `respool.json` in a show's folder beats the tags, e.g.
  `{"title": "Dune", "author": "Frank Herbert", "description": "…"}`. Any
  field can be left out. The feed URL doesn't change.
- **Cover art:** `cover`, `folder`, `front` or `poster` `.jpg`/`.png` in the
  show's folder, then any single image there, then art embedded in the audio,
  then a generated placeholder. A loose `Dune.m4b` uses `Dune.jpg`.
- **Description:** `description.txt` or `desc.txt` in the folder (`Dune.txt`
  for a loose file), then the file's description or comment tag.
- **Chapters** in m4b/m4a files are published as
  [Podcasting 2.0 chapters](https://github.com/Podcastindex-org/podcast-namespace/blob/main/chapters/jsonChapters.md);
  most apps also read them straight from the file.
- Supported formats: mp3, m4a, m4b, aac, flac, ogg, opus and wav. Hidden
  files and NAS folders such as `@eaDir` and `#recycle` are ignored.

Feed URLs are derived from each show's path, so they survive restarts and
rescans. Renaming or moving a show's folder gives it a new feed URL.

## Adding audio from the web page

Set `RESPOOL_UPLOAD_DIR` to a writable folder and the page gets an **Add
audio** button (you can also drop files anywhere on the page). Your media
folder stays read-only: uploads live in their own folder, which ReSpool scans
alongside it.

- **One upload is one show.** Give it a title and author, or leave them blank
  to take them from the files' tags (album and artist). Uploads are filed as
  `Author/Title/` inside the uploads folder, and anything you type is saved
  in the show's `respool.json`, so it wins over the tags.
- **Add to an existing show** by picking it in the dialog, or with **Add
  audio** in the show's edit dialog, for example to add the next part. Files
  with names that are already taken get a number.
- **Include a JPEG or PNG** to use it as the show's cover.
- **Edit uploaded shows** with the **⋯** button on their card: change the
  title, author, description and cover, remove single files, or delete the
  whole show. Renaming only changes what podcast apps show; the folder stays
  put, so the feed URL and existing subscriptions keep working. Deleting is
  permanent and asks first.
- Shows in your media folder can't be changed from the page, since ReSpool
  never writes there. Use a `respool.json` (see above) to fix their details.
- Every file is checked against its extension's format before anything is
  saved, so a mislabelled or partial upload is rejected as a whole.
- Supported formats are the same as above. ReSpool doesn't convert audio, so
  upload a format your podcast app can play.

Uploads arrive in a hidden `.incoming` folder and are only moved into place
once complete, so a scan never picks up half a file.

## Signing in

ReSpool can ask for a username and password before it shows the page. It's
off by default. To turn it on, add a volume for ReSpool's data and
`RESPOOL_LOGIN=true` to the command from the quick start:

```sh
docker run -d --name respool --restart unless-stopped -p 8080:8080 \
  -v /path/to/audio:/media:ro \
  -v respool-uploads:/uploads -e RESPOOL_UPLOAD_DIR=/uploads \
  -v respool-data:/data -e RESPOOL_LOGIN=true \
  -e RESPOOL_TOKEN=your-token \
  ghcr.io/jeffchimney/respool:latest
```

With Compose, uncomment `RESPOOL_LOGIN: "true"` in
[docker-compose.yml](docker-compose.yml). It already mounts the data volume.

To add signing in to a ReSpool you already run, remove the container and
start it again with those two extra settings (see [Updating](#updating)).
Keep the same `RESPOOL_TOKEN` and your feed URLs don't change. If you ran
ReSpool without a token, turning on signing in adds one to every feed URL,
so you'll need to subscribe to your feeds again.

### Creating your account

1. **Find the setup code** with `docker logs respool`. It looks like
   `setup_code=ABCD-EFGH`. The code proves you're the one running the
   server, so nobody else on your network can create the account first. A
   new code is printed every time ReSpool starts, until the account exists.
2. **Open `http://<server-ip>:8080/`.** Enter the code and choose a username
   and a password of 8 to 72 characters.
3. From then on, the same address asks you to sign in, then takes you to your
   library.

### What it protects

- **Behind your password:** the page and your library list, Add audio,
  editing and deleting shows, Rescan and OPML export.
- **Not behind it:** feeds, audio, covers and chapters. Podcast apps can't
  sign in, so these stay on their secret URLs, which include the token.
  Someone with a feed URL can listen to that show, but can't open the page
  or change anything.
- **The token stays secret:** the sign-in page never shows it. ReSpool only
  sends a browser to the URL with the token once it has signed in.
- **Without `RESPOOL_TOKEN`,** ReSpool makes a token for feed URLs and keeps
  it in the data volume, so feeds are never left without one.

### Using it

- **Each browser stays signed in for 30 days,** renewed while you keep using
  it. If you're signed out while the page is open, the next time it talks to
  ReSpool, for example when you save a change or press Rescan, it takes you
  to the sign-in page.
- **Usernames aren't case-sensitive.** Passwords are.
- **The account button** at the top right of the page lets you change your
  password, which signs out every other browser, and sign out.

### How the account is kept safe

- **The password is never stored.** ReSpool keeps a bcrypt hash of it in
  `auth.json` in the data folder, which only ReSpool's user can read.
- **The sign-in cookie** can't be read by scripts, and browsers don't send it
  with forms or scripts on other websites, so another site can't make
  changes as you. Over HTTPS, it's only ever sent over HTTPS.
- **Guessing is slowed down.** After 10 wrong passwords or setup codes in 15
  minutes, ReSpool makes that address wait before it can try again. Behind a
  reverse proxy, every attempt comes from the proxy's address, so the limit
  applies to everyone together.
- **It never starts unprotected.** If signing in is on but the data folder is
  missing or isn't writable, ReSpool stops with an error instead of starting
  without a password.
- **Keep the `respool-data` volume** when you update. It holds the account
  and, if you didn't set `RESPOOL_TOKEN`, the token for your feed URLs.
- **Sign in over HTTPS** when you're away from home. Over plain HTTP, the
  password crosses the network unencrypted (see
  [Listening away from home](#listening-away-from-home)).

### Forgot your password?

Remove the account and restart ReSpool, then create a new account with the
new setup code from the logs. This signs out every browser. Feeds and
uploads aren't affected.

```sh
docker exec respool /respool reset-login
docker restart respool
```

Without Docker, run `RESPOOL_DATA_DIR=/path/to/data respool reset-login`,
then restart ReSpool.

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
| `RESPOOL_LOGIN` | `false` | `true` requires signing in to use the page. Feeds still work with the token. Needs `RESPOOL_DATA_DIR`. See [Signing in](#signing-in). |
| `RESPOOL_DATA_DIR` | `/data` in Docker, otherwise *(none)* | Writable folder where ReSpool keeps its account, and the feed token it makes when `RESPOOL_TOKEN` isn't set. In Docker, mount a volume at `/data`. |

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
download the whole library and upload to it. [Signing in](#signing-in)
closes that gap: the page and every change need your password, and a feed
URL only gets someone the feeds. Over plain HTTP, the password crosses your
network unencrypted, like the token does, so use HTTPS (see above) when you
sign in from outside your home network. The media folder can, and should, be
mounted read-only. Feeds carry `itunes:block` and `podcast:locked`
so directories and podcast hosts won't list or import them.

Uploads only accept the audio and image formats listed above, checked by
content as well as extension, and must come from ReSpool's own page: requests
without its `X-Requested-With: ReSpool` header are refused, so other websites
can't upload through your browser.

## Development

```sh
go test ./...
go run ./cmd/respool -media ~/Audio -log-level debug
go run ./cmd/respool -media ~/Audio -login -data ./data   # with signing in
docker build -t respool .
```

The audio fixtures in `internal/media/testdata` are a few kilobytes each; see
the README there for the ffmpeg commands that generate them.

## License

[MIT](LICENSE)
