# Release checklist

Use this checklist before publishing a Homir release.

1. Run `docker run --rm -v "$PWD:/src" -w /src golang:1.24 go test -race ./...`.
2. Build the native image: `docker build -t homir:release-smoke .`.
3. Validate deployment configuration: `HOMIR_ADMIN_PASSWORD=test docker compose config`.
4. Run client-container smoke tests against a non-root Homir container:
   Debian `apt-get update` plus a `.deb` download, Alpine `apk update` plus an
   `.apk` fetch, and `pip download` from the local Simple endpoint.
5. Update release notes and create a signed version tag, for example:
   `git tag -a v0.1.0 -m "Homir v0.1.0"` and `git push origin v0.1.0`.

Pushing a `v*` tag triggers the GitHub workflow to publish the amd64 and arm64
manifest to `ghcr.io/kumatea/homir`.

## Optional Docker Hub publication

Docker Hub publication is intentionally separate from the GitHub release
workflow. After explicitly choosing a release tag and logging in, publish both
architectures manually:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t kumatea/homir:latest -t kumatea/homir:v0.1.0 --push .
```

Replace `v0.1.0` with the actual release version. This command pushes only when
you run it; local Docker Hub login does not cause Homir to publish automatically.
