# check=error=true
FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY *.go ./
# embedded by main.go (//go:embed) as the first-boot starter config
COPY config.example.yaml ./
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /seadex-scout .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

COPY --chmod=755 --from=builder /seadex-scout /seadex-scout
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD ["/seadex-scout", "health"]
ENTRYPOINT ["/seadex-scout"]
