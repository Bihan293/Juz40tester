# ---------- Build stage ----------
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Download dependencies first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /app/bot ./cmd/bot

# ---------- Runtime stage ----------
FROM alpine:3.20

RUN adduser -D -u 10001 botuser
WORKDIR /app

COPY --from=builder /app/bot /app/bot

USER botuser

ENV PORT=8080
EXPOSE 8080

CMD ["/app/bot"]
