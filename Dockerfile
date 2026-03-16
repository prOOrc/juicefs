FROM golang:1.23-alpine AS builder

ENV CGO_CFLAGS="-D_LARGEFILE64_SOURCE"

RUN  \
    apk add --no-cache git make gcc musl-dev sqlite && \
    rm -rf /var/cache/apk/*

COPY . /home/juicefs
WORKDIR /home/juicefs
RUN make juicefs

FROM golang:1.23-alpine

EXPOSE 9000

COPY --from=builder /home/juicefs/juicefs /usr/bin/juicefs

ENTRYPOINT ["/usr/bin/juicefs"]

CMD ["gateway"]
