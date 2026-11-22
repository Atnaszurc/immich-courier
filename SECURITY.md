# Security

**Status (20 November 2026):** the code has had a full security audit, and every issue it
found has been fixed. Details below.

## Reporting a problem

Please report security problems privately through GitHub's private vulnerability
reporting (the repository's **Security** tab → **Report a vulnerability**), not in a
public issue. The [No promises](README.md#no-promises) section applies here too: this is
a personal project and there is no guaranteed response time.

## What immich-courier protects

- **Your Immich API key.** It lives in the OS keychain (Windows Credential Manager, macOS
  Keychain, Secret Service on Linux), never in the config file. It is only sent to the server
  it was created for, and redirects to another host are refused.
- **Custom headers** (for a reverse proxy with its own login) get the same treatment as the
  API key: stored in the OS keychain, only sent to that account's server, never followed
  through a redirect to another host, never written to the log. The settings page and CLI
  only show their names; values are write-only, and the CLI prompts for them instead of
  taking them as arguments. They can't replace the headers the app sets itself
  (`x-api-key`, `Host`, `Content-Length` and so on), and line breaks are refused.
- **Your files.** Only photos and videos in folders you chose are uploaded, only to the
  account you bound the folder to. Nothing outside a watched folder is read.
- **The settings page.** It is served on `127.0.0.1` only while open. The tray opens it with a
  one-time link that expires after two minutes; the page trades it for a session secret that
  it keeps in that browser tab and sends in a header, never in a cookie. Every request also
  passes an exact Host check (against DNS rebinding), CSRF checks on changes and a strict
  Content Security Policy.
- **The local control socket** (used by the CLI and tray) is only used in a folder that
  belongs to you and that other users can't write to.

It does not protect against someone who already runs code as your user, or against the
Immich server itself: the server you log in to receives your uploads by design.

## Security audit, November 2026

### How it was done

The whole repository at commit `ec8212f` was audited with Cloudflare's open-source
[security-audit skill](https://github.com/cloudflare/security-audit-skill) for Claude
Code, using its complete (`standard`) workflow. It is an AI-driven audit: 60 agent runs in
phases (reconnaissance, bug hunting in waves, coverage critics, then two rounds of
independent verification of every candidate). It is **not** a professional penetration test,
and finding nothing at a level doesn't prove there is nothing there.

Every finding had to be reproduced by running the real code, with dummy data only, in a
locked-down Linux sandbox: no network except loopback, fake Immich servers on `127.0.0.1`, no
access to the host's drives, sockets or real keychain, read-only source and dependencies, and
memory/CPU/time limits. Code that can't run there (Windows- and macOS-specific code, real
browsers, real keychains, anything needing a second OS user) was reviewed from source only;
leads that depend on it are listed as needing validation rather than counted as findings.

### Result

No critical, high or medium findings. 13 confirmed **low** findings, all fixed. Each fix has a
regression test built from the audit's reproduction, and those tests fail on the pre-fix code.

| Area | Finding (fixed) |
|---|---|
| Watched files | A symlink named like a photo uploaded the file it pointed to |
| Watched files | A FIFO in a watched folder stalled hashing for every folder, and blocked quit |
| Watched files | A FIFO swapped in for a queued file blocked that account's uploads |
| Server trust | The API key followed redirects to another host, including to plain HTTP |
| Server trust | The server's list of media types could widen uploads to any file type |
| Reliability | One file that kept failing stopped its whole account indefinitely |
| Sign-in | Any request to the sign-in callback port ended a pending login |
| CLI | File names with control characters could rewrite `status` output in the terminal |
| Accounts | Adding an account with an existing name silently moved its folders to the new server |
| Accounts | A failed config save after a login left a key paired with the wrong server |
| Accounts | Two config files with the same account name shared one keychain entry |
| Config files | Two config files in one folder shared the upload queue |
| Folders | Files queued before "Stop watching" were still uploaded |

What held up under test: the settings page's Host, CSRF and CSP defences, text-only
rendering, the sign-in `state` and PKCE binding, TLS verification, and the supply chain
(dependencies verify; the committed Windows resource files rebuild byte for byte).

The details of each fix are in [SPEC.md](SPEC.md#security-audit-fixes-2026-11-20).

### Leads that needed a real machine

The audit also raised five leads that depend on things the sandbox couldn't observe. All are
now closed:

| Lead | Outcome |
|---|---|
| Browsers might send the settings page's session cookie to other programs listening on other `127.0.0.1` ports | **Confirmed** by the maintainer: Chrome, Edge and Firefox all sent it, with no prompt. Fixed: the settings page uses no cookies at all (see above). |
| On multi-user Linux, the settings link and its token are passed to `xdg-open` on the command line, where other local users can read them | **Fixed without validating:** the link now works once and expires after two minutes, so a copy read from the process list is useless once the browser has used it. |
| With a very long config path, another local user could create the fallback socket folder in the shared temp directory first | **Fixed without validating:** the app and its CLI refuse a socket folder that belongs to someone else or that others can write to. |
| The login URL from the server was handed to the OS URL opener unchecked | **Fixed without validating:** only `http(s)` URLs are passed to it. |
| Account names differing only in case might share one Windows Credential Manager entry | **No longer applies:** account names must differ by more than case. |
