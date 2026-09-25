# Test runner for `make integration` / deploy/docker-compose.test.yml: the
# official Playwright image (Chromium + system libraries) plus the Go
# toolchain and the Playwright driver, running `go test` against the
# compose stack's Redis and PostgreSQL. Also serves the fixture site for the
# end-to-end run. Not needed for the application itself.
#
# EXTRA_CA: optional CA bundle for building behind a TLS-inspecting proxy
# (BuildKit secret "extra_ca", see deploy/README.md). Unused when absent;
# never stored in the image.
FROM golang:1.26-bookworm AS go

FROM mcr.microsoft.com/playwright:v1.60.0-noble
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH \
    GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    PLAYWRIGHT_DRIVER_PATH=/opt/ms-playwright-go \
    SERP_HARVESTER_PLAYWRIGHT=1
COPY scripts/install-playwright.sh /tmp/install-playwright.sh
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export CURL_CA_BUNDLE=/run/secrets/extra_ca; fi; \
    PLAYWRIGHT_SKIP_BROWSER_INSTALL=1 sh /tmp/install-playwright.sh \
 && rm /tmp/install-playwright.sh
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export SSL_CERT_FILE=/run/secrets/extra_ca; fi; \
    go mod download
COPY . .
RUN go build ./... && go vet ./... && go build -o /usr/local/bin/fixtureserver ./tests/integration/fixtureserver
CMD ["go", "test", "-count=1", "./..."]
