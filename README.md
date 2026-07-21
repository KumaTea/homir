# Homir

**Homir** (Home Mirror) is a self-hosted, on-demand package mirror. It acts as
a compatible package proxy: it immediately streams an uncached artifact from a
configured upstream to the requesting client while saving it locally. Later
requests are served from the local cache.

Unlike a public full mirror, Homir retains only artifacts that clients actually
download. A background manager refreshes active packages and releases disk space
when cached content becomes inactive.

> **Project status:** the streaming cache core and native APT, Alpine APK, and
> PyPI routes are implemented. The administration dashboard currently provides
> authenticated, read-only status; configuration editing and package prefetch
> are the next milestones.

## Goals

- Keep the first download responsive by proxying upstream bytes to the client in
  real time; never wait for a full artifact to cache before responding.
- Serve cached package artifacts quickly from local disk.
- Let concurrent clients share a single in-progress upstream download of the
  same artifact, without a slow client blocking others.
- Preserve each ecosystem's normal integrity and signature mechanisms.
- Be easy to deploy as a small Docker container on a home server or LAN.
- Use explicit, allowlisted upstreams rather than becoming an arbitrary proxy.

## Initial scope

The first release will support these HTTP package protocols:

| Backend | Initial repository scope |
| --- | --- |
| APT | Debian and Ubuntu security repositories |
| APK | Alpine Linux repositories |
| PyPI | Simple API metadata, wheels, and source distributions |

The image will be published for `linux/amd64` and `linux/arm64`. Homir is
LAN-first, but package-serving endpoints are not artificially restricted to a
LAN. The administration interface is authenticated.

Planned follow-up backends include general APT repositories, RPM-MD
(Fedora/EPEL), Go modules, Cargo sparse, and Arch/pacman.

## Core behavior

For a cache miss, Homir creates one in-progress download session per artifact.
It streams the upstream response to the first client while appending it to a
temporary file. Further clients read available bytes from that growing file and
wait only if they catch up to the upstream download. Once verification and the
download complete, the file is atomically promoted to a cache entry.

This approach supports simultaneous downloads of different files, shared
downloads of the same file, and HTTP Range/resume requests. Incomplete files
are never served as completed cache entries.

## Cache lifecycle

Only completed package artifacts are tracked for inactivity retention. By
default, Homir keeps a tracked artifact for 30 days after its last use and runs
cleanup hourly. The default capacity is 50 GB. Cleanup first removes inactive
tracked artifacts; if the cache is still over capacity, it evicts completed
entries from least recently used to most recently used. In-progress downloads
are never cleanup candidates.

Configure these values under the `cache` section:

```yaml
cache:
  max_size_bytes: 50000000000
  inactivity_ttl: "720h"
  cleanup_interval: "1h"
  watch_interval: "24h"
  prefetch_versions: 5
```

Repository metadata does not count as a requested package. It is retained for
its freshness policy and only becomes an eviction candidate under disk pressure.

Successfully served package artifacts also enter a watch list. Homir performs
a conditional upstream refresh for active watched artifacts once a day by
default, and removes watch records after the same 30-day inactivity period.
This first implementation refreshes known artifacts; discovering and prefetching
new package versions is protocol-specific. PyPI now uses its project JSON
metadata to prefetch the newest five non-yanked releases by upload time. To
avoid downloading every platform-specific wheel, it chooses one broadly useful
artifact per release: a universal wheel when available, otherwise the source
distribution. Other clients and platforms still receive uncached artifacts by
live streaming on demand.

APT records package/version/architecture mappings from the signed `Packages`
indexes that clients already request. For an active watched package, Homir
prefetches the newest indexed versions for the architecture actually requested
by a client (falling back to `Architecture: all`), without downloading unrelated
CPU architectures.

Alpine follows the same policy through its signed `APKINDEX.tar.gz`: Homir
tracks the package, version, and requested architecture, then prefetches newer
compatible `.apk` artifacts while excluding unrelated architectures.

## Milestone 1 quick start

The current route is a technical-preview endpoint for exercising the shared
cache engine. It is not the final APT, APK, or PyPI URL layout.

```bash
docker build -t homir .
docker run --rm -p 8080:8080 \
  -v "$PWD/homir.example.yaml:/etc/homir/homir.yaml:ro" \
  --workdir /tmp homir
```

The supplied configuration exposes Debian Security at:

```text
http://localhost:8080/apt/debian-security/
```

For Debian Bookworm, a client source entry is:

```text
deb http://<homir-host>:8080/apt/debian-security bookworm-security main
```

APT metadata is relayed unchanged, including its upstream signature. Homir
caches `.deb`, `.udeb`, and `.ddeb` artifacts after an actual download; signed
metadata uses the upstream's configured refresh interval. The service reports
readiness at `GET /healthz` with HTTP 204.

For Alpine, configure the repository URL as:

```text
http://<homir-host>:8080/apk/alpine-main
```

Homir relays Alpine's signed repository index unchanged and caches requested
`.apk` artifacts using the same streaming and lifecycle policy as APT files.

For PyPI, point pip at the Simple API route:

```bash
pip install --index-url http://<homir-host>:8080/pypi/pypi/simple/ requests
```

Homir rewrites the upstream Simple-page artifact links into signed local URLs,
preserving package hashes while ensuring wheels and source distributions stream
through and remain in the local cache.

The protocol-neutral technical-preview endpoint remains available for core
cache testing:

```text
http://localhost:8080/v1/proxy/<upstream-name>/<artifact-path>
```

## Admin dashboard

Set `HOMIR_ADMIN_PASSWORD` when starting Homir to enable the lightweight,
authenticated dashboard at `/admin/`. It shows cache totals and configured
upstreams. The dashboard intentionally starts read-only; package-serving
endpoints continue to work when no admin password is configured.

```bash
docker run --rm -p 8080:8080 -e HOMIR_ADMIN_PASSWORD='choose-a-long-password' \
  -v "$PWD/homir.example.yaml:/etc/homir/homir.yaml:ro" \
  --workdir /tmp homir
```

When Homir is exposed outside a trusted LAN, put it behind a TLS reverse proxy.

## Development verification

Go is built and tested in Docker so no host Go installation is required:

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.24 go test -race ./...
```

The integration suite verifies live first-download streaming, same-artifact
request coalescing, cached Range responses, parallel distinct downloads,
primary-to-backup failover, APT metadata/artifact handling, APK
metadata/artifact handling, PyPI link rewriting/artifact handling, and cache
lifecycle eviction.

## Documentation

- [Product decisions and implementation plan](docs/PROJECT_PLAN.md)
- [Streaming cache and architecture contract](docs/ARCHITECTURE.md)

## License

Homir is released under [Apache-2.0](LICENSE).
