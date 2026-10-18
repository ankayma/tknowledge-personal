# The Personal store as one image: tk-api, tk-mcp and tk-proxy behind one
# port, with the tk CLI. Postgres runs beside it (docker-compose.yml). The
# store's signing key is made on first start in /data, never in the image.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/ ./cmd/tk-api ./cmd/tk-mcp ./cmd/tk ./cmd/tk-proxy

FROM alpine:3.21
RUN apk add --no-cache ca-certificates postgresql16-client tzdata
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY config.yaml /app/config.yaml
COPY deploy/personal/entrypoint.sh /usr/local/bin/tk-personal
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["tk-personal"]
