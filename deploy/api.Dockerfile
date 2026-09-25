# Builds the job-submission API (cmd/api), separate from the harvester
# worker image (Dockerfile) since they're independently scaled: typically
# one or a few api replicas behind a load balancer, vs. many harvester
# replicas consuming the queue.
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
RUN CGO_ENABLED=0 go build -o /out/api ./cmd/api

FROM alpine:3.20
RUN adduser -D -u 10001 apiuser
WORKDIR /app
COPY --from=build /out/api /app/api
USER apiuser
ENTRYPOINT ["/app/api"]
