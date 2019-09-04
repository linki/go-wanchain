# builder image
FROM golang:1.13-alpine3.10 as builder

RUN apk add --no-cache make gcc git musl-dev linux-headers
WORKDIR /go/src/github.com/wanchain/go-wanchain
COPY . .
RUN make release

# final image
FROM alpine:3.10
MAINTAINER Linki <linki+docker.com@posteo.de>

RUN apk --no-cache add ca-certificates dumb-init
COPY --from=builder /go/src/github.com/wanchain/go-wanchain/build/bin/gwan-linux-amd64 /bin/gwan

USER 65534
ENTRYPOINT ["dumb-init", "--", "/bin/gwan"]
