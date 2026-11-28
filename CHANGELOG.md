# Changelog

This file lists the changes a user can notice in each release. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.1] - 2026-11-28

### Changed

- Unified mirror validation, health backoff, persistent cache state, and
  failover selection across the archive and DOI mirror clients.
- Consolidated shared HTTP status and request error formatting, retry backoff,
  download store setup, DOI parsing, and web UI view helpers with no public
  API changes.

## [0.4.0] - 2026-11-27

### Added

- Opt-in DOI mirror fallback (`AAKB_DOI_BASE_URL`, `AAKB_DOI_MIRRORS`,
  `AAKB_DOI_ALLOWED_TLDS`, `AAKB_DOI_COOKIE` and matching `aakb.Config`
  fields) with TLD enforcement, SSRF-safe transfers, challenge detection,
  backoff failover, and `Capabilities.DOIMirror`.
- Unpaywall open-access PDF fallback (`AAKB_UNPAYWALL_EMAIL` and
  `aakb.Config.UnpaywallEmail`, set to `off` to disable).
- Verified OpenAlex sibling-work lookup during downloads so working-paper
  records without a PDF can resolve to published or repository copies.

### Changed

- Canonical `/scidb/<doi>/` downloads are trusted without rejecting
  filename-stub or blank archive titles, and free `/scidb/` PDFs are tried
  before paid fast-download quota.
- Default OpenAlex searches include books, book chapters, editorials, and
  reports, report `fast_download` when a direct PDF is available, and rank
  downloadable and published-DOI works ahead of working-paper stubs.
- OpenAlex downloads check up to five external PDF locations, upgrade plain
  HTTP PDF URLs to HTTPS, and try remaining advertised PDF links when open
  locations fail.

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

[0.4.1]: https://github.com/bjoernaagaard/aakb/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/bjoernaagaard/aakb/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/bjoernaagaard/aakb/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/bjoernaagaard/aakb/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/bjoernaagaard/aakb/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/bjoernaagaard/aakb/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/bjoernaagaard/aakb/releases/tag/v0.1.0
