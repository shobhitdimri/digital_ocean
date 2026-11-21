# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && mkdir -p /rootfs/data /rootfs/logs

# distroless has no shell and no package manager; the process runs as the
# non-root user 65532 and owns only the two directories it writes to.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
COPY --from=build --chown=65532:65532 /rootfs/ /

ENV PORT=8080 \
    DATA_DIR=/data \
    LOG_DIR=/logs

VOLUME ["/data", "/logs"]
EXPOSE 8080

# Exec form: the Go process is PID 1 and receives SIGTERM directly, which the
# graceful shutdown depends on. A shell wrapper would swallow the signal.
ENTRYPOINT ["/server"]
