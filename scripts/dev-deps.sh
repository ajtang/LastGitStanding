#!/usr/bin/env bash
# Local Redis and Postgres for people without Docker (or who are tired of it).
# Non-default ports, private data dir, nothing shared with anything already running.
#   scripts/dev-deps.sh start   # then export the two URLs it prints
#   scripts/dev-deps.sh stop
set -euo pipefail
DATA="${LGS_DEV_DATA:-$(cd "$(dirname "$0")/.." && pwd)/.devdata}"
PG_PORT=5490
REDIS_PORT=6390
# On macOS, Postgres refuses to start under some locale settings ("postmaster became
# multithreaded during startup"). Its own hint is to set this, so we do.
export LC_ALL="${LC_ALL:-en_US.UTF-8}"

find_pg() {
  command -v pg_ctl >/dev/null && { dirname "$(command -v pg_ctl)"; return; }
  ls -d /opt/homebrew/opt/postgresql@*/bin /usr/local/opt/postgresql@*/bin /usr/lib/postgresql/*/bin 2>/dev/null | tail -1
}

case "${1:-start}" in
start)
  mkdir -p "$DATA"
  PG="$(find_pg)"
  [ -n "$PG" ] || { echo "no Postgres binaries found" >&2; exit 1; }
  [ -d "$DATA/pg" ] || "$PG/initdb" -D "$DATA/pg" -U lgs --auth=trust >/dev/null
  "$PG/pg_ctl" -D "$DATA/pg" -l "$DATA/pg.log" -w -o "-p $PG_PORT -c unix_socket_directories='' -c listen_addresses=127.0.0.1" start >/dev/null
  "$PG/psql" -h 127.0.0.1 -p "$PG_PORT" -U lgs -d postgres -tc "SELECT 1 FROM pg_database WHERE datname='lgs'" | grep -q 1 \
    || "$PG/createdb" -h 127.0.0.1 -p "$PG_PORT" -U lgs lgs
  redis-server --bind 127.0.0.1 --port "$REDIS_PORT" --dir "$DATA" --daemonize yes --save "" --pidfile "$DATA/redis.pid" --logfile "$DATA/redis.log" >/dev/null
  echo "export REDIS_URL=redis://127.0.0.1:$REDIS_PORT"
  echo "export DATABASE_URL=postgres://lgs@127.0.0.1:$PG_PORT/lgs"
  ;;
stop)
  "$(find_pg)/pg_ctl" -D "$DATA/pg" -m fast stop >/dev/null || true
  redis-cli -p "$REDIS_PORT" shutdown nosave 2>/dev/null || true
  ;;
*)
  echo "usage: $0 start|stop" >&2; exit 2 ;;
esac
