# Harvester image for `mode: playwright` (headless Chromium). Kept separate
# from deploy/Dockerfile because Chromium and its system libraries add well
# over 1GB to an image the mock, live and provider modes don't need.
#
# Based on Microsoft's official Playwright image for the exact driver
# version playwright-go in go.mod expects, which already contains Chromium,
# its system libraries and Node.js. The build therefore runs no apt and
# downloads no browsers; it only adds the Playwright driver (a checksum-
# pinned npm package, see scripts/install-playwright.sh). A unit test keeps
# this tag, the script and go.mod on the same version. Nothing is downloaded
# at runtime. No credentials are baked in.
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

FROM mcr.microsoft.com/playwright:v1.60.0-noble
# The base image sets PLAYWRIGHT_BROWSERS_PATH=/ms-playwright (browsers
# preinstalled); the driver goes next to it.
ENV PLAYWRIGHT_DRIVER_PATH=/opt/ms-playwright-go
COPY scripts/install-playwright.sh /tmp/install-playwright.sh
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export CURL_CA_BUNDLE=/run/secrets/extra_ca; fi; \
    PLAYWRIGHT_SKIP_BROWSER_INSTALL=1 sh /tmp/install-playwright.sh \
 && rm /tmp/install-playwright.sh \
 && chmod -R a+rX /opt/ms-playwright-go
WORKDIR /app
COPY --from=build /out/harvester /app/harvester
COPY internal/parser/testdata /app/internal/parser/testdata
# Unprivileged user provided by the base image.
USER pwuser
ENTRYPOINT ["/app/harvester"]
CMD ["-config", "/app/config.yaml"]
