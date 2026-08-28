# Provisioning job: creates the AWS resources, applies the schema, loads the
# seed data and builds the search index.
#
# postgres:16-alpine for psql (matching the engine Floci starts), plus the AWS
# CLI and curl from apk. Nothing here is Go: this is deliberately an external
# tool that knows nothing about the application.
FROM postgres:16-alpine

RUN apk add --no-cache aws-cli curl

WORKDIR /provision
COPY sql/ ./sql/
COPY provision.sh ./
RUN chmod +x provision.sh

ENTRYPOINT ["/provision/provision.sh"]
