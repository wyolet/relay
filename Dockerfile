# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS builder
RUN apk add --no-cache make
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# UI is fetched separately (relay-ui release tarball, currently a private repo).
# In dev the UI runs via vite on its own port. The embed source dir must exist
# for //go:embed to compile; an empty dist is fine — runtime serves no UI.
RUN mkdir -p cmd/relay/web/dist && touch cmd/relay/web/dist/.gitkeep
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /relay ./cmd/relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /relay /relay
EXPOSE 8080
ENTRYPOINT ["/relay"]
