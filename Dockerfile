# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pricetag ./cmd/pricetag

FROM alpine:3.20
RUN adduser -D -u 10001 pricetag && mkdir /data && chown pricetag /data
COPY --from=build /out/pricetag /usr/local/bin/pricetag
USER pricetag
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["pricetag"]
CMD ["serve", "-addr", ":8080", "-data", "/data"]
