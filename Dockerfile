# syntax=docker/dockerfile:1

# Build on the host's architecture and cross-compile, which is much faster
# than emulating arm64/armv7 with QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/respool ./cmd/respool
RUN mkdir -p /out/uploads /out/data

# A static binary on distroless: no shell, no package manager, runs as an
# unprivileged user (uid 65532).
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/respool /respool
# Where uploads go when RESPOOL_UPLOAD_DIR=/uploads. Owned by the nonroot
# user, so a named volume mounted here starts out writable.
COPY --from=build --chown=65532:65532 /out/uploads /uploads
# Where the account is kept when RESPOOL_LOGIN=true, owned the same way.
COPY --from=build --chown=65532:65532 /out/data /data
ENV RESPOOL_MEDIA_DIR=/media \
    RESPOOL_DATA_DIR=/data \
    RESPOOL_ADDR=:8080
EXPOSE 8080
VOLUME ["/media"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["/respool", "healthcheck"]
ENTRYPOINT ["/respool"]

LABEL org.opencontainers.image.title="ReSpool" \
      org.opencontainers.image.description="Serve audiobooks, lectures and other long-form audio as podcast feeds" \
      org.opencontainers.image.licenses="MIT"
