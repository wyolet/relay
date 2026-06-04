FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /mock .
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /mock /mock
EXPOSE 9990
ENTRYPOINT ["/mock"]
