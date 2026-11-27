FROM golang:1.26-alpine AS builder

ENV CGO_CFLAGS="-D_LARGEFILE64_SOURCE"

# GOPROXY chain mirrors agio-cloud docker/juicefs/Dockerfile (ci c1d16737) and
# agio-platform docker/Dockerfile (ci 71166a028): proxy.golang.org intermittently
# blackholes long transfers on VK egress (ESTABLISHED TCP, no data — go has no
# HTTP timeout and hangs forever); Huawei/Aliyun mirrors measured ~2/1.3MB/s.
ENV GOPROXY=https://repo.huaweicloud.com/repository/goproxy,https://mirrors.aliyun.com/goproxy,direct

RUN  \
    apk add --no-cache git make gcc musl-dev sqlite && \
    rm -rf /var/cache/apk/*

COPY . /home/juicefs
WORKDIR /home/juicefs
RUN make juicefs

FROM golang:1.26-alpine

EXPOSE 9000

COPY --from=builder /home/juicefs/juicefs /usr/bin/juicefs

ENTRYPOINT ["/usr/bin/juicefs"]

CMD ["gateway"]
