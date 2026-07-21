# Homir

**Homir** (Home Mirror) is a self-hosted, on-demand package mirror. It acts as
a compatible package proxy: it immediately streams an uncached artifact from a
configured upstream to the requesting client while saving it locally. Later
requests are served from the local cache.

Unlike a public full mirror, Homir retains only artifacts that clients actually
download. A background manager refreshes active packages and releases disk space
when cached content becomes inactive.

> **Project status:** Milestone 1 is implemented: the protocol-neutral streaming
> cache core, SQLite metadata store, primary/backup retrieval, and Docker build
> are ready. Native package-manager routes are the next milestone.

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
```

Repository metadata does not count as a requested package. It is retained for
its freshness policy and only becomes an eviction candidate under disk pressure.

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

The protocol-neutral technical-preview endpoint remains available for core
cache testing:

```text
http://localhost:8080/v1/proxy/<upstream-name>/<artifact-path>
```

## Development verification

Go is built and tested in Docker so no host Go installation is required:

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.24 go test -race ./...
```

The integration suite verifies live first-download streaming, same-artifact
request coalescing, cached Range responses, parallel distinct downloads, and
primary-to-backup failover.

## Documentation

- [Product decisions and implementation plan](docs/PROJECT_PLAN.md)
- [Streaming cache and architecture contract](docs/ARCHITECTURE.md)

## License

Homir is released under [Apache-2.0](LICENSE).
