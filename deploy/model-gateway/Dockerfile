FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY model-gateway /usr/local/bin/model-gateway
ENTRYPOINT ["/usr/local/bin/model-gateway"]
