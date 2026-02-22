FROM golang:1.21-alpine as builder
WORKDIR /app
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o telegram-llm-bot ./cmd

FROM alpine:latest
RUN apk --no-cache add ca-certificates
RUN adduser -D -u 1000 appuser
USER appuser
WORKDIR /app
RUN mkdir -p data/store
COPY --from=builder /app/telegram-llm-bot .
CMD ["./telegram-llm-bot"]
