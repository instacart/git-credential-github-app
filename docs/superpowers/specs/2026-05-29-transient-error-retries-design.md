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
  - `RetryMax = 4`, `RetryWaitMin = 1s`, `RetryWaitMax = 30s`, exponential
    backoff with jitter (library defaults — bounded worst-case wait ~30–45s).
  - `DefaultRetryPolicy` retries on 5XX (except 501), 429, and network errors;
    `DefaultBackoff` honors `Retry-After` on 429/503.
  - `Logger = nil` to suppress default per-request chatter; a `RequestLogHook`
    logs a brief stderr warning only when `attempt > 0`.
- Wire it into `newGithubAppClient` in place of `http.DefaultTransport`. Both
  `doGet` and `doGenerate` benefit automatically.

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
