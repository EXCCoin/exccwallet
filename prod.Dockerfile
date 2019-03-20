FROM golang:1.12.1-alpine3.9 as builder

RUN apk add git gcc g++ musl-dev --update --no-cache

WORKDIR /go/src/github.com/EXCCoin/exccwallet
COPY . .

ENV GO111MODULE=on
RUN go build -ldflags='-s -w -X main.appBuild=alpine3.9 -extldflags "-static"' .


FROM alpine:3.9

WORKDIR /app
COPY --from=builder /go/src/github.com/EXCCoin/exccwallet/exccwallet .

EXPOSE 9110
EXPOSE 9111
ENV DATA_DIR=/data
ENV CONFIG_FILE=/app/exccwallet.conf
CMD ["sh", "-c", "/app/exccwallet --appdata=${DATA_DIR} --configfile=${CONFIG_FILE}"]
