FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go tool templ generate && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /runwall ./cmd/runwall

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /runwall /runwall
ENV LISTEN_ADDR=:8080 DB_PATH=/data/runwall.db NOTIFIER=log
EXPOSE 8080
VOLUME /data
USER nonroot
ENTRYPOINT ["/runwall"]
