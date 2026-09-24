# Harvester image for `mode: playwright` (headless Chromium). Kept separate
# from deploy/Dockerfile because Chromium needs glibc and ~40 system
# libraries, which roughly adds 600MB to an image the mock, live and
# provider modes don't need.
#
# The Playwright driver and Chromium are installed at build time with the
# installer compiled from the playwright-go version pinned in go.mod, so the
# driver, the browser build and the Go bindings always match. Nothing is
# downloaded at runtime. No credentials are baked in.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/harvester ./cmd/harvester \
 && CGO_ENABLED=0 go build -o /out/playwright github.com/playwright-community/playwright-go/cmd/playwright

# Debian 12 is a Playwright-supported distribution for `--with-deps`.
FROM debian:bookworm-slim
ENV PLAYWRIGHT_DRIVER_PATH=/opt/ms-playwright-go \
    PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
COPY --from=build /out/playwright /usr/local/bin/playwright
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && playwright install --with-deps chromium \
 && rm -rf /var/lib/apt/lists/* \
 && chmod -R a+rX /opt/ms-playwright /opt/ms-playwright-go \
 && useradd --uid 10001 --create-home harvester
WORKDIR /app
COPY --from=build /out/harvester /app/harvester
COPY internal/parser/testdata /app/internal/parser/testdata
USER harvester
ENTRYPOINT ["/app/harvester"]
CMD ["-config", "/app/config.yaml"]
