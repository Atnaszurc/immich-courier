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
