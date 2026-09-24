# Multi-stage build: compile the harvester binary, then run it in a minimal
# image. No Google/provider credentials are baked in — those are supplied
# at runtime via environment variables and a mounted config file.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
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
