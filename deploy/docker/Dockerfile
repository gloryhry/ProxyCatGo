FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
COPY config ./config

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/proxycat ./cmd/proxycat

FROM alpine:3.20

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/proxycat /app/proxycat
COPY web /app/web
COPY config /app/config

RUN mkdir -p /app/logs

EXPOSE 1080 5000

ENTRYPOINT ["/app/proxycat"]
