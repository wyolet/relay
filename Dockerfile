# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /relay ./cmd/relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /relay /relay
EXPOSE 8080
ENTRYPOINT ["/relay"]
