# Changelog

This file lists the changes a user can notice in each release. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.3.0] - 2026-11-26

### Changed

- Archive settings use the `AAKB_ARCHIVE_` prefix. Set `AAKB_ARCHIVE_BASE_URL`
  or `AAKB_ARCHIVE_MIRRORS` to enable archive operations. Go callers configure
  `BaseURL` or `Mirrors` directly.
- The default download directory is `~/aakb-downloads`.

## [0.2.1] - 2026-11-26

### Changed

- CLI help, MCP tool descriptions, documentation, and diagnostics use neutral archive wording.
- The HTTP server registers MCP tools once at startup.

### Fixed

- Help, version, and health work with invalid provider settings.
- `-json-errors` formats argument and configuration errors as JSON. Put it first to include flag parsing errors.
- The HTTP server waits for shutdown when the listener stops.

## [0.2.0] - 2026-11-26

### Added

- Native archive and OpenAlex clients inside `aakb`, replacing the
  external archive client dependency and `goquery`.
- `AAKB_ARCHIVE_MIRRORS` and `aakb.Config.Mirrors` for configuring additional
  failover mirror hosts.
- `aakb.Config.AllowedTLDs`, `aakb.Config.HTTPClient`, `aakb.Config.UserAgent`,
  and `Client.UpstreamHealth`.
- Unified local download store with an indexed manifest for O(1) reuse
  lookups and cross-process locking.
- Fast-download server health memory so failing partner servers are tried
  last instead of delaying every download.

### Changed

- `AAKB_ARCHIVE_ALLOWED_TLDS` now filters every mirror candidate, including
  failover mirrors, before credentials are sent.
- `GET /healthz?upstream=1` probes the active mirror through `/dyn/up/`
  and mirror health state instead of an unauthenticated `HEAD` request.
- DOI verification reads `/db/aarecord_elasticsearch/md5:<hash>.json`
  instead of scraping full HTML record pages.
- OpenAlex requests use retry backoff on transient `429` and `5xx`
  responses and run independently of the archive.

### Removed

- Dependency on the external archive client,
  `github.com/PuerkitoBio/goquery`, and `github.com/andybalholm/cascadia`.

## [0.1.1] - 2026-11-25

### Added

- The Go library at `github.com/bjoernaagaard/aakb`.
- The REST API at `/api/v1` and the web UI at `/`, both served by `aakb serve`.
- OpenAlex search takes author, institution and repository ids and citation
  relations. An OpenAlex download can pick the artifact, location and
  version. `details` and `get_details` take an OpenAlex work id.
- `AAKB_TRUSTED_PROXIES`, so a server behind a proxy counts requests for
  each real client.
- `-json-errors` on every command, `-timeout` on `account`, and progress
  lines during a download.
- Progress notifications from the MCP `download` tool.
- Value ranges and allowed choices in the MCP tool schemas.
- `aakb health`, which the Docker health check now uses.
- `.env.local` loading and `AAKB_ARCHIVE_ALLOWED_TLDS`.
- `aakb version` prints the commit and its time.

### Changed

- A timeout above 600 seconds, or above 7200 seconds for a download, is
  lowered to that cap.

## [0.1.0] - 2026-11-24

First release. It holds the MCP server over stdio and Streamable HTTP, the
`search`, `details`, `read`, `download` and `account` commands, OpenAlex
search, article reading and downloads behind `OPENALEX_API_KEY`, bearer
token sign-in, the request limit for each client, and
`GET /downloads/{name}`.

[0.3.0]: https://github.com/bjoernaagaard/aakb/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/bjoernaagaard/aakb/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/bjoernaagaard/aakb/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/bjoernaagaard/aakb/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/bjoernaagaard/aakb/releases/tag/v0.1.0
