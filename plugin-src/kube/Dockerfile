# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine AS dependencies
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

FROM dependencies AS wuko
ARG TARGETOS=linux
ARG TARGETARCH
ARG WUKO_VERSION=v0.18.0
RUN cd "$(go env GOMODCACHE)/github.com/up2jj/wuko@${WUKO_VERSION}" && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/wuko .

FROM dependencies AS supervisor
ARG TARGETOS=linux
ARG TARGETARCH
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/wuko-supervisor ./cmd/worker

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && addgroup -g 10001 worker && adduser -D -u 10001 -G worker worker \
 && mkdir -p /work /var/run/wuko/runtime && chown -R 10001:10001 /work /var/run/wuko/runtime
COPY --from=supervisor /out/wuko-supervisor /usr/local/bin/wuko-supervisor
COPY --from=wuko /out/wuko /usr/local/bin/wuko
WORKDIR /work
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/wuko-supervisor"]
