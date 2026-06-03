FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY main.go ./
RUN go build -o compute .

FROM alpine:3.19

RUN addgroup -S app && adduser -S app -G app
USER app

WORKDIR /app
COPY --from=builder /app/compute .

EXPOSE 8080
ENTRYPOINT ["./compute"]
