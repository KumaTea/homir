# Homir streaming cache architecture

## Design principle

The first client must not wait for an artifact to download before receiving it.
Homir is a streaming proxy first and a cache second.

```text
First client                 Homir                         upstream
    │ request artifact         │                               │
    ├─────────────────────────>│ GET, with retry/failover       │
    │                          ├──────────────────────────────>│
    │<── bytes immediately ────┤<── response bytes ────────────┤
    │                          │ append the same bytes to .part │
```

Each artifact has one cache session while it is downloading. The session owns
the temporary file, the upstream response, byte availability state, checksum
state, and a notification mechanism for waiting consumers.

```text
                 ┌─ first client: receives bytes live
upstream ───────>├─ append-only temporary file
                 └─ other clients: read available file bytes; wait at frontier
```

No consumer controls the upstream read rate. A slow consumer therefore cannot
block a fast client or the cache writer.

## Cache state machine

```text
missing
  │ request
  ▼
downloading (.part) ── failure/cancel/validation error ──> discarded
  │ successful EOF + integrity validation
  ▼
complete (atomically promoted)
  │ retention/capacity eviction
  ▼
evicted
```

Only `complete` entries are normal cache hits. A temporary download can serve
live or growing-file consumers, but it is never mistaken for a complete object.

## Concurrency and Range requests

- Different artifact keys use independent sessions and may download in parallel.
- Requests for the same key join its existing session rather than create a
  second upstream transfer.
- A joining reader serves bytes already present in the temporary file, then
  waits on a condition/event until the writer advances or ends.
- A Range beginning beyond the written frontier waits until that offset becomes
  available. A Range within written bytes starts immediately.
- Completed entries use standard file-based Range responses.
- If the shared upstream transfer fails, readers receive an appropriate transfer
  failure; Homir discards the incomplete temporary entry. A later request may
  start a fresh session.

## Integrity and upstream changes

Package artifacts are treated as immutable cache objects. Repository metadata
is what determines whether newer artifacts exist. Homir does not overwrite a
completed artifact while it is being served.

For a new or revalidated transfer, Homir records upstream validators where
available and checks protocol-provided integrity material, such as package
index hashes or client-verifiable signatures. A changed checksum or upstream
inconsistency creates a new cache generation or a visible transfer error; it
does not silently replace a valid object in place. Retrying with backoff avoids
caching content while an upstream mirror is in an inconsistent update window.

## Package backends

Backends translate native client paths into configured upstream paths, define
metadata freshness and integrity rules, and identify artifact downloads. They
share the cache/session, storage, upstream-health, retention, and UI layers.

| Backend | Metadata | Artifacts | Trust behavior |
| --- | --- | --- | --- |
| APT | `InRelease`/`Release`, package indexes | `.deb` files | relay signed metadata unchanged |
| APK | signed repository indexes | `.apk` files | relay indexes and preserve APK verification |
| PyPI | Simple project pages | wheels and sdists | preserve upstream links/hashes and cache artifacts |

## Storage and operations

SQLite stores artifact identity, package/version relationships, access times,
upstream validators, cache state, and transfer outcomes. Disk stores content
under stable cache keys and temporary partial files. The cache is disposable;
configuration and the database are backed up.

Security-marked upstreams use the common 30-minute metadata revalidation
default; ordinary repositories use six hours. Expired security metadata that
cannot be revalidated is not presented as current.
