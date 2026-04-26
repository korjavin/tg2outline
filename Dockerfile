FROM golang:1.21-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o tg2outline .

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

COPY --from=builder /app/tg2outline .

CMD ["./tg2outline"]
