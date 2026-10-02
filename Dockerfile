# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /out/wingman-telegram .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 telegram \
    && adduser -D -u 10001 -G telegram telegram \
    && mkdir /data \
    && chown telegram:telegram /data
COPY --from=build /out/wingman-telegram /usr/local/bin/wingman-telegram
ENV XDG_STATE_HOME=/data
USER 10001:10001
ENTRYPOINT ["wingman-telegram"]
