FROM golang:1.17.1 AS builder
WORKDIR /build
# Separate dependency caching from compilation
COPY go.mod go.mod 
COPY go.sum so.sum
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o regproxy

FROM scratch
WORKDIR /
COPY --from=builder /build/regproxy regproxy
ENTRYPOINT ["/regproxy"]

