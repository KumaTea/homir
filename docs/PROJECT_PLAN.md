# Homir product decisions and implementation plan

This document records the decisions agreed before implementation. It is the
source of truth for product scope; implementation details may evolve without
changing these user-visible commitments.

## Product definition

Homir is a self-hosted home mirror: an on-demand caching proxy for open-source
package services. It is not a full public mirror. Clients use their normal
package manager against a Homir URL; Homir obtains and caches only package
artifacts that clients actually download.

Homir is LAN-first, but does not deliberately restrict package endpoints to a
LAN. Administrative operations are protected by authentication.

## First-release scope

- APT support for Debian and Ubuntu security repositories.
- APK support for Alpine Linux repositories.
- PyPI support for the Simple API, wheels, and source distributions.
- Configured HTTP/HTTPS upstreams and HTTP downstream package endpoints.
- Docker deployment for `linux/amd64` and `linux/arm64`.
- SQLite metadata, local artifact cache, a small authenticated web UI, and a
  configuration-file-first workflow.

Out of scope for the first release:

- General Debian/Ubuntu release and backports repositories.
- RPM-MD (Fedora/EPEL), Go modules, Cargo sparse, and Arch/pacman.
- `arm/v7` image support.
- An rsync transport, an arbitrary HTTP proxy, user roles, OAuth, and package
  publishing.

## Upstreams and availability

Each upstream is named and explicitly allowlisted in configuration. It has one
primary URL and an ordered list of equivalent backup URLs. A backup is used only
for connection, TLS, timeout, or 5xx failures; a 404 is an authoritative result
and must not trigger a fallback.

Backups must describe the same logical repository. Homir must never treat an
Ubuntu source as a Debian fallback, for example. Cache metadata and logs retain
the upstream identity used for a transfer.

Homir v1 uses HTTP/HTTPS for upstream retrieval. Its design separates package
protocol handling from retrieval so an rsync-based background synchronization
transport can be introduced later without changing client URLs. Rsync is not a
downstream protocol and is not required for real-time cache-miss streaming.

## Metadata freshness and security repositories

Any upstream may be marked `security: true`; this is a common rule rather than
an APT-only special case. Defaults are:

| Policy | Default |
| --- | --- |
| Standard metadata revalidation | 6 hours |
| Security metadata revalidation | 30 minutes |
| Configurable lower bound | 5 minutes |

When a metadata entry's TTL expires, Homir conditionally revalidates it using
ETag or Last-Modified where possible. It relays signed repository metadata
without changing or re-signing it. If expired security metadata cannot be
revalidated, Homir returns an upstream error rather than presenting it as
current. Administrators may tune all freshness intervals.

## What is tracked and retained

Only a successful actual artifact download counts as a package request. Index,
search, and metadata requests do not add a package to the watch list.

Defaults, all configurable through the config file and eventually the UI:

| Policy | Default |
| --- | --- |
| Inactivity retention period | 30 days |
| Versions kept per package/project | 5, when upstream provides them |
| Cache capacity | 50 GB |
| Eviction order | inactive artifacts, old versions, then least recently used |

The artifact store is disposable. Configuration and SQLite metadata are the
important parts to back up. No external telemetry is sent; operational logs are
local with configurable retention.

## Administration and UI

The configuration file is authoritative. The UI validates proposed changes,
writes them atomically, and requests a safe reload; it does not maintain a
second incompatible source of configuration.

The first release has one administrator account. It is bootstrapped from
configuration or an environment variable, stores its password using Argon2id,
and uses authenticated browser sessions. Package download endpoints do not
require authentication. Deployments exposed beyond a trusted LAN should place
Homir behind TLS, typically at a reverse proxy. The implemented bootstrap path
uses `HOMIR_ADMIN_PASSWORD` (with `admin.username`, defaulting to `admin`), and
the configuration also reserves `admin.password_hash` for a persisted Argon2id
credential.

The UI is server-rendered and lightweight. It includes:

- upstream health, selected source, last revalidation, and last error;
- cache and disk usage, active transfers, and watch-list status;
- artifact/package inspection and purge controls;
- configuration validation and editing;
- concise transfer, eviction, and error events.

## License

Homir will use Apache-2.0. It remains permissive while including an explicit
patent grant appropriate for infrastructure software.

## Delivery milestones

1. **Complete:** core Go HTTP service, YAML configuration parser, SQLite, disk
   layout, Docker image, and a protocol-neutral technical-preview endpoint.
2. **Complete:** streaming cache sessions with growing temporary files, shared
   consumers, cached Range handling, atomic promotion, failed-transfer cleanup,
   and a periodic lifecycle manager. The manager tracks successful artifact
   downloads, removes inactive tracked content, and applies LRU capacity
   eviction while excluding active sessions.
3. Upstream routing, retries/failover, metadata freshness, cache retention, and
   health reporting.
4. **Complete:** APT security, Alpine APK, and PyPI backends. Native
   `/apt/<upstream>/...` and `/apk/<upstream>/...` paths relay signed
   upstream metadata unchanged; `/pypi/<upstream>/simple/...` rewrites
   upstream artifact links into signed local URLs. All backends cache package
   artifacts. Debian `apt-get`, Alpine `apk`, and Python `pip`
   client-container smoke tests have passed.
5. **In progress:** authenticated lightweight UI, documentation, Docker
   multi-architecture builds, and full integration tests. The initial
   server-rendered dashboard provides authenticated read-only cache and
   upstream status; configuration editing, transfer/health details, and
   package controls remain pending. The watch worker records successful
   artifact requests only, conditionally refreshes active watched artifacts on
   a configurable daily interval, and expires watches with the normal
   inactivity policy. The PyPI worker discovers releases through the upstream
   project JSON endpoint and prefetches a configurable number of newest
   non-yanked releases, choosing a universal wheel or source distribution per
   release. APT now parses cached `Packages`, `Packages.gz`, and `Packages.xz`
   indexes to associate completed `.deb` downloads with their real package
   names and versions; architecture-aware newer-version selection remains
   pending, as does APK version discovery.
6. Follow-up protocol families: general APT, RPM-MD, Go modules, Cargo sparse,
   and Arch/pacman.

## Acceptance criteria

- On a cache miss, a client receives bytes immediately while the file is cached.
- A second client requesting the same artifact shares the in-progress transfer;
  it does not trigger another full upstream download.
- Independent artifacts download concurrently.
- Cached completed artifacts serve without contacting upstream within their
  valid cache policy.
- Range and resume requests behave correctly for completed and in-progress
  artifacts.
- Failed, interrupted, checksum-mismatched, or otherwise incomplete transfers
  never become completed cache entries.
- Inactive content is reclaimed according to retention and capacity policies.
- Signed security metadata is relayed unchanged and expired metadata is not
  represented as freshly validated when its upstream is unavailable.
- The service recovers cleanly after a container restart.
