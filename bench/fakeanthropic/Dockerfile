# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /fakeanthropic ./bench/fakeanthropic/cmd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /fakeanthropic /fakeanthropic
COPY --from=builder /src/bench/fakeanthropic/testdata/session.jsonl /session.jsonl
EXPOSE 9999
ENTRYPOINT ["/fakeanthropic", "--session", "/session.jsonl", "--listen", ":9999"]
