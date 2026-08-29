# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build

RUN apk add --no-cache ca-certificates git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/controlplane ./cmd/controlplane

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 65532 opencluster \
    && adduser -S -D -H -u 65532 -G opencluster opencluster
COPY --from=build /out/controlplane /usr/local/bin/controlplane
USER 65532:65532
EXPOSE 8080 8443
ENTRYPOINT ["/usr/local/bin/controlplane"]
