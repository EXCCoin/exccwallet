FROM golang:1.26-alpine3.23 AS builder
RUN apk add git ca-certificates upx gcc build-base --update --no-cache

WORKDIR /go/src/github.com/EXCCoin/exccwallet
COPY . .

ENV GO111MODULE=on
ARG BUILD_PRERELEASE=pre
ARG BUILD_METADATA=docker
RUN go build -buildvcs=false -trimpath \
    -ldflags="-s -w -X github.com/EXCCoin/exccwallet/v2/version.PreRelease=${BUILD_PRERELEASE} -X github.com/EXCCoin/exccwallet/v2/version.BuildMetadata=${BUILD_METADATA} -extldflags '-static'" .

FROM alpine:3.23

WORKDIR /app
COPY --from=builder /go/src/github.com/EXCCoin/exccwallet/exccwallet .

EXPOSE 9110
EXPOSE 9111
ENV DATA_DIR=/data
ENV CONFIG_FILE=/app/exccwallet.conf
CMD ["sh", "-c", "/app/exccwallet --appdata=${DATA_DIR} --configfile=${CONFIG_FILE}"]
