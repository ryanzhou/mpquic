# syntax=docker/dockerfile:1
# The build stage always runs on the build host's architecture and
# cross-compiles (fast multi-arch builds without emulating the Go toolchain).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
RUN export GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 \
 && if [ "$TARGETARCH" = "arm" ]; then export GOARM="${TARGETVARIANT#v}"; fi \
 && go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mpquic ./cmd/mpquic \
 && go build -trimpath -ldflags "-s -w" -o /out/mpbench ./cmd/mpbench

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
 && adduser -D -H -u 10001 mpquic
COPY --from=build /out/mpquic /usr/local/bin/mpquic
COPY --from=build /out/mpbench /usr/local/bin/mpbench
# shadowsocks-style command names
RUN for n in ss-local ss-server ss-tunnel mpquic-relay; do ln -s mpquic /usr/local/bin/$n; done
USER mpquic
EXPOSE 8388/udp 1080/tcp
ENTRYPOINT ["mpquic"]
CMD ["help"]
