# Builds cmd/palimpsestd. Build context must be the repo root (see
# docker-compose.yml's build.context: ..), since the binary depends on
# internal/* and pkg/*.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/
RUN CGO_ENABLED=0 go build -trimpath -o /out/palimpsestd ./cmd/palimpsestd

FROM alpine:3.20
RUN adduser -D -H palimpsest
COPY --from=build /out/palimpsestd /usr/local/bin/palimpsestd
USER palimpsest
ENTRYPOINT ["palimpsestd"]
