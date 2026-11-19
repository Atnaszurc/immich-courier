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
# key stored in OS keychain under immich-courier/personal

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

## Open decisions

- Settings UI toolkit.
- IPC mechanism between daemon, CLI and settings UI (named pipe / unix socket).
- Running at login: per-OS autostart (Startup folder / LaunchAgent / systemd user unit).
