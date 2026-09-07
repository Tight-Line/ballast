# Build stage. --platform=$BUILDPLATFORM keeps the Go toolchain on the native
# runner architecture and cross-compiles for $TARGETARCH (see GOARCH below), so
# an arm64 build doesn't run the whole builder under slow QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.1@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS builder
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
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
LABEL org.opencontainers.image.source=https://github.com/tight-line/ballast

WORKDIR /
COPY --from=builder /workspace/ballastd .
USER 65532:65532

ENTRYPOINT ["/ballastd"]
