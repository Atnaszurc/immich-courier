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

1. **API key** (always supported): user pastes a key; stored in the OS keychain.
2. **OIDC login** (if the server has OAuth enabled): loopback redirect
   (`http://127.0.0.1:<port>/callback`) through Immich's `/api/oauth/authorize` +
   `/api/oauth/callback` → Immich session → app creates an API key scoped to
   upload-only permissions via `POST /api/api-keys` → stores the key in the keychain
   and logs out the session.
   - Open question: does Immich accept a non-mobile redirect URI? Verify first.

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

- Uses its own `deviceId` (e.g. `immich-courier-<hostname>`) and
  `deviceAssetId` derived from the file path + mtime.
- Checksum dedupe means files already backed up from a phone are skipped, not duplicated.

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

## Later / maybe

- Per-folder "add to album".
- Per-folder post-upload action: keep / move to `uploaded/` / delete.
- Sidecars (.xmp), Live Photos (HEIC+MOV), RAW+JPEG pairing.
- Pause/resume, bandwidth limit, metered-network awareness.

## Open decisions

- License (MIT / Apache-2.0 / AGPL-3.0 to match Immich).
- Settings UI toolkit.
- State DB: SQLite (modernc, pure Go) vs bbolt.
