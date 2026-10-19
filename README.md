# godis-shortener

A small URL shortener that uses [godis](https://github.com/divizn/godis) as its only storage. It is an example of an app built on godis: it claims codes with `CAS`, counts with `INCR`, expires links and rate-limit windows with TTLs, and shares one concurrent client between all HTTP handlers. Standard library only, apart from the godis client.

The module requires `github.com/divizn/godis v0.1.0`, which is not tagged yet, so it only builds with a `go.work` that points at a godis checkout (and CI cannot build until the tag exists). Do not run `go mod tidy` before then.

## Run

Start a godis cluster (see the godis README), then:

```
go run . --godis localhost:8081,localhost:8082,localhost:8083 --base-url http://localhost:8090
```

Open http://localhost:8090, or from a shell:

```
curl -s localhost:8090/links -H 'Content-Type: application/json' -d '{"url":"https://example.com/a/long/path","expires_in":"24h"}'
```

The answer holds the short URL and a management URL. The management URL is shown once; it is the only way to see the click count or delete the link.

## Flags

| flag | default | |
| --- | --- | --- |
| `--addr` | `:8090` | listen address |
| `--godis` | `localhost:8081,localhost:8082,localhost:8083` | godis client addresses |
| `--base-url` | `http://localhost:8090` | public URL, used to build links and to refuse links back to the service |
| `--admin-token` | | admin password; `SHORTENER_ADMIN_TOKEN` works too; the admin pages answer 404 when empty |
| `--safe-browsing-key` | | Google Safe Browsing key; `SAFE_BROWSING_KEY` works too; links are not checked when empty |
| `--create-limit` | `20` | creations per IP per hour, and reports per IP per hour |
| `--trust-proxy` | `false` | take the client IP from the first `X-Forwarded-For` hop instead of the socket; only turn it on behind a proxy that sets that header itself |

## Routes

- `GET /` form
- `POST /links` form or JSON `{"url": ..., "expires_in": "24h"}` (empty or missing: never). 201, or 400 bad URL or expiry, 403 flagged by Safe Browsing, 429 with `Retry-After`, 503 godis or Safe Browsing unavailable. Answers JSON for a JSON request or `Accept: application/json`, HTML otherwise
- `GET /{code}` preview page with the full destination and a Continue link (never a redirect); counts a click. 404 unknown or expired, 410 taken down
- `POST /{code}/report` report a link
- `GET /m/{code}/{secret}` and `POST /m/{code}/{secret}/delete` manage and delete; a wrong secret answers like an unknown code
- `GET /admin` (basic auth, any user, password is the admin token) lists reported links; `POST /admin/takedown/{code}`
- `GET /healthz` 200 when godis answers a ping within a second

## Keys in godis

- `link/{code}`: JSON `{url, created, secret, removed}`; `secret` is the SHA-256 of the management secret. Claimed with `CAS` against absent, retrying with a new code on a lost race (5 tries). godis has no TTL on `CAS`, so an expiring link is claimed and then given `Expire`; if that fails the link is deleted and the request gets 503
- `clicks/{code}`, `reports/{code}`: `INCR` counters; for an expiring link they are created with `SetTTL` up front so `INCR` keeps the deadline
- `reported`: JSON array of the last 500 reported codes, updated with a `CAS` loop (godis has no key listing)
- `ratelimit/{create|report}/{ip}/{window}`: `INCR`, `Expire` to the end of the hourly window; the key's `TTL` is the `Retry-After`

A takedown rewrites the link with `CAS`, which as in godis clears its deadline: the code keeps answering 410 instead of lapsing to 404.

## Limits

- The client IP rate limit is per address, not per network.
- godis expiry follows the leader's clock, so a deadline can be off by the skew between leaders (see the godis README).
- Takedown is manual, through the admin page; reports are only counted.
- No accounts, custom codes or analytics beyond the two counters.

## Tests

`go test -race ./...` uses an in-memory fake of the godis client methods the app calls. With a cluster running, `GODIS_ADDRS=localhost:8081,localhost:8082,localhost:8083 go test -race ./...` also runs create, visit, manage and delete against it.
