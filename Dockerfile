# Proxy image: `docker build -t azemu/proxy:dev .`
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/azemu ./cmd/azemu

FROM alpine:3.21
# wget: compose healthcheck against :8081/healthz
RUN apk add --no-cache ca-certificates wget
COPY --from=build /out/azemu /usr/local/bin/azemu
ENTRYPOINT ["azemu", "proxy"]
