#!/usr/bin/env bash
set -euo pipefail

# A private socket and disabled TCP keep this cluster separate from local services.
pg_bin="${PG_BINDIR:-$(pg_config --bindir)}"
test_root="$(mktemp -d /tmp/ddl-mt-postgres.XXXXXX)"
pool_pids=()
# cleanup stops only the processes started by this script and removes its private cluster.
cleanup() {
  for pid in "${pool_pids[@]}"; do kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; done
  "$pg_bin/pg_ctl" -D "$test_root/data" -m immediate -w stop >/dev/null 2>&1 || true
  rm -rf "$test_root"
}
trap cleanup EXIT
if [[ "${DDL_MT_TEST_PGBOUNCER:-}" == 1 ]]; then
  command -v "${PGBOUNCER_BIN:-pgbouncer}" >/dev/null || { echo 'PgBouncer is required; install it or set PGBOUNCER_BIN.' >&2; exit 1; }
fi
mkdir "$test_root/socket"
"$pg_bin/initdb" -D "$test_root/data" -A trust --no-locale -E UTF8 >"$test_root/init.log"
"$pg_bin/pg_ctl" -D "$test_root/data" -l "$test_root/server.log" \
  -o "-k $test_root/socket -h '' -p 55439" -w start >/dev/null
if [[ "${DDL_MT_TEST_PGBOUNCER:-}" == 1 ]]; then
  pool_bin="${PGBOUNCER_BIN:-pgbouncer}"
  for mode in transaction session; do
    pool_dir="$test_root/$mode"
    mkdir "$pool_dir"
    printf '"%s" "test-only"\n' "$(id -un)" > "$pool_dir/users.txt"
    cat > "$pool_dir/pgbouncer.ini" <<EOF
[databases]
* = host=$test_root/socket port=55439
[pgbouncer]
listen_addr =
listen_port = 55440
unix_socket_dir = $pool_dir
unix_socket_mode = 0700
auth_type = trust
auth_file = $pool_dir/users.txt
pool_mode = $mode
default_pool_size = 3
server_round_robin = 1
max_client_conn = 30
logfile = $pool_dir/log
pidfile = $pool_dir/pid
EOF
    "$pool_bin" "$pool_dir/pgbouncer.ini" >"$pool_dir/stdout" 2>&1 &
    pool_pids+=("$!")
    ready=0
    for ((i=0; i<100; i++)); do
      if "$pg_bin/pg_isready" -h "$pool_dir" -p 55440 -U "$(id -un)" -d postgres >/dev/null 2>&1; then ready=1; break; fi
      sleep 0.05
    done
    if [[ "$ready" != 1 ]]; then cat "$pool_dir/stdout"; exit 1; fi
  done
  export DDL_MT_TEST_TRANSACTION_DSN="host=$test_root/transaction port=55440 user=$(id -un) dbname=postgres sslmode=disable"
  export DDL_MT_TEST_SESSION_DSN="host=$test_root/session port=55440 user=$(id -un) dbname=postgres sslmode=disable"
fi
DDL_MT_TEST_DSN="host=$test_root/socket port=55439 user=$(id -un) dbname=postgres sslmode=disable" \
  go test -race -count=1 -timeout=2m "$@" ./...
