# immich-courier — spec

A small, cross-platform background app that watches folders and uploads new
photos and videos to an [Immich](https://immich.app) server. Think "Dropbox
folder", but upload-only and for Immich.

Status: design phase. Intended to be released as open source.

## Goals

- Drop a photo/video into a watched folder → it ends up in the right Immich account.
- Runs on Windows, macOS and Linux as a single native binary. No Node, no Electron.
- Very small idle footprint (target: < 30 MB RSS for the daemon).
- Works offline: keeps detecting and queuing files, uploads when the server is reachable again.
- Multiple folders, each mapped to its own account (and possibly its own server).
- Fully configurable from GUI, CLI and config file.

## Non-goals

- **Not a sync client.** Upload-only. Local deletes are never propagated, nothing is downloaded.
- No dependency on any system other than Immich. No provider-specific auth code
  (Authentik, Keycloak, etc.); OIDC goes through Immich's own OAuth endpoints only.
- Not a replacement for the Immich mobile app or bulk-import tools (immich-go, @immich/cli).
  This is the "always-on desktop drop folder" use case.

## Architecture

```
┌──────────────────────────┐   local IPC    ┌──────────────────────────┐
│ courier daemon (always)  │◄──────────────►│ settings UI (on demand)  │
│  - tray icon             │                │  - separate process      │
│  - watcher + scanner     │◄──────────────►│  - exits when closed     │
│  - queue (state DB)      │   CLI          └──────────────────────────┘
│  - reachability check    │◄── immich-courier <cmd>
│  - uploader              │
└────────────┬─────────────┘
             │ HTTPS (Immich API only)
             ▼
        Immich server(s)
```

- Language: Go (fsnotify, easy cross-compile, OS keychain via go-keyring).
- The daemon stays headless apart from a tray icon. The settings window is a
  separate process, started from the tray or CLI, so its memory is freed on close.
- GUI toolkit for the settings window: TBD (evaluate footprint, native feel and
  how small it builds on all three OSes).

## Auth

Any login Immich supports can be used. The app reads `GET /api/server/features` and offers:

1. **OIDC** if `oauth: true`: loopback redirect (`http://127.0.0.1:47823/callback`) through
   Immich's `/api/oauth/authorize` + `/api/oauth/callback`. Requires the Immich admin to register
   that redirect URI with their identity provider (one-time, provider-agnostic).
2. **Email + password** if `passwordLogin: true`: `POST /api/auth/login`.
3. **Paste an API key**: always available; the app opens Immich's API key page.

Methods 1 and 2 give a temporary session, which the app uses to create its own API key
(`POST /api/api-keys`, permissions `asset.upload` + `user.read`) and then logs out.
Every method ends with an API key in the OS keychain. Passwords and session tokens are never stored.
`login --method auto` picks OIDC, then password, then API key.

### OIDC findings (spike, 2026-11-19, Immich v3.3.5)

- Immich does **not** restrict `redirectUri`: `POST /api/oauth/authorize` with
  `http://127.0.0.1:<port>/callback` returns a provider URL with that redirect, PKCE (S256)
  and state. No auth needed for this call.
- Immich keeps state + PKCE verifier in cookies by default, but in v3 `OAuthConfigDto` accepts
  client-generated `state` + `codeChallenge`, and `OAuthCallbackDto` takes `state` + `codeVerifier`.
  Verified: Immich passes our own state/challenge through to the provider. So no cookie jar needed.
- The **identity provider** is the gate: a redirect URI that isn't registered is rejected
  (tested: Authentik with strict matching → 400 "mismatching redirection URI").
  So OIDC login only works if the Immich admin registers the app's loopback URI with their
  provider. That is a one-time provider setting, not app code — the app stays provider-agnostic.
- Design consequence:
  - Default to a **fixed loopback port** (configurable), e.g. `http://127.0.0.1:47823/callback`,
    so providers with exact-match redirect URIs work. Document "add this redirect URI to your
    Immich OAuth client" in the README.
  - The **zero-config path** stays the API key. The app can open the Immich web UI at the
    API key page so the user only copies and pastes one value.
  - If OAuth isn't enabled (`/api/server/features` → `oauth: false`), only show the API key option.
- Still to verify end to end: callback → session token, `POST /api/api-keys` with
  upload-only permissions, then logout.

Secrets never go in the config file; config references a keychain entry.

## Watching and queuing

- Startup: full scan of every watched folder (catches changes made while the app wasn't running).
- Runtime: fsnotify events; a periodic rescan as a safety net (network drives, missed events).
- Stability check: a file is only queued once size and mtime haven't changed for N seconds.
- Local state DB (SQLite or bbolt) per machine: path, size, mtime, SHA1, account, status.
  A restart doesn't need to re-hash files that haven't changed.
- Dedupe: SHA1 checked against `/api/assets/bulk-upload-check` before uploading.

## Connectivity

- Per server: periodic `GET /api/server/ping`, nothing else (no general internet checks).
- Unreachable → that server's uploads pause, queuing continues; exponential backoff
  on checks, capped (e.g. 5 min). Also re-check on OS network-change events where available.
- Servers are independent: one being down doesn't block another.

## Coexistence with the mobile app

- Immich v3 removed `deviceId`/`deviceAssetId` from asset upload, so there's no per-device
  bookkeeping to collide with.
- SHA1 checksum dedupe (`bulk-upload-check`) means files already backed up from a phone are
  skipped, not duplicated.

## Multi-account

- Config maps folder → account profile (server URL + keychain ref).
- Nested watched folders mapped to different accounts are rejected at config time.

## Configuration (draft)

```toml
[[account]]
name   = "personal"
server = "https://photos.example.com"
# key stored in the OS keychain (service immich-courier, entry "personal@<hash of config path + server>")
# user_id = "..."  (written at login; a later login under this name must be the same Immich user)

[[folder]]
path    = "~/Pictures/ToImmich"
account = "personal"
```

CLI (draft): `immich-courier daemon | status | add-folder | remove-folder | login | logout | settings`.

## License

MIT. The app talks to Immich over its HTTP API and doesn't include Immich code, so Immich's
AGPL-3.0 doesn't apply. Keep it that way: write our own small API client rather than
vendoring Immich's generated SDK or copying server code.

## Later / maybe

- Per-folder "add to album".
- Per-folder post-upload action: keep / move to `uploaded/` / delete.
- Sidecars (.xmp), Live Photos (HEIC+MOV), RAW+JPEG pairing.
- Pause/resume, bandwidth limit, metered-network awareness.
- Better dates for files with no EXIF date (from user feedback after the v0.1.0 release,
  2026-11-20). Immich falls back to the `fileCreatedAt` we send, which is currently the
  mtime. After phone migrations (iPhone→Android) and some copy tools, the mtime is the
  transfer date instead. Both ideas are opt-in per folder, change only the date courier
  sends on upload, never edit files or existing assets (still upload-only), and don't
  apply when the file has an EXIF date (Immich uses that anyway):
  - **Date from filename.** Parse common patterns (`IMG_20190612_143201`, `PXL_…`,
    `IMG-20190612-WA0001`, `Screenshot_…`) and send that date. Local time with no zone,
    so it's ambiguous like EXIF.
  - **Oldest file timestamp.** If creation/birth time and mtime differ by more than
    ~1 day, send the older one, in either direction. Created 2023 / modified 2026 →
    2023 (it was edited later). Created 2026 / modified 2023 → 2023 (a copy tool gave it a
    new creation time). The older timestamp is the best guess for when the photo was taken. Platform caveat: Windows `CreationTime` and
    macOS `st_birthtime` are real creation times, but Linux `ctime` is the inode *change*
    time, not creation. Linux needs `statx` btime, which some filesystems/NFS don't
    provide. Ignore implausible values (epoch 0, before ~1990, in the future).
  - Not a fix for assets already in Immich. Point those users to ExifTool or immich-go
    (Google Takeout JSON sidecars).

## Implementation notes (2026-11-19)

- State DB: **bbolt** (pure Go, no CGO). One process holds it, so a running daemon's
  status must come over IPC, not by opening the DB.
- File types come from `GET /api/server/media-types` (unauthenticated), cached per server;
  a built-in list is used until the server has been reached once. `.xmp` sidecars are ignored for now.
- Error handling: network errors and HTTP 5xx pause the account (backoff grows with consecutive
  failures, so a server that answers ping but fails uploads isn't hammered); 401 stops the account
  until login is re-run; other 4xx mark the file failed with retry (1 min doubling, max 6 h).
- Measured on Windows: 8.4 MB binary (stripped); ~19 MB working set after scanning, hashing and
  queuing 2,000 files, steady while idle.

### IPC and tray (2026-11-19)

- **One binary.** No arguments (or `tray`) runs the daemon with a tray icon when a desktop session
  is available; otherwise it prints help. `run` is the headless daemon for servers. All other
  commands are CLI. The tray logs to `immich-courier.log` next to the config (at most 5 MB plus one old copy, `.log.1`; rotated while running, not only at startup).
- Windows: built as a console app so the CLI behaves normally in terminals. An embedded manifest
  sets `consoleAllocationPolicy=detached` (Windows 11 24H2 / build 26100+), so starting it from
  Explorer or at login opens no console window; verified on build 26300. Older Windows ignores the
  policy: the tray then frees the console it was given (a brief flash), and **CLI behavior on older
  Windows may vary**. Manifest + exe icon are embedded via `rsrc` (pinned with `go get -tool`);
  the generated `rsrc_windows_*.syso` files are committed, regenerate with `go generate ./cmd/immich-courier`. The CLI is mainly for Linux/macOS; Windows users are expected to use the tray
  (and the official Immich CLI exists for scripted bulk uploads).
- IPC: HTTP over a Unix domain socket (`courier.sock` next to the config; Windows 10 1803+
  supports AF_UNIX). If that path exceeds ~100 bytes (sun_path limit), a hashed name in a private
  per-user dir is used (`$XDG_RUNTIME_DIR` on Linux, else `<tmp>/immich-courier-<user>`, must be 0700).
  Endpoints: `GET /v1/status`, `POST /v1/{pause,resume,rescan,reload,quit}`.
- One instance per config: a live socket means "already running"; the bbolt lock backs that up.
- `login`, `add-folder`, `remove-folder` tell a running instance to reload; queued files survive.
- Pause is persisted in the state DB. Watching and hashing continue while paused.
- Tray: fyne.io/systray (pure Go on Windows/Linux via DBus StatusNotifierItem; **CGO on macOS**,
  so mac builds need a mac runner). Icons are drawn in code (no Immich artwork):
  green idle, blue busy, grey paused, dark grey + slash offline, amber needs attention.
- Start at login, per user, no admin: HKCU Run key / LaunchAgent plist / XDG autostart .desktop.
- Measured on Windows: tray process ~26 MB working set with 2,000 queued files.

### Settings page (2026-11-19)

- Decision: settings live in the user's browser, not a native window. Keeps the binary small and
  pure Go, costs nothing while closed, and SSO login already happens in the browser.
- Served by the running instance (`internal/webui`) on `127.0.0.1:<random port>`, started on demand
  (tray "Settings…", `immich-courier settings`, automatically on first start with no accounts) and
  stopped after 30 idle minutes.
- Security: the tray/CLI opens `/#t=TICKET` (random, one use, 2-minute expiry, in the fragment so it
  is never sent in a request line, Referer or log). The page trades it for a session secret
  (`POST /api/session`), keeps it in sessionStorage (per tab; survives the SSO round trip) and sends
  it as `X-Courier-Session` on every API call. No cookies: browsers send cookies to every port on
  127.0.0.1 (confirmed in Chrome, Edge and Firefox, 2026-11-20). Also: exact Host check (DNS
  rebinding), custom header + Origin check on every POST (CSRF), strict CSP, no external resources.
  The page's static files need no session. Covered by tests. (Until 2026-11-20 this was an
  HttpOnly SameSite=Lax cookie holding the token.)
- Features: add account (server probe → SSO / email+password / API key), log in again, remove
  account (only when no folder uses it), watch/stop watching folders via an in-page folder browser
  (browsers can't hand real paths to a page), create folder, pause/resume, check now, live status.
- Headless servers: `immich-courier settings --print` gives the URL to open through an SSH tunnel.
- All account logic is shared with the CLI (`internal/accounts`).
- "Upload timing" section: **Wait before uploading** (`settle_seconds`, presets 5 s–15 min,
  default 5 s) doubles as an undo window like the Immich mobile app's upload delay: a file deleted or
  moved before the delay is over is never uploaded. **Check folders for missed files every**
  (`rescan_minutes`, 5–60 min, default 15). Saved immediately; config rejects out-of-range values
  (settle 1–3600 s, rescan 1–1440 min, 0 = default); non-preset values from the file show as "custom".

## Open decisions

- Linux headless: ship a systemd user unit for `immich-courier run`.
- Release builds: CI for Windows/Linux (pure Go) + macOS runner (CGO), signing/notarization.
- Per-folder options (album, post-upload action) in the settings page.

### Custom headers (2026-11-22)

Requested by a user whose Immich API sits behind basic auth at the reverse proxy (the Android
app has "custom headers"; most other clients don't). Per account, because the key and server
are per account.

- **Storage:** one keychain entry per account, `<KeyName>/headers` ("/" can't occur in an
  account name), holding the whole set as JSON. Nothing in config.toml. Written after the API
  key in `accounts.Save` and undone with it if the config can't be saved; removed by
  `DeleteKey` (logout, remove account).
- **Write-only values:** `/api/state` and `list` return names only. Forms send names with an
  empty value to mean "keep the stored value" (`accounts.ResolveHeaders`); stored values are
  only reused for the same account name on the same server. A `nil` set (CLI `login` without
  `--header`) keeps the stored headers; an empty set removes them.
- **CLI:** `--header NAME` (repeatable) on `login` and `headers`; values are prompted with
  hidden input, never taken from argv (shell history, process list). `headers --clear` removes
  them.
- **Validation** (`immich.ValidateHeaders`): RFC 7230 token names, printable-ASCII values (no
  CR/LF/NUL), no duplicates, at most 10 headers / 2000 bytes (Windows Credential Manager caps an
  entry at 2560 bytes). Reserved, because the app or net/http owns them: `Host`,
  `Content-Length`, `Content-Type`, `Transfer-Encoding`, `Connection`, `Accept`,
  `Accept-Encoding` (would turn off transparent gzip), hop-by-hop headers, `x-api-key`,
  `x-immich-user-token`, `x-immich-session-token`, `x-immich-share-key`, `x-immich-checksum`.
  `Cookie` is allowed (some access gateways use one).
- **Sending:** custom headers are set first, then the app's own (`immich.Client.authorize`),
  on every request including `/server/ping`, since the proxy gates that too. Ping still
  sends no Immich credentials. The existing same-origin redirect check covers them.
- **Session tokens** (password/OAuth login, only while minting the API key) normally go as
  `Authorization: Bearer`. If a custom header uses `Authorization`, the token goes in Immich's
  `x-immich-user-token` header instead (Immich checks that one first).
- **Changing headers on an existing account** (`/api/accounts/headers`, CLI `headers`)
  re-checks `/users/me` with the stored key and the new set, as the same Immich user, before
  saving; a set the proxy refuses is not saved.
- **Probe hint:** a 401/403/407 on ping can't come from Immich (ping is unauthenticated), so
  the error suggests adding custom headers.
- **OAuth:** only the app's API calls carry the headers. The browser part of SSO goes to the
  identity provider and the loopback callback, not through the Immich proxy; if Immich's own
  web login is behind the proxy, the browser handles that itself.
- **http://:** allowed (LAN setups), with a warning in the CLI and the headers dialog.

### Security audit fixes (2026-11-20)

From the November 2026 audit (how it was run and the results: [SECURITY.md](SECURITY.md)). Each fix
has a regression test that fails on the pre-fix code.

- **Watched files are opened safely** (`internal/fsafe`): symlinks are never followed (Lstat, and
  `O_NOFOLLOW` on Unix), FIFOs and devices are never opened blocking (`O_NONBLOCK`), and the opened
  handle is checked (regular file, settled size and mtime) before hashing. The uploader opens the
  file once, checks it against the hashed record and uploads from that handle. Hashing stops on
  cancel. On Windows the check is best effort (Lstat + same-file-ID after open); creating symlinks
  there needs admin or Developer Mode.
- **No credentialed redirects off the server's origin**: the Immich client refuses a redirect to
  another scheme or host (the `x-api-key` header is not stripped by net/http on redirects).
- **Media types**: the server's list can only narrow the built-in list of known media extensions,
  never widen it; the cached copy is filtered the same way. New Immich formats must be added to
  `fallbackExtensions`.
- **Failures are charged to the file**: any upload failure (network, 5xx, 4xx, unreadable file)
  gives that file a retry time, so a file the server keeps failing can't block its account. Network
  errors and 5xx still pause the account; an unreadable file doesn't.
- **OIDC callback**: a request without the login's `state` is answered 400 and ignored (the login
  keeps waiting); the provider's free-text `error_description` is not shown.
- **OS opener**: only absolute http(s) URLs are passed to rundll32 / open / xdg-open (the OAuth
  URL comes from the server).
- **CLI `status`** escapes control characters in file names, errors and paths.
- **Accounts**: logging in under an existing name must be the same server and the same Immich user
  (recorded as `user_id`); otherwise it is refused, so folders can't silently move to another
  server or person. Names are unique ignoring case. The keychain is written after all checks and
  restored if the config save fails. Keychain entries are named `<name>@<hash(config path, server)>`;
  entries from earlier builds (bare name) are moved on first use. `logout` takes `--config`.
- **Per-config files**: a config other than `config.toml` gets its own `<file>.state.db`,
  `<file>.sock` and `<file>.log`, so two configs in one folder never share a queue.
- **Removed or re-bound folders**: queued files whose folder no longer belongs to the account are
  dropped (marked rejected, re-hashed if the folder is watched again) instead of uploaded.
- **Settings page without cookies** (the cookie reaching other local ports was confirmed in all
  three browsers): one-time 2-minute ticket in the link fragment, traded for a per-tab session
  secret sent in a header; see "Settings page" above. This also makes a link read from
  `xdg-open`'s command line (other Linux users) useless once used.
- **Socket folder ownership**: the daemon and every CLI/tray client refuse a socket folder that is
  a symlink, belongs to another user, or is writable by others (the shared-`/tmp` fallback).

### Release builds (2026-11-20)

- GitHub Actions: `ci.yml` vets and tests on Linux, Windows and macOS for every push and pull
  request, and checks that the committed Windows `.syso` resources still rebuild byte for byte.
  `release.yml` runs on a `v*` tag: Linux and Windows (amd64, arm64) cross-compiled with
  `CGO_ENABLED=0` on Ubuntu; macOS built on a macOS runner (CGO for the tray), arm64 + amd64
  joined with `lipo` into one universal binary in an `.app` (see "macOS app bundle"), minimum macOS 11. Output: zip (Windows, macOS) or
  tar.gz with LICENSE, README and SECURITY, plus `SHA256SUMS` and build provenance
  attestations, in a **draft** release to review and publish by hand.
- Decision: **no code signing** on any platform (cost). The README explains the SmartScreen
  and Gatekeeper warnings; provenance attestations stand in for a signature. Go's linker still
  ad-hoc signs darwin/arm64 code, which Apple Silicon requires.
- Dependabot opens weekly updates for the actions and Go modules.

### macOS app bundle (2026-11-20)

Found by testing v0.0.1/v0.0.2-test on Apple Silicon (macOS 26):
- A **bare binary started by launchd** (the Start-at-login LaunchAgent) got "no route to host"
  for a LAN Immich server, although the same binary worked from Terminal and immich-courier was
  listed as allowed under Privacy & Security → Local Network. Started from Terminal, Terminal is
  the "responsible" app and lends its Local Network permission; started by launchd, the bare
  binary has no stable app identity to hold that permission.
- **Turning Start at login on killed the running copy** (SIGKILL: nothing in the log, even after
  SIGTERM got a handler) the first time macOS registered the new login item; later toggles didn't.

So macOS releases ship `immich-courier.app` (zip):
- `packaging/macos/Info.plist`: bundle ID `io.github.atnaszurc.immich-courier`, `LSUIElement`
  (menu bar only, no Dock icon), `NSLocalNetworkUsageDescription` (so macOS can show the Local
  Network prompt), minimum macOS 11. Icon: `.icns` built by `iconutil` from
  `tools/winicon -iconset`, same artwork as the Windows icon.
- Ad-hoc signed (`codesign --sign -`), no Developer ID. The signature seals Info.plist to the
  binary. Each build has a new ad-hoc identity, so macOS may ask for Local Network again after an
  update.
- Start at login from inside a bundle: the LaunchAgent runs `/usr/bin/open -g -a <app> --args
  tray [--config …]`, so LaunchServices starts it as the app (own permission; no second copy if
  already running) and `AssociatedBundleIdentifiers` shows its name and icon in Login Items. A
  bare binary (built from source) is still started directly.
- **Verified on Apple Silicon with v0.0.3-test (2026-11-20):** installed in /Applications, menu bar
  icon and no Dock icon; macOS showed the Local Network prompt on the first server connection and
  uploads worked after Allow; enabling Start at login no longer killed the running copy (the plist
  uses `/usr/bin/open`); after logging out and in, the app started, was online and uploaded to the
  LAN server.
