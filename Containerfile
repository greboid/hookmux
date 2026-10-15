FROM git.mouse-lake.ts.net/containers/golang:latest AS builder

ENV CGO_ENABLED=0

WORKDIR /src
COPY . .

RUN mkdir -p /rootfs/data && \  
    chown -R 65532:65532 /rootfs && \
    go build -o /rootfs/hookmux .

FROM ghcr.io/greboid/dockerbase/nonroot:1.20261007.0
COPY --from=builder /rootfs /
EXPOSE 8080  
WORKDIR /data
ENTRYPOINT ["/hookmux"]
