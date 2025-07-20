FROM golang:1.24.5 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o myapp ./...

FROM scratch

COPY --from=builder /app/myapp /myapp

ENV CONFIG=/app/config.toml

ENTRYPOINT ["/myapp"]