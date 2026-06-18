#!/usr/bin/env bash
# demo-quickstart.sh -- One-liner evaluation script
# Brings up a demo PG + minio S3 on Docker, runs pg_hardstorage into it
set -euo pipefail

echo "pg_hardstorage — demo quicksart"
echo "Pull a PG 18 evaluation in 60 seconds on Docker"
docker run --name pg-hardstorage-demo-pg -e POSTGRES_PASSWORD=demo -e POSTGRES_DB=demo -d postgres:18-alpine
echo "PostgreSQL 18 running. Next: run pg_hardstorage init --quick against it."

echo "Cleanup: docker stop pg-hardstorage-demo-pg && docker rm pg-hardstorage-demo-pg"