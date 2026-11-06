# syntax=docker/dockerfile:1
# LV24 Brain server image. Build from the repository root:
#   docker build -f deploy/Dockerfile -t lv24-brain-server:local .
FROM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=dev
ARG COMMIT=dev
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w \
      -X github.com/levapveeskela/lv24-brain/internal/version.Version=${VERSION} \
      -X github.com/levapveeskela/lv24-brain/internal/version.Commit=${COMMIT} \
      -X github.com/levapveeskela/lv24-brain/internal/version.Date=${DATE}" \
    -o /out/brain-server ./cmd/brain-server

FROM alpine:3.22@sha256:bfe7ceb9f09ec0566c48fb93356e45a47a00be5a1dd14fa5b909e7709c66b034

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 brain \
    && adduser -S -D -H -u 10001 -G brain brain

COPY --from=build /out/brain-server /usr/local/bin/brain-server

USER 10001:10001
EXPOSE 8080
ENV BRAIN_ADDR=0.0.0.0:8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/brain-server"]
