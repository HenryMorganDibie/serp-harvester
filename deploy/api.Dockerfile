# Builds the job-submission API (cmd/api), separate from the harvester
# worker image (Dockerfile) since they're independently scaled: typically
# one or a few api replicas behind a load balancer, vs. many harvester
# replicas consuming the queue.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/api ./cmd/api

FROM alpine:3.20
RUN adduser -D -u 10001 apiuser
WORKDIR /app
COPY --from=build /out/api /app/api
USER apiuser
ENTRYPOINT ["/app/api"]
