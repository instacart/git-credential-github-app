# Transient-error resilience for the credential helper

## Problem

The credential helper makes GitHub API calls (`FindOrganizationInstallation`,
`CreateInstallationToken`, `ListInstallations`) that can fail with transient
errors — 5XX server responses, 429 rate limiting, and network blips. Today any
such failure aborts immediately via `log.Fatal`, surfacing a hard failure to git
even when a simple retry would succeed.

## Approach

Insert retry logic at the HTTP transport layer. `newGithubAppClient` already
accepts an `http.RoundTripper`, and the call chain is:

```
github.Client → http.Client{Transport: ghinstallation AppsTransport} → base RoundTripper
```

The ghinstallation transport adds the JWT auth header, then delegates to the base
transport. Making a **retrying transport the base** means each retry re-sends the
already-authenticated request at the network level — auth happens once, retries
are pure transport concerns.

Use `github.com/hashicorp/go-retryablehttp`.

## Implementation

- New helper `newRetryableTransport()` returns an `http.RoundTripper` built from
  `retryablehttp.NewClient()`:
  - `RetryMax = 4`, `RetryWaitMin = 1s`, `RetryWaitMax = 30s`.
  - Custom retry policy (`githubRetryPolicy`): extends `DefaultRetryPolicy`
    (5XX except 501, 429, network errors) to also retry GitHub's secondary
    rate-limit responses, which arrive as **403 with a `Retry-After` header**.
    Primary rate-limit 403s (no `Retry-After`, reset potentially an hour away)
    are deliberately **not** retried so git fails fast instead of hanging.
  - Custom backoff (`cappedBackoff`): honors a server-supplied `Retry-After`
    (seconds) for 403/429/503 but **caps every wait at `RetryWaitMax`**, and
    caps the HTTP-date path of `DefaultBackoff` too. This is the key fix for the
    library default, which returns `Retry-After` uncapped and could otherwise
    block git for minutes.
  - `Logger = nil` to suppress default per-request chatter; a `RequestLogHook`
    logs a brief stderr warning only when `attempt > 0`.
- Wire it into `newGithubAppClient` in place of `http.DefaultTransport`. Both
  `doGet` and `doGenerate` benefit automatically.
- Each entry point wraps its context with a hard deadline as a final ceiling:
  `getTimeout = 60s` (git's critical path) and `generateTimeout = 2m` (interactive,
  paginating command). Once the deadline passes, retries stop and the failure
  surfaces through the existing fatal paths.

## Error handling

After retries are exhausted, behavior is unchanged: the existing `fatal()` /
`log.Fatal()` calls report the final error and exit non-zero so git sees a clean
failure.

## Testing

`git-credential-github-app_test.go`:

- 503 twice then 200 → request ultimately succeeds; server observed 3 requests
  (`RetryWaitMin` lowered for speed).
- Always 503 → final error surfaces after retries are exhausted.
- 200 first response → exactly one request, no spurious retries.
