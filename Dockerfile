FROM golang:1.10

ENV TERM linux

# update base distro & install build tooling
ENV DEBIAN_FRONTEND noninteractive

RUN apt-get update && \
    apt-get install -qy rsync

RUN mkdir -p /.exccwallet/
RUN mkdir -p /.exccwallet/logs/
RUN mkdir -p /.exccwallet/logs/mainnet/
RUN mkdir -p /.exccwallet/mainnet/

COPY .dockerfiles/logs/mainnet/exccwallet.log /.exccwallet/logs/mainnet/exccwallet.log
COPY .dockerfiles/mainnet/wallet.db /.exccwallet/mainnet/wallet.db
COPY .dockerfiles/rpc.cert /.exccwallet/rpc.cert
COPY .dockerfiles/rpc.key /.exccwallet/rpc.key

#Get deps
ENV DEP_TAG v0.4.1

RUN go get -v github.com/golang/dep && \
    cd /go/src/github.com/golang/dep && \
    git checkout $DEP_TAG && \
    go install ./...

#Get exccwallet
ENV BRANCH master

RUN go get -v github.com/EXCCoin/exccwallet && \
    cd /go/src/github.com/EXCCoin/exccwallet && \
    git checkout $BRANCH && \
    dep ensure && \
    go install ./...
