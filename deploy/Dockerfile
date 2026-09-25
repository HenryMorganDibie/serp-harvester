# Multi-stage build: compile the harvester binary, then run it in a minimal
# image. No Google/provider credentials are baked in — those are supplied
# at runtime via environment variables and a mounted config file.
# EXTRA_CA: optional CA bundle for building behind a TLS-inspecting proxy
# (BuildKit secret "extra_ca", see deploy/README.md). Unused when absent;
# never stored in the image.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export SSL_CERT_FILE=/run/secrets/extra_ca; fi; \
    go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/harvester ./cmd/harvester

FROM alpine:3.20
RUN adduser -D -u 10001 harvester
WORKDIR /app
COPY --from=build /out/harvester /app/harvester
COPY internal/parser/testdata /app/internal/parser/testdata
USER harvester
ENTRYPOINT ["/app/harvester"]
CMD ["-config", "/app/config.yaml"]
