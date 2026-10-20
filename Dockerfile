# syntax=docker/dockerfile:1.7
# Multi-stage: build with the full toolchain, ship a distroless image holding
# only the static binary. modernc.org/sqlite is pure Go, so CGO stays off.

FROM golang:1.24-alpine AS build
WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/recond ./cmd/recond \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/recond /recond
COPY --from=build --chown=nonroot:nonroot /out/data /data
USER nonroot:nonroot
# The binary defaults to 127.0.0.1; the image binds all interfaces so that a
# published port reaches the listener.
ENV RECOND_DB=/data/recond.db \
    RECOND_ADDR=0.0.0.0:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/recond"]
CMD ["serve"]
