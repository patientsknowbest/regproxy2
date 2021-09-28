FROM golang:1.17.1 AS builder
WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 go build -o regproxy
FROM scratch
WORKDIR /
COPY --from=builder /build/regproxy regproxy
ENTRYPOINT ["/regproxy"]

