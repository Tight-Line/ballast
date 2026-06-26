# Build stage
FROM golang:1.26 AS builder
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
FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.source=https://github.com/tight-line/ballast

WORKDIR /
COPY --from=builder /workspace/ballastd .
USER 65532:65532

ENTRYPOINT ["/ballastd"]
