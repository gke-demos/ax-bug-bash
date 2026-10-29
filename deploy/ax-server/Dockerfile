FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY bin/linux_amd64/ax-server /usr/local/bin/ax-server
ENTRYPOINT ["/usr/local/bin/ax-server"]
