#!/bin/sh
# One-shot provisioner for Floci Flix.
#
# Runs to completion before the API starts (compose depends_on with
# service_completed_successfully). It owns everything the application consumes:
# the three AWS resources, the database schema, the seed data and the search
# index. The API only discovers and connects.
#
# This is deliberately an external job with no knowledge of the app, to show what
# that costs. It runs exactly once, at deploy. Nothing re-runs it when Floci
# restarts and forgets every resource it was told to create.
set -eu

FLOCI_ENDPOINT="${FLOCI_ENDPOINT:-http://floci:4566}"
FLOCI_HOST="$(printf '%s' "$FLOCI_ENDPOINT" | sed -e 's|^https\{0,1\}://||' -e 's|:.*$||')"

DB_INSTANCE_ID="${DB_INSTANCE_ID:-flociflix-db}"
CACHE_GROUP_ID="${CACHE_GROUP_ID:-flociflix-cache}"
SEARCH_DOMAIN="${SEARCH_DOMAIN:-flociflix-search}"
SEARCH_PORT="${SEARCH_PUBLISHED_PORT:-9400}"
INDEX="${SEARCH_INDEX:-movies}"

export AWS_ENDPOINT_URL="$FLOCI_ENDPOINT"
export AWS_REGION="${AWS_REGION:-us-east-1}"
export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-floci}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-floci}"
export AWS_PAGER=""

log() { echo "[provision] $*"; }
die() { echo "[provision] FATAL: $*" >&2; exit 1; }

# ---------------------------------------------------------------- 1. wait for Floci
# /_floci/health is what the Floci image's own HEALTHCHECK uses. Creating a
# resource before the API server is listening just fails, so block here first.
log "waiting for Floci at $FLOCI_ENDPOINT"
i=0
until curl -sf "$FLOCI_ENDPOINT/_floci/health" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -gt 300 ] && die "Floci did not become healthy within 10 minutes"
    sleep 2
done
log "Floci is healthy"

# Floci also reports its init-hook phases. Waiting for 'start' means the AWS
# services are actually mounted, not merely that the process is up.
i=0
until curl -sf "$FLOCI_ENDPOINT/_floci/init" 2>/dev/null | grep -q '"start":true'; do
    i=$((i + 1))
    [ "$i" -gt 60 ] && { log "WARNING: init phases never reported ready; continuing"; break; }
    sleep 2
done
log "Floci reports its startup phases complete"

# ---------------------------------------------------------------- 2. AWS resources
# Describe-then-create so a re-run is harmless.
if aws rds describe-db-instances --db-instance-identifier "$DB_INSTANCE_ID" >/dev/null 2>&1; then
    log "RDS instance $DB_INSTANCE_ID already exists"
else
    log "creating RDS instance $DB_INSTANCE_ID"
    aws rds create-db-instance \
        --db-instance-identifier "$DB_INSTANCE_ID" \
        --db-instance-class db.t3.micro \
        --engine postgres \
        --allocated-storage 20 \
        --master-username "$APP_DB_USER" \
        --master-user-password "$APP_DB_PASSWORD" \
        --db-name "$APP_DB_NAME" >/dev/null
fi

if aws elasticache describe-replication-groups --replication-group-id "$CACHE_GROUP_ID" >/dev/null 2>&1; then
    log "ElastiCache group $CACHE_GROUP_ID already exists"
else
    log "creating ElastiCache replication group $CACHE_GROUP_ID"
    aws elasticache create-replication-group \
        --replication-group-id "$CACHE_GROUP_ID" \
        --replication-group-description "Floci Flix read cache" >/dev/null
fi

if aws opensearch describe-domain --domain-name "$SEARCH_DOMAIN" >/dev/null 2>&1; then
    log "OpenSearch domain $SEARCH_DOMAIN already exists"
else
    log "creating OpenSearch domain $SEARCH_DOMAIN (pulls a ~1.2GB image on a cold cache)"
    # Tolerated: on a cold image cache this call routinely outlives its deadline
    # while Floci keeps provisioning. The readiness wait below is the real gate.
    aws opensearch create-domain \
        --domain-name "$SEARCH_DOMAIN" \
        --engine-version OpenSearch_2.11 >/dev/null 2>&1 \
        || log "create-domain did not return cleanly; polling for the domain instead"
fi

# ---------------------------------------------------------------- 3. wait until usable
# The API fails fast rather than retrying, so this job must not exit until the
# resources actually answer - not merely until the control plane admits they exist.
log "waiting for RDS to report available"
i=0
until [ "$(aws rds describe-db-instances --db-instance-identifier "$DB_INSTANCE_ID" \
           --query 'DBInstances[0].DBInstanceStatus' --output text 2>/dev/null)" = "available" ]; do
    i=$((i + 1)); [ "$i" -gt 150 ] && die "RDS never became available"; sleep 2
done

log "waiting for ElastiCache to report available"
i=0
until [ "$(aws elasticache describe-replication-groups --replication-group-id "$CACHE_GROUP_ID" \
           --query 'ReplicationGroups[0].Status' --output text 2>/dev/null)" = "available" ]; do
    i=$((i + 1)); [ "$i" -gt 150 ] && die "ElastiCache never became available"; sleep 2
done

log "waiting for the OpenSearch domain to finish processing"
i=0
until [ "$(aws opensearch describe-domain --domain-name "$SEARCH_DOMAIN" \
           --query 'DomainStatus.[Created,Processing]' --output text 2>/dev/null)" = "True	False" ]; do
    i=$((i + 1)); [ "$i" -gt 450 ] && die "OpenSearch domain never became active"; sleep 2
done
log "all three resources are usable"

# ---------------------------------------------------------------- 4. resolve endpoints
DB_HOST="$(aws rds describe-db-instances --db-instance-identifier "$DB_INSTANCE_ID" \
           --query 'DBInstances[0].Endpoint.Address' --output text)"
DB_PORT="$(aws rds describe-db-instances --db-instance-identifier "$DB_INSTANCE_ID" \
           --query 'DBInstances[0].Endpoint.Port' --output text)"

# Floci advertises OpenSearch as a Docker container name, which resolves only
# inside its Docker network. From a pod it does not, so use the port the
# container publishes on the Floci Service instead.
SEARCH_URL="http://${FLOCI_HOST}:${SEARCH_PORT}"

log "postgres=$DB_HOST:$DB_PORT opensearch=$SEARCH_URL"

export PGPASSWORD="$APP_DB_PASSWORD"
PSQL="psql -h $DB_HOST -p $DB_PORT -U $APP_DB_USER -d $APP_DB_NAME -v ON_ERROR_STOP=1"

log "waiting for the database to accept connections"
i=0
until $PSQL -c 'SELECT 1' >/dev/null 2>&1; do
    i=$((i + 1)); [ "$i" -gt 150 ] && die "database never accepted a connection"; sleep 2
done

# ---------------------------------------------------------------- 5. schema and seed
log "applying schema"
$PSQL -q -f sql/schema.sql

COUNT="$($PSQL -t -A -c 'SELECT count(*) FROM movies')"
if [ "$COUNT" -gt 0 ]; then
    log "catalogue already holds $COUNT rows, skipping seed"
else
    log "loading seed data"
    $PSQL -q -f sql/seed.sql
    COUNT="$($PSQL -t -A -c 'SELECT count(*) FROM movies')"
    log "seeded $COUNT rows"
fi

# ---------------------------------------------------------------- 6. search index
log "waiting for the OpenSearch node to answer"
i=0
until curl -sf "$SEARCH_URL/_cluster/health" >/dev/null 2>&1; do
    i=$((i + 1)); [ "$i" -gt 150 ] && die "OpenSearch node never answered at $SEARCH_URL"; sleep 2
done

if curl -sf -o /dev/null "$SEARCH_URL/$INDEX"; then
    log "index $INDEX already exists"
else
    log "creating index $INDEX"
    curl -sf -X PUT "$SEARCH_URL/$INDEX" \
        -H 'Content-Type: application/json' \
        --data-binary @sql/opensearch-index.json >/dev/null || die "could not create the index"
fi

# Build the bulk payload in SQL rather than shell: PostgreSQL emits valid JSON,
# shell string-mangling does not.
log "indexing the catalogue into OpenSearch"
$PSQL -t -A -c "
SELECT format('{\"index\":{\"_id\":%s}}', to_json(id)) || chr(10) ||
       json_build_object(
         'id', id, 'title', title, 'year', year, 'genre', genre,
         'director', director, 'synopsis', synopsis, 'rating', rating,
         'createdAt', to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')
       )::text
FROM movies ORDER BY created_at;" \
  | curl -sf -X POST "$SEARCH_URL/$INDEX/_bulk" \
      -H 'Content-Type: application/x-ndjson' --data-binary @- >/dev/null \
  || die "bulk indexing failed"

curl -sf -X POST "$SEARCH_URL/$INDEX/_refresh" >/dev/null || true
INDEXED="$(curl -sf "$SEARCH_URL/$INDEX/_count" | sed -n 's/.*"count":\([0-9]*\).*/\1/p')"
log "indexed $INDEXED documents"

log "done - the API may start"
