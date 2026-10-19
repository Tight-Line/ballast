# Build stage. --platform=$BUILDPLATFORM keeps the Go toolchain on the native
# runner architecture and cross-compiles for $TARGETARCH (see GOARCH below), so
# an arm64 build doesn't run the whole builder under slow QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH}" go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o ballastd ./cmd/ballastd

# Runtime stage — distroless for minimal attack surface
FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
LABEL org.opencontainers.image.source=https://github.com/tight-line/ballast

WORKDIR /
COPY --from=builder /workspace/ballastd .
USER 65532:65532

ENTRYPOINT ["/ballastd"]
