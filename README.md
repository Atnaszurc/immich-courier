# immich-courier

A small background app for Windows, macOS and Linux that watches folders on your
computer and uploads new photos and videos to your [Immich](https://immich.app)
server. Each folder can upload to a different Immich account.

> **Status: first release (0.1).** It works on Windows, macOS and Linux, but it is young:
> expect rough edges, and see [No promises](#no-promises).

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

## Download

Builds for Windows, macOS and Linux are on the
[Releases](../../releases) page: a `.zip` for Windows and a `.tar.gz` for Linux (amd64
and arm64), and for macOS a `.zip` with `immich-courier.app` (Apple Silicon and Intel).
On macOS, move `immich-courier.app` to **Applications** before you start it.

**The binaries are not code-signed.** Signing costs money every year, and this is a
free hobby project. Your system will warn you the first time:

- **Windows:** SmartScreen says "Windows protected your PC". Click **More info → Run
  anyway**. (Or before extracting: right-click the zip → **Properties** → tick
  **Unblock**.)
- **macOS:** the first start fails with "Apple could not verify 'immich-courier' is free of
  malware" and offers only **Move to Trash** or **Done**. Click **Done**, go to
  **System Settings → Privacy & Security**, scroll down and click **Open Anyway** next to
  immich-courier, then start it again: the same warning now has an **Open Anyway** button,
  which asks for your password once. After that it starts normally. (Or, in Terminal, remove
  the download flag before the first start:
  `xattr -dr com.apple.quarantine /Applications/immich-courier.app`.)
  If your Immich server is on your home network, macOS then asks to let immich-courier
  find devices on your local network: click **Allow**, or uploads fail with "no route to
  host". You can change it later under **Privacy & Security → Local Network**. Updates are
  new unsigned builds, so macOS may ask again after each one.
- **Linux:** no warning; make it executable with `chmod +x immich-courier` if needed.

Instead of a signature, every release comes with a `SHA256SUMS` file and a build
provenance attestation, which proves a file was built by this repository's GitHub
workflow from a specific commit. To check a download with the
[GitHub CLI](https://cli.github.com):

```sh
gh attestation verify immich-courier-v1.0.0-linux-amd64.tar.gz --repo Atnaszurc/immich-courier
```

## Getting started

1. Start `immich-courier` (on Windows, double-click it). It goes to the system tray,
   and the first time it opens its settings page in your browser.
2. **Add account:** enter your Immich server's address and log in with single
   sign-on, email and password, or an API key. The app keeps its own upload-only
   key in your system's keychain, never your password. (On macOS it's in the
   **Keychain Access** app under the login keychain, entry "immich-courier"; the newer
   Passwords app doesn't list it.)
3. **Add folder:** pick a folder and the account it uploads to.
4. In the tray menu, turn on **Start at login**.

Settings are always available from the tray icon → **Settings…**. They open in your
browser but are served by the app itself on your computer (127.0.0.1 only).

**Single sign-on:** your Immich admin must allow `http://127.0.0.1:47823/callback`
as a redirect address for Immich's OAuth client in the login provider. Email and
password or an API key work without that.

**Server behind a proxy with its own login:** if a reverse proxy in front of Immich
asks for its own credentials (for example basic auth on the API), open **Custom
headers** when adding the account and enter what the proxy expects, such as
`Authorization` with `Basic <base64 of user:password>`. They are sent with every
request to that server and stored in the keychain next to the upload key, never in
the config file; the settings page only ever shows their names again. Use an
`https://` address, or they travel unencrypted. Single sign-on still opens in your
browser, which has to get past the proxy on its own.

### Command line and servers

The same program is a command-line tool when given arguments: `login`,
`headers`, `add-folder`, `status`, `pause`, `resume`, `rescan`, `stop`, `list`, `run`
(headless, for servers) and `settings --print` (URL for an SSH tunnel). Run
`immich-courier help` for details. On Windows the command line is mainly meant
for Windows 11 24H2 and later and may behave differently on older versions. On
macOS the program is inside the app:
`/Applications/immich-courier.app/Contents/MacOS/immich-courier status` (or link it
into your PATH). For scripted bulk uploads, the official Immich CLI is a good fit.

### Building from source

```sh
go build -o bin/ ./cmd/immich-courier
```

Windows and Linux builds are pure Go and cross-compile from anywhere. macOS builds need
a Mac with the Xcode command line tools, because the tray icon uses CGO there.
Releases are built by [.github/workflows/release.yml](.github/workflows/release.yml)
when a `v*` tag is pushed.

## Security

How the app handles your API key and files, the November 2026 security audit and its
results, and how to report a problem: see [SECURITY.md](SECURITY.md).

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
