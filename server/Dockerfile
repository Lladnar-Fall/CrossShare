FROM golang:1.21-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /app
# Build context is the repo root (see railway.json dockerfilePath).
COPY server/go.mod server/go.sum* ./
RUN go mod download || true

COPY server/*.go ./
RUN CGO_ENABLED=1 go build -o server .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/server .
# Mobile PWA served by the Go server at / (see resolveWebDir in main.go).
COPY web ./web
EXPOSE 8080
ENV PORT=8080
ENV DATA_DIR=/data
ENV WEB_DIR=/app/web

CMD ["./server"]
