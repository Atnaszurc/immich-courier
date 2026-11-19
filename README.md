# immich-courier

A small background app for Windows, macOS and Linux that watches folders on your
computer and uploads new photos and videos to your [Immich](https://immich.app)
server. Each folder can upload to a different Immich account.

> **Status: early development. Not usable yet.**

## What it is (and isn't)

- **Upload-only.** Files you add to a watched folder get uploaded. Deleting a
  local file does not delete it in Immich, and nothing is ever downloaded.
- **Offline-tolerant.** If your Immich server can't be reached (for example,
  you're away from your home network), files are queued and uploaded once the
  server is reachable again.
- **Talks only to Immich.** No accounts, telemetry or third-party services.
- **Not a replacement** for the Immich mobile app or for bulk-import tools like
  `immich-go` and the official `@immich/cli`. It's for the "always-on folder on
  my computer" use case.

See [SPEC.md](SPEC.md) for the design.

## Getting started

1. Start `immich-courier` (on Windows, double-click it). It goes to the system tray,
   and the first time it opens its settings page in your browser.
2. **Add account:** enter your Immich server's address and log in with single
   sign-on, email and password, or an API key. The app keeps its own upload-only
   key in your system's keychain, never your password.
3. **Add folder:** pick a folder and the account it uploads to.
4. In the tray menu, turn on **Start at login**.

Settings are always available from the tray icon → **Settings…**. They open in your
browser but are served by the app itself on your computer (127.0.0.1 only).

**Single sign-on:** your Immich admin must allow `http://127.0.0.1:47823/callback`
as a redirect address for Immich's OAuth client in the login provider. Email and
password or an API key work without that.

### Command line and servers

The same program is a command-line tool when given arguments: `login`,
`add-folder`, `status`, `pause`, `resume`, `rescan`, `stop`, `list`, `run`
(headless, for servers) and `settings --print` (URL for an SSH tunnel). Run
`immich-courier help` for details. On Windows the command line is mainly meant
for Windows 11 24H2 and later and may behave differently on older versions. For
scripted bulk uploads, the official Immich CLI is a good fit.

### Building from source

```sh
go build -o bin/ ./cmd/immich-courier
```

## No promises

This is a personal project, shared in case it's useful to others. It is
provided **as is**, without warranty of any kind. There is no commitment to
future features, fixes, support or a release schedule. Issues and pull
requests are welcome, but may not get a response.

## License

[MIT](LICENSE). You're free to use, modify, fork and redistribute it, including
commercially, as long as the license notice is kept.

immich-courier is an independent community project and is not affiliated with
or endorsed by the Immich project or FUTO.
