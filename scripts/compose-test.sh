#!/bin/sh
# Reproducible integration / end-to-end runs in Docker, isolated from any
# stack you run yourself (own project name, no published ports, throwaway
# passwords, volumes removed afterwards).
#
#   scripts/compose-test.sh integration   go test ./... with real Redis,
#                                         PostgreSQL and Chromium
#   scripts/compose-test.sh e2e           the harvester-playwright image in
#                                         hybrid mode against the fixture site
#   scripts/compose-test.sh all           both
#
# Set EXTRA_CA_FILE to a CA bundle when building behind a TLS-inspecting
# proxy. KEEP=1 leaves the stack up for inspection.
set -eu
cd "$(dirname "$0")/.."

export COMPOSE_PROJECT_NAME=serp-harvester-test
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-compose-test-only}"
export GRAFANA_ADMIN_PASSWORD="${GRAFANA_ADMIN_PASSWORD:-compose-test-only}"
DC="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.test.yml --profile test --profile e2e"

cleanup() { [ "${KEEP:-}" = "1" ] || $DC down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

integration() {
	echo "== integration: go test ./... against compose Redis + PostgreSQL + Chromium"
	$DC build integration
	out=$($DC run --rm integration go test -count=1 -v ./... 2>&1) || {
		echo "$out" | grep -E '^(--- FAIL|FAIL|panic)|_test.go:' >&2
		exit 1
	}
	echo "$out" | grep -E '^(ok|FAIL)'
	# The browser, PostgreSQL and pipeline tests skip without their
	# services; here they must all run. (tests/live skips by design.)
	skipped=$(echo "$out" | grep -E '^--- SKIP: Test(Pipeline|PostgresSink|Playwright|Pool_Playwright)' || true)
	if [ -n "$skipped" ]; then
		echo "FAIL: gated tests were skipped:" >&2
		echo "$skipped" >&2
		exit 1
	fi
	echo "PASS: $(echo "$out" | grep -c '^--- PASS') tests passed, none of the gated ones skipped"
}

e2e() {
	echo "== e2e: harvester-playwright (hybrid) -> fixture site via scripted proxies"
	$DC build harvester-playwright fixture
	$DC up -d --wait redis postgres fixture harvester-playwright

	run_id="e2e-$(date +%s)"
	jobs=4
	i=0
	while [ $i -lt $jobs ]; do
		$DC exec -T redis redis-cli XADD serp-harvester:queries '*' \
			query "e2e query $i" run_id "$run_id" locale US-en device desktop >/dev/null
		i=$((i + 1))
	done

	count=0
	deadline=$(($(date +%s) + 120))
	while [ "$(date +%s)" -lt $deadline ]; do
		count=$($DC exec -T postgres psql -U harvester -d serp_harvester -tAc \
			"SELECT count(*) FROM serp_results WHERE run_id = '$run_id' AND jsonb_array_length(organic) = 3")
		[ "$count" -ge $jobs ] && break
		sleep 2
	done

	echo "-- metrics from the running container:"
	metrics=$($DC exec -T harvester-playwright bash -c \
		"exec 3<>/dev/tcp/127.0.0.1/9090 && printf 'GET /metrics HTTP/1.0\r\n\r\n' >&3 && cat <&3")
	echo "$metrics" | grep -E '^serp_harvester_(success_total|fetch_outcomes_total|proxy_cooldowns_total|fetch_failovers_total|proxies_(available|cooling)|browser_launches_total)[{ ]' || true

	if [ "$count" -lt $jobs ]; then
		echo "FAIL: $count/$jobs fully parsed rows for $run_id in PostgreSQL" >&2
		$DC logs --tail 50 harvester-playwright >&2
		exit 1
	fi
	echo "$metrics" | grep -q 'serp_harvester_fetch_outcomes_total{outcome="captcha"}' ||
		{ echo "FAIL: expected the CAPTCHA'd proxy to be detected" >&2; exit 1; }
	echo "$metrics" | grep -Eq '^serp_harvester_fetch_failovers_total [1-9]' ||
		{ echo "FAIL: expected HTTP -> browser failovers" >&2; exit 1; }
	echo "PASS: $count/$jobs jobs rendered, parsed and stored via the deployed image"
}

case "${1:-all}" in
integration) integration ;;
e2e) e2e ;;
all) integration && e2e ;;
*) echo "usage: $0 [integration|e2e|all]" >&2; exit 2 ;;
esac
