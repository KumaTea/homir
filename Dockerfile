FROM --platform=$BUILDPLATFORM golang:1.24 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/homir ./cmd/homir

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/homir /usr/local/bin/homir
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/homir"]
CMD ["-config", "/etc/homir/homir.yaml"]
