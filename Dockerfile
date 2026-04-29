# syntax=docker/dockerfile:1.7

############################
# Stage 1: build
############################
FROM golang:1.23-alpine AS build

WORKDIR /src

# Cache module downloads
COPY go.mod go.sum* ./
RUN go mod download || true

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 \
    go build -trimpath \
      -ldflags "-s -w \
        -X 'github.com/PhantomMatthew/nextcloud-go/internal/observability.Version=${VERSION}' \
        -X 'github.com/PhantomMatthew/nextcloud-go/internal/observability.Commit=${COMMIT}' \
        -X 'github.com/PhantomMatthew/nextcloud-go/internal/observability.BuildDate=${BUILD_DATE}'" \
      -o /out/ncgo ./cmd/ncgo

############################
# Stage 2: runtime
############################
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="ncgo"
LABEL org.opencontainers.image.description="Nextcloud-compatible server in Go"
LABEL org.opencontainers.image.source="https://github.com/PhantomMatthew/nextcloud-go"
LABEL org.opencontainers.image.licenses="AGPL-3.0-or-later"

USER nonroot:nonroot
WORKDIR /app

COPY --from=build /out/ncgo /app/ncgo

EXPOSE 8080
ENTRYPOINT ["/app/ncgo"]
CMD ["serve"]
