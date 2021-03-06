# Build Geth in a stock Go builder container
FROM golang:1.14-alpine as builder

RUN apk add --no-cache make gcc musl-dev linux-headers git

# next 2 lines helping utilize docker cache
COPY go.mod go.sum /go-ethereum/
RUN cd /go-ethereum && go mod download

ADD . /go-ethereum
RUN cd /go-ethereum && make geth

# Pull Geth into a second stage deploy alpine container
FROM alpine:latest

RUN apk add --no-cache ca-certificates
COPY --from=builder /go-ethereum/build/bin/geth /usr/local/bin/

EXPOSE 8545 8546 8547 30303 30303/udp
ENTRYPOINT ["geth"]
