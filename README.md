# Homir

**Homir** (Home Mirror) is a self-hosted, on-demand package mirror. It acts as
a compatible package proxy: it immediately streams an uncached artifact from a
configured upstream to the requesting client while saving it locally. Later
requests are served from the local cache.

Unlike a public full mirror, Homir retains only artifacts that clients actually
download. A background manager refreshes active packages and releases disk space
when cached content becomes inactive.

> **Project status:** planning. This repository records the agreed product and
> technical direction before implementation begins.

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

## Documentation

- [Product decisions and implementation plan](docs/PROJECT_PLAN.md)
- [Streaming cache and architecture contract](docs/ARCHITECTURE.md)

## License

Homir is planned for release under Apache-2.0. The full license file will be
added with the initial code scaffold.
