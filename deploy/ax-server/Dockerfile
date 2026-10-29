FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY ax-server /usr/local/bin/ax-server
ENTRYPOINT ["/usr/local/bin/ax-server"]
