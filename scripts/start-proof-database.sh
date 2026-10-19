#!/usr/bin/env bash

# Start one disposable CI database. The workflow owns container cleanup.
set -euo pipefail

case "${1:-}" in
  mysql)
    MYSQL_ROOT_PASSWORD="$(openssl rand -hex 24)"
    export MYSQL_ROOT_PASSWORD
    echo "::add-mask::${MYSQL_ROOT_PASSWORD}"
    echo "MYSQL_ROOT_PASSWORD=${MYSQL_ROOT_PASSWORD}" >> "${GITHUB_ENV}"
    docker run --detach --name otelcontext-dbproof-mysql \
      --publish 127.0.0.1:3306:3306 \
      --env MYSQL_ROOT_PASSWORD --env MYSQL_DATABASE=otelcontext \
      --health-cmd 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysqladmin ping --host=127.0.0.1 --user=root' \
      --health-interval 5s --health-timeout 5s --health-retries 24 \
      mysql:8.4.11@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb
    for ((attempt=0; attempt<120; attempt++)); do
      if [[ "$(docker inspect --format '{{.State.Health.Status}}' otelcontext-dbproof-mysql)" == healthy ]]; then
        exit 0
      fi
      sleep 1
    done
    echo "MySQL failed to become healthy" >&2
    exit 1
    ;;
  sqlserver)
    MSSQL_SA_PASSWORD="$(openssl rand -hex 24)-aA1"
    export MSSQL_SA_PASSWORD
    echo "::add-mask::${MSSQL_SA_PASSWORD}"
    echo "MSSQL_SA_PASSWORD=${MSSQL_SA_PASSWORD}" >> "${GITHUB_ENV}"
    docker run --detach --name otelcontext-dbproof-sqlserver \
      --publish 127.0.0.1:1433:1433 \
      --env ACCEPT_EULA=Y --env MSSQL_SA_PASSWORD \
      --env MSSQL_PID=Developer --env MSSQL_MEMORY_LIMIT_MB=2048 \
      --health-cmd '/opt/mssql-tools18/bin/sqlcmd -S 127.0.0.1 -U sa -P "$MSSQL_SA_PASSWORD" -C -Q "SELECT 1"' \
      --health-interval 10s --health-timeout 5s --health-retries 30 \
      mcr.microsoft.com/mssql/server:2022-CU26-ubuntu-22.04@sha256:ba4c8329f48fb8f02e1416be6a930ebfd71268caee78aa985f3af4315e457c89
    for ((attempt=0; attempt<300; attempt++)); do
      if [[ "$(docker inspect --format '{{.State.Health.Status}}' otelcontext-dbproof-sqlserver)" == healthy ]]; then
        exit 0
      fi
      sleep 1
    done
    echo "SQL Server failed to become healthy" >&2
    exit 1
    ;;
  *)
    echo "usage: $0 {mysql|sqlserver}" >&2
    exit 2
    ;;
esac
