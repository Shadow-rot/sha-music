FROM golang:1.21-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux go build -a -ldflags '-linkmode external -extldflags "-static"' -o sha-music-bot .

FROM alpine:latest

RUN apk --no-cache add \
    ca-certificates \
    ffmpeg \
    python3 \
    py3-pip \
    opus \
    opus-dev

WORKDIR /root/

COPY --from=builder /app/sha-music-bot .
COPY config.yml .

RUN pip3 install --no-cache-dir yt-dlp spotdl

RUN mkdir -p downloads

CMD ["./sha-music-bot"]