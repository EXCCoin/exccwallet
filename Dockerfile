FROM golang:alpine3.17 as builder
RUN apk add git ca-certificates upx gcc build-base --update --no-cache

WORKDIR /go/src/github.com/EXCCoin/exccwallet
COPY . .

ENV GO111MODULE=on
RUN go build -ldflags='-s -w -X main.appBuild=alpine:latest -extldflags "-static"' .

FROM alpine:latest

WORKDIR /app
COPY --from=builder /go/src/github.com/EXCCoin/exccwallet/exccwallet .

EXPOSE 9110
EXPOSE 9111
ENV DATA_DIR=/data
ENV CONFIG_FILE=/app/exccwallet.conf
CMD ["sh", "-c", "/app/exccd --appdata=${DATA_DIR} --configfile=${CONFIG_FILE}"]
 
