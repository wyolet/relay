# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS builder
# curl and tar are needed for make ui-fetch (alpine ships tar; curl is added here)
RUN apk add --no-cache curl make
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Fetch the pinned UI bundle before compiling (UI_VERSION inherits from Makefile default)
RUN make ui-fetch
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /relay ./cmd/relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /relay /relay
EXPOSE 8080
ENTRYPOINT ["/relay"]
