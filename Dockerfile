FROM golang:1.26.5-alpine AS builder
ADD . /go/src/github.com/guru-docker/docker-secret-gcloud
WORKDIR /go/src/github.com/guru-docker/docker-secret-gcloud

RUN apk add --no-cache --virtual .build-deps gcc libc-dev
RUN go install --ldflags '-extldflags "-static"'
RUN apk del .build-deps

CMD ["/go/bin/docker-secret-gcloud"]


FROM alpine

# The plugin talks TLS to secretmanager.googleapis.com, so the rootfs needs a
# trust store of its own; /run/gcloud is where an optional credentials file is
# bind-mounted from the host.
RUN apk update && apk add --no-cache ca-certificates
RUN mkdir -p /run/docker/plugins /run/gcloud

COPY --from=builder /go/bin/docker-secret-gcloud .
CMD ["docker-secret-gcloud"]
