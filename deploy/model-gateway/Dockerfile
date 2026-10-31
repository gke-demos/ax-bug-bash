FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY bin/linux_amd64/model-gateway /usr/local/bin/model-gateway
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/model-gateway"]
