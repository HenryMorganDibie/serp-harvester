# Backup and restore

What's persisted, what isn't, and how to back up each piece. This complements
[`PRODUCTION_READINESS.md` §15 "Disaster recovery"](../PRODUCTION_READINESS.md#15-disaster-recovery),
which is the honest summary; this file is the how-to.

## What's persisted (and where)

`deploy/docker-compose.yml` defines four named volumes:

| Volume            | Contains                                    | Loss impact |
|--------------------|----------------------------------------------|--------------|
| `postgres-data`     | All harvested results (`serp_results` table) | **Data loss** — this is the durable record |
| `redis-data`        | The job queue (AOF-persisted)                 | In-flight/unprocessed jobs lost; already-completed work in Postgres is unaffected |
| `prometheus-data`   | Metrics history                               | Historical dashboards/alerts context lost; not operationally critical |
| `grafana-data`      | Dashboard edits, users, alert config          | Re-provisioned dashboards survive (they're in `deploy/grafana/provisioning/`, not this volume) — only manual UI changes are at risk |

Prioritize backing up `postgres-data`. `redis-data` matters only if you
can't tolerate re-submitting in-flight jobs after a crash.

## Postgres backup

```bash
# Logical backup (portable, works across Postgres versions):
docker compose -f deploy/docker-compose.yml exec postgres \
  pg_dump -U harvester serp_harvester | gzip > backup-$(date +%Y%m%d-%H%M%S).sql.gz

# Restore into a fresh instance:
gunzip -c backup-20261001-120000.sql.gz | \
  docker compose -f deploy/docker-compose.yml exec -T postgres psql -U harvester -d serp_harvester
```

For a running production instance, schedule the `pg_dump` line via cron (or
your platform's job scheduler) and ship the resulting file off-host —
`docker exec` output alone doesn't survive host loss. Volume-level snapshots
(e.g. cloud provider EBS/disk snapshots targeting wherever
`postgres-data` is mounted) are the alternative if you'd rather not run
`pg_dump` on a schedule; neither is configured automatically by this repo,
since the choice depends on where you're actually hosting.

## Redis backup

The `appendonly yes` flag in `docker-compose.yml` means Redis persists the
queue to disk continuously (AOF), so a container restart doesn't lose
unprocessed jobs on its own. To back up the AOF file itself:

```bash
docker compose -f deploy/docker-compose.yml exec redis redis-cli BGSAVE
docker cp $(docker compose -f deploy/docker-compose.yml ps -q redis):/data/dump.rdb ./redis-backup-$(date +%Y%m%d).rdb
```

In practice: if Redis data is lost, the fix is usually "re-submit the
affected runs via `POST /jobs`," not "restore from backup" — the queue is
transient work-in-progress, not the durable record (Postgres is). Treat
Redis backup as optional unless your workload can't tolerate re-submission.

## Restore verification

After any restore, confirm row counts and a spot-check query against
Postgres before resuming traffic:

```sql
SELECT count(*), max(fetched_at) FROM serp_results;
```

Compare against what you expected from before the incident. This repo
doesn't automate that comparison — it's a manual step, called out here
rather than assumed away.
