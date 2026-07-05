# Builds a real OTel Collector distribution containing this repo's
# csresidual processor, via ocb (see demo/otelcol-builder.yaml). Optional
# `otel` compose profile only — best-effort, see demo/README.md.
FROM golang:1.22-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
COPY pkg/ ./pkg/
COPY internal/ ./internal/
COPY cmd/ ./cmd/
COPY otel/ ./otel/

RUN go install go.opentelemetry.io/collector/cmd/builder@v0.155.0

WORKDIR /build
COPY demo/otelcol-builder.yaml ./builder-config.yaml
RUN builder --config builder-config.yaml

FROM alpine:3.20
COPY --from=build /build/dist/palimpsest-otelcol /usr/local/bin/palimpsest-otelcol
ENTRYPOINT ["palimpsest-otelcol"]
