# builder image
FROM golang:1.14-alpine3.11 as builder

RUN apk add --no-cache make gcc git musl-dev linux-headers
WORKDIR /go/src/github.com/wanchain/go-wanchain
COPY . .
RUN make gwan

# final image
FROM alpine:3.11

RUN apk --no-cache add ca-certificates dumb-init
COPY --from=builder /go/src/github.com/wanchain/go-wanchain/build/bin/gwan /bin/gwan

USER 65534
ENTRYPOINT ["dumb-init", "--", "/bin/gwan"]
