# Security policy

## Supported versions

Only the latest release gets security fixes.

## Reporting a vulnerability

Report a vulnerability in private through
[GitHub security advisories](https://github.com/bjoernaagaard/aakb/security/advisories/new).
Do not open a public issue for it. Include the aakb version from
`aakb version`, the command or request that shows the problem, and what an
attacker gains from it.

## What aakb protects

- The archive secret key, the account cookie and the OpenAlex key are
  read from the environment. They are never tool arguments, and they are
  never written to logs or error messages.
- `aakb serve` listens on `127.0.0.1` by default. On any other address, the
  REST API and the web UI answer `403 TOKEN_REQUIRED` until `AAKB_HTTP_TOKEN`
  is set.
- Cross-origin writes are rejected, and each client gets 120 requests a
  minute.
- `GET /downloads/{name}` serves only files inside the download folder.

aakb does not terminate TLS. Put a public server behind a proxy that does.
