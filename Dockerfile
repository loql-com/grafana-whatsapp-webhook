
# Base image for development
FROM golang:1.26-alpine3.24 AS baseimage

WORKDIR /app

RUN apk add --no-cache make gcc git sqlite-dev musl-dev

COPY ./go.mod ./go.sum /app/

RUN go mod download

# Base image for development
FROM baseimage AS dev

WORKDIR /app

RUN go install github.com/air-verse/air@latest

CMD ["air"]

# Builder image
FROM baseimage AS builder

COPY pkg/ /app/pkg/
COPY main.go /app/

RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o main .

# Production image
FROM alpine:3.24 AS prod

WORKDIR /app/

RUN apk add --no-cache sqlite

COPY --from=builder /app/main .

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q --spider http://127.0.0.1:8080/healthy || exit 1

CMD ["./main"]
