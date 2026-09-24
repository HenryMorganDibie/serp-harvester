# Deployment

Two ways to run this on your own infrastructure, no Apify and no mandatory
hosted scraping API involved.

## Option 1: Docker Compose (fastest way to see it running)

```bash
cp deploy/.env.example deploy/.env
# edit deploy/.env: set POSTGRES_PASSWORD at minimum
docker compose -f deploy/docker-compose.yml up -d
```

This starts six containers, all on infrastructure you control:

| Service     | Port | Purpose |
|--------------|------|---------|
| `harvester`  | 9090 | The pipeline itself: queue consumer, workers, fetch, parse, sink |
| `api`        | 8080 | Job submission: `POST /jobs`, `GET /jobs/{run_id}` |
| `redis`      | 6379 | Distributed job queue (Redis Streams), AOF-persisted |
| `postgres`   | 5432 | Structured result storage (`serp_results` table) |
| `prometheus` | 9091 | Scrapes `harvester:9090/metrics` every 15s, evaluates alerting rules |
| `grafana`    | 3000 | Pre-provisioned dashboard reading from Prometheus |

All six have resource limits set (see `docker-compose.yml`'s `deploy.resources.limits`)
and restart automatically (`restart: unless-stopped`) if they crash. Redis,
Postgres, Prometheus, and Grafana data all persist across `docker compose down`/`up`
via named volumes — see [`BACKUP.md`](BACKUP.md) for what to actually back up
and how.

Open Grafana at `http://localhost:3000` (anonymous access is enabled for
this demo compose file — lock that down before using it for anything real)
and the "SERP Harvester" dashboard is already there: throughput, latency
percentiles, success rate, AI Overview hit rate, parser drift events, and
proxy ban rate. Prometheus's own alert states are at `http://localhost:9091/alerts`
(`deploy/prometheus/alerts.yml`) — visible there, not yet wired to
Alertmanager/Slack/PagerDuty, which is a client-specific routing decision.

`deploy/config.yaml` runs `mode: mock` with `sink_backend: postgres` out of
the box, so this is demonstrably real infrastructure wiring, not a claim —
bring it up and watch real (synthetic) data flow through the whole stack
into a real database. Switch to `mode: provider` and set `SERPAPI_KEY` (or
your provider's equivalent) in `deploy/.env` once you have a key. Submit
real jobs through the API instead of the static `queries` list in
`config.yaml`:

```bash
curl -X POST localhost:8080/jobs -d '{"queries": ["best laptops 2026"], "country": "US", "language": "en"}'
curl localhost:8080/jobs/<run_id_from_above>
```

Scale workers horizontally by running more `harvester` replicas pointed at
the same Redis stream with unique `redis_consumer` values — that's the
mechanism, not a hypothetical:

```bash
docker compose -f deploy/docker-compose.yml up -d --scale harvester=4
```

(Each replica needs a distinct `redis_consumer`; the current compose file
hardcodes one value in `config.yaml`, so scaling past 1 replica today means
either templating that value per-replica or moving it to an environment
variable — noted here rather than glossed over, since this is a config
change, not new code.)

## Option 2: systemd (bare-metal / VM)

```bash
go build -o bin/harvester ./cmd/harvester
sudo mkdir -p /opt/serp-harvester/bin
sudo cp bin/harvester /opt/serp-harvester/bin/
sudo cp -r internal/parser/testdata /opt/serp-harvester/internal/parser/testdata
sudo cp your-config.yaml /opt/serp-harvester/config.yaml
sudo cp deploy/systemd/serp-harvester.service /etc/systemd/system/
sudo useradd -r -s /usr/sbin/nologin harvester || true
sudo systemctl daemon-reload
sudo systemctl enable --now serp-harvester
```

Provider API keys go in `/etc/serp-harvester.env` (mode 600, not in git),
loaded via `EnvironmentFile=` in the unit — never baked into the binary or
committed anywhere.

## Health and metrics

Both deployment options expose, once `metrics_addr` is set in config:

```bash
curl localhost:9090/healthz   # 200 ok while the process is alive
curl localhost:9090/metrics   # Prometheus text format
```

See [`../RUNBOOK.md`](../RUNBOOK.md) for what to do when those metrics show
a problem.
