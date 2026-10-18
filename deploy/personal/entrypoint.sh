#!/bin/sh
# Personal store in one container: tk-api, tk-mcp and tk-proxy, next to a
# Postgres the operator runs (see docker-compose.yml). Everything this store
# owns lives in /data: its signing key, the database password of its own
# role, and the private link of its owner.
#
#   TK_DB_HOST      Postgres host                      (default postgres)
#   TK_DB_PASSWORD  password of the postgres superuser (required)
#   TK_OWNER        the owner's email, who signs       (required)
#   TK_PUBLIC_URL   the address people connect to      (default http://localhost:8080)
#   RESEND_API_KEY, MAIL_FROM  optional: sign-in by emailed code instead of the private link only
set -eu

DATA=${TK_DATA:-/data}
DB_HOST=${TK_DB_HOST:-postgres}
PUBLIC_URL=${TK_PUBLIC_URL:-http://localhost:8080}
: "${TK_DB_PASSWORD:?set TK_DB_PASSWORD, the postgres superuser password}"
: "${TK_OWNER:?set TK_OWNER, the email of the person who signs in this store}"
mkdir -p "$DATA"
umask 077

# The store's own signing key, made once and never shipped in the image.
if [ ! -s "$DATA/grant.seed" ]; then
  tk token keygen --out "$DATA/grant.seed" > "$DATA/grant.pub"
  echo "tk: made this store's signing key in $DATA/grant.seed"
fi
[ -s "$DATA/db.pass" ] || head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$DATA/db.pass"
DB_PASS=$(cat "$DATA/db.pass")

# The database and the roles the migrations grant to. tk-api runs the
# migrations itself; it reads and writes as tk_api, which row-level security
# applies to.
ADMIN="postgres://postgres:$TK_DB_PASSWORD@$DB_HOST:5432"
export PGCONNECT_TIMEOUT=3
until psql "$ADMIN/postgres" -qtAc 'SELECT 1' >/dev/null 2>&1; do echo "tk: waiting for Postgres at $DB_HOST"; sleep 2; done
psql "$ADMIN/postgres" -qtAc "SELECT 1 FROM pg_database WHERE datname = 'tk'" | grep -q 1 || psql "$ADMIN/postgres" -qc 'CREATE DATABASE tk'
psql "$ADMIN/postgres" -q -v ON_ERROR_STOP=1 -v pass="$DB_PASS" <<'SQL'
SELECT format('CREATE ROLE tk_api LOGIN PASSWORD %L', :'pass') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tk_api') \gexec
SELECT format('ALTER ROLE tk_api LOGIN PASSWORD %L', :'pass') \gexec
SELECT 'CREATE ROLE tk_checker NOLOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tk_checker') \gexec
REVOKE CONNECT ON DATABASE tk FROM PUBLIC;
GRANT CONNECT ON DATABASE tk TO tk_api;
SQL

DEMO_KEY=""
[ -n "${RESEND_API_KEY:-}" ] && DEMO_KEY=$(cat "$DATA/grant.seed")

TK_ADMIN_DSN="$ADMIN/tk?sslmode=disable" TK_DSN="postgres://tk_api:$DB_PASS@$DB_HOST:5432/tk?sslmode=disable" \
  TK_GRANT_PUBKEYS="$(cat "$DATA/grant.pub")" TK_GRANT_AUD=tk TK_API_ADDR=127.0.0.1:8081 \
  TK_PUBLIC_URL="$PUBLIC_URL" TK_DEMO_KEY="$DEMO_KEY" TK_OAUTH_CIMD=off TK_CONTACT="${TK_CONTACT:-$TK_OWNER}" \
  tk-api &
API=$!
until wget -qO- http://127.0.0.1:8081/v1/healthz >/dev/null 2>&1; do
  kill -0 $API 2>/dev/null || { echo "tk: tk-api stopped" >&2; exit 1; }
  sleep 1
done

TK_API_URL=http://127.0.0.1:8081 TK_MCP_ADDR=127.0.0.1:8083 TK_PUBLIC_URL="$PUBLIC_URL" tk-mcp --transport http &
MCP=$!
TK_PROXY_ADDR=0.0.0.0:8080 tk-proxy &
PROXY=$!

# The owner's private link: the key to the store, issued once.
if [ ! -s "$DATA/owner.grant" ]; then
  tk token issue --user "$TK_OWNER" --actor claude --key "$DATA/grant.seed" --aud tk --node local > "$DATA/owner.grant"
fi
echo "tk: store ready. Connect your assistant to this private link (treat it like a password):"
echo "tk:   $PUBLIC_URL/mcp/t/$(cat "$DATA/owner.grant")"
echo "tk: it is also kept in $DATA/owner.grant"

# One part stopping stops the container, so Docker's restart policy brings
# the whole store back.
while kill -0 $API 2>/dev/null && kill -0 $MCP 2>/dev/null && kill -0 $PROXY 2>/dev/null; do sleep 5; done
echo "tk: a part stopped; exiting" >&2
exit 1
