#!/usr/bin/env bash

# Disposable, production-shaped systemd proof for one OtelContext release.
# Run only on an isolated host. The script refuses any pre-existing production
# account, path, unit, or listener and removes every object it creates.

set -euo pipefail

readonly proof_schema="otelcontext.systemd-proof.v1"
readonly service_name="otelcontext.service"
readonly service_user="otelcontext"
readonly opt_root="/opt/otelcontext"
readonly config_root="/etc/otelcontext"
readonly state_root="/var/lib/otelcontext"
readonly rollback_root="/var/lib/otelcontext-rollback"
readonly backup_root="/var/backups/otelcontext"
readonly unit_path="/etc/systemd/system/otelcontext.service"
readonly env_path="/etc/otelcontext/otelcontext.env"
readonly http_base="http://127.0.0.1:8080"

archive=""
tag=""
candidate_sha=""
proof_dir=""
work_dir=""
cleanup_done=0
safe_to_cleanup=0
unit_installed=0
command_sequence=0
source_proof="${OTELCONTEXT_PROOF_SOURCE:-0}"
cosign_bin="${OTELCONTEXT_COSIGN:-cosign}"
candidate_binary=""
candidate_archive_sha=""
candidate_binary_sha=""
previous_binary=""

usage() {
  echo "usage: sudo scripts/prove-systemd-release.sh --archive PATH --tag TAG --sha 40_HEX_SHA --proof-dir ABSOLUTE_PATH" >&2
}

die() {
  echo "systemd proof: $*" >&2
  exit 1
}

while (($# > 0)); do
  case "$1" in
    --archive)
      archive="${2:-}"
      shift 2
      ;;
    --tag)
      tag="${2:-}"
      shift 2
      ;;
    --sha)
      candidate_sha="${2:-}"
      shift 2
      ;;
    --proof-dir)
      proof_dir="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage
      die "unknown argument $1"
      ;;
  esac
done

[[ $EUID -eq 0 ]] || die "run this proof with sudo on an isolated host"
[[ -n "$archive" && -n "$tag" && -n "$candidate_sha" && -n "$proof_dir" ]] || {
  usage
  exit 2
}
[[ "$archive" = /* && -f "$archive" ]] || die "--archive must be an absolute regular file"
[[ "$proof_dir" = /* ]] || die "--proof-dir must be absolute"
[[ "$candidate_sha" =~ ^[0-9a-f]{40}$ ]] || die "--sha must be a lowercase 40-character commit"
[[ "$tag" =~ ^[0-9A-Za-z._+-]+$ ]] || die "--tag contains unsafe path characters"
[[ "$source_proof" == "0" || "$source_proof" == "1" ]] || die "OTELCONTEXT_PROOF_SOURCE must be 0 or 1"
command -v realpath >/dev/null 2>&1 || die "required command not found: realpath"
proof_dir="$(realpath -m -- "$proof_dir")"
case "$proof_dir/" in
  "$opt_root/"*|"$config_root/"*|"$state_root/"*|"$rollback_root/"*|"$backup_root/"*)
    die "--proof-dir must be outside every disposable production path"
    ;;
esac

for command in systemctl systemd-analyze systemd-run journalctl useradd userdel getent \
  install mount umount mountpoint tar sha256sum jq curl ss stat find awk sed grep \
  base64 openssl go dd "$cosign_bin"; do
  command -v "$command" >/dev/null 2>&1 || die "required command not found: $command"
done

[[ ! -e "$proof_dir" ]] || die "proof directory already exists: $proof_dir"
install -d -m 0750 "$proof_dir"
touch "$proof_dir/command-exits.jsonl"
work_dir="$(mktemp -d /var/tmp/otelcontext-systemd-proof.XXXXXX)"

record_exit() {
  local name="$1" code="$2"
  jq -nc --arg name "$name" --argjson exit_code "$code" \
    '{name:$name,exit_code:$exit_code}' >>"$proof_dir/command-exits.jsonl"
}

run_capture() {
  local name="$1" expected="$2" stdout_path="$3" stderr_path="$4"
  shift 4
  set +e
  "$@" >"$stdout_path" 2>"$stderr_path"
  local code=$?
  set -e
  record_exit "$name" "$code"
  if [[ "$code" -ne "$expected" ]]; then
    sed -n '1,160p' "$stdout_path" >&2 || true
    sed -n '1,160p' "$stderr_path" >&2 || true
    die "$name exited $code, want $expected"
  fi
}

capture_journal() {
  if [[ "$unit_installed" -eq 1 ]]; then
    journalctl -u "$service_name" --no-pager -o short-iso-precise >"$proof_dir/journal.log" 2>&1 || true
  fi
}

listeners_present() {
  ss -H -ltn | awk '{print $4}' | grep -Eq '(^|:)(8080|4317)$'
}

perform_cleanup() {
  local failures=0
  set +e
  systemctl stop "$service_name" >/dev/null 2>&1
  systemctl disable "$service_name" >/dev/null 2>&1
  rm -f -- "$unit_path" "/etc/systemd/system/multi-user.target.wants/$service_name"
  systemctl daemon-reload >/dev/null 2>&1
  systemctl reset-failed "$service_name" >/dev/null 2>&1
  if mountpoint -q "$rollback_root"; then
    umount "$rollback_root" || failures=$((failures + 1))
  fi
  if mountpoint -q "$state_root"; then
    umount "$state_root" || failures=$((failures + 1))
  fi
  if getent passwd "$service_user" >/dev/null 2>&1; then
    userdel "$service_user" >/dev/null 2>&1 || failures=$((failures + 1))
  fi
  rm -rf -- "$opt_root" "$config_root" "$state_root" "$rollback_root" "$backup_root"
  rm -rf -- "$work_dir"

  [[ ! -e "$unit_path" && ! -L "$unit_path" ]] || failures=$((failures + 1))
  [[ ! -e "$opt_root" && ! -e "$config_root" && ! -e "$state_root" && ! -e "$rollback_root" && ! -e "$backup_root" ]] || failures=$((failures + 1))
  getent passwd "$service_user" >/dev/null 2>&1 && failures=$((failures + 1))
  listeners_present && failures=$((failures + 1))

  jq -n --argjson failures "$failures" \
    '{account_removed:($failures == 0),unit_removed:($failures == 0),paths_removed:($failures == 0),listeners_closed:($failures == 0),passed:($failures == 0)}' \
    >"$proof_dir/cleanup.json"
  cleanup_done=1
  set -e
  [[ "$failures" -eq 0 ]]
}

finish() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  capture_journal
  if [[ "$safe_to_cleanup" -eq 1 && "$cleanup_done" -ne 1 ]]; then
    perform_cleanup || status=1
  elif [[ -n "$work_dir" ]]; then
    rm -rf -- "$work_dir"
  fi
  if [[ -s "$proof_dir/command-exits.jsonl" ]]; then
    jq -s . "$proof_dir/command-exits.jsonl" >"$proof_dir/command-exits.json" 2>/dev/null || status=1
  fi
  if [[ -n "${SUDO_UID:-}" && -n "${SUDO_GID:-}" ]]; then
    chown -R "$SUDO_UID:$SUDO_GID" "$proof_dir" 2>/dev/null || status=1
  fi
  exit "$status"
}
trap finish EXIT INT TERM

for path in "$opt_root" "$config_root" "$state_root" "$rollback_root" "$backup_root" "$unit_path"; do
  [[ ! -e "$path" && ! -L "$path" ]] || die "refusing pre-existing production path: $path"
done
getent passwd "$service_user" >/dev/null 2>&1 && die "refusing pre-existing account: $service_user"
systemctl cat "$service_name" >/dev/null 2>&1 && die "refusing pre-existing unit: $service_name"
listeners_present && die "refusing host with an existing listener on 8080 or 4317"
[[ "$(systemctl is-system-running 2>/dev/null || true)" =~ ^(running|degraded)$ ]] || die "systemd is not running"
safe_to_cleanup=1

decode_certificate() {
  local source="$1" target="$2"
  if grep -q '^-----BEGIN CERTIFICATE-----' "$source"; then
    cp "$source" "$target"
  else
    base64 --decode "$source" >"$target"
  fi
  openssl x509 -in "$target" -noout -subject -issuer -ext subjectAltName >/dev/null
}

verify_archive() {
  local label="$1" release_archive="$2" release_tag="$3" expected_version="$4" identity="$5" require_deploy="$6" expected_sha="${7:-}"
  local asset_dir checksums signature certificate sbom decoded archive_name archive_sha binary_sha extract_dir version_output executable_count
  asset_dir="$(dirname "$release_archive")"
  checksums="$asset_dir/checksums.txt"
  signature="$asset_dir/checksums.txt.sig"
  certificate="$asset_dir/checksums.txt.pem"
  sbom="$release_archive.sbom.json"
  archive_name="$(basename "$release_archive")"
  decoded="$proof_dir/$label-certificate.pem"
  extract_dir="$work_dir/$label"

  for file in "$checksums" "$signature" "$certificate" "$sbom"; do
    [[ -s "$file" ]] || die "$label release evidence is missing $file"
  done
  [[ "$(awk -v name="$archive_name" '$2 == name { count++ } END { print count + 0 }' "$checksums")" -eq 1 ]] || die "$label archive checksum entry is missing or duplicated"
  [[ "$(awk -v name="$(basename "$sbom")" '$2 == name { count++ } END { print count + 0 }' "$checksums")" -eq 1 ]] || die "$label SBOM checksum entry is missing or duplicated"
  (
    cd "$asset_dir"
    {
      awk -v name="$archive_name" '$2 == name' checksums.txt
      awk -v name="$(basename "$sbom")" '$2 == name' checksums.txt
    } | sha256sum --check -
  ) >"$proof_dir/$label-checksum.txt"
  decode_certificate "$certificate" "$decoded"
  run_capture "$label-signature" 0 "$proof_dir/$label-signature.txt" "$proof_dir/$label-signature.stderr" \
    "$cosign_bin" verify-blob --certificate "$decoded" --signature "$signature" \
    --certificate-identity "$identity" --certificate-oidc-issuer "https://token.actions.githubusercontent.com" "$checksums"

  tar -tzf "$release_archive" >"$proof_dir/$label-archive-files.txt"
  while IFS= read -r entry; do
    [[ "$entry" != /* && "$entry" != ".." && "$entry" != ../* && "$entry" != */../* ]] || die "$label archive contains an unsafe path: $entry"
  done <"$proof_dir/$label-archive-files.txt"
  install -d -m 0755 "$extract_dir"
  tar -xzf "$release_archive" -C "$extract_dir"
  [[ -f "$extract_dir/otelcontext" && ! -L "$extract_dir/otelcontext" ]] || die "$label archive must contain one regular otelcontext binary at its root"
  [[ -f "$extract_dir/README.md" && -f "$extract_dir/LICENSE.md" ]] || die "$label archive is missing README.md or LICENSE.md"
  if [[ "$require_deploy" -eq 1 ]]; then
    [[ -f "$extract_dir/deploy/systemd/otelcontext.service" && -f "$extract_dir/deploy/systemd/otelcontext.env.example" ]] || die "candidate archive is missing the systemd deployment files"
  fi
  executable_count="$(find "$extract_dir" -type f -perm /111 | wc -l)"
  [[ "$executable_count" -eq 1 ]] || die "$label archive contains $executable_count executable files, want one"
  chmod 0755 "$extract_dir/otelcontext"
  version_output="$("$extract_dir/otelcontext" --version)"
  [[ "$version_output" == "OtelContext version $expected_version" ]] || die "$label version is $version_output, want OtelContext version $expected_version"
  go version -m "$extract_dir/otelcontext" >"$proof_dir/$label-go-build.txt"
  if [[ -n "$expected_sha" ]]; then
    grep -Eq "vcs\.revision(=|[[:space:]]+)$expected_sha$" "$proof_dir/$label-go-build.txt" || die "$label binary is not bound to commit $expected_sha"
  fi
  archive_sha="$(sha256sum "$release_archive" | awk '{print $1}')"
  binary_sha="$(sha256sum "$extract_dir/otelcontext" | awk '{print $1}')"
  openssl x509 -in "$decoded" -noout -subject -issuer -ext subjectAltName >"$proof_dir/$label-certificate.txt"
  jq -n --arg tag "$release_tag" --arg archive "$archive_name" --arg archive_sha256 "$archive_sha" \
    --arg binary_sha256 "$binary_sha" --arg version "$version_output" --arg certificate_identity "$identity" \
    --arg sbom "$(basename "$sbom")" \
    '{tag:$tag,archive:$archive,archive_sha256:$archive_sha256,binary_sha256:$binary_sha256,version:$version,certificate_identity:$certificate_identity,signature_verified:true,sbom:$sbom}' \
    >"$proof_dir/$label-release.json"
  printf -v "${label}_binary" '%s' "$extract_dir/otelcontext"
  printf -v "${label}_archive_sha" '%s' "$archive_sha"
  printf -v "${label}_binary_sha" '%s' "$binary_sha"
}

candidate_identity="${OTELCONTEXT_PROOF_CERTIFICATE_IDENTITY:-https://github.com/RandomCodeSpace/otelcontext/.github/workflows/release.yml@refs/tags/$tag}"
candidate_version="$tag"
if [[ "$source_proof" -eq 1 ]]; then
  [[ -n "${OTELCONTEXT_PROOF_CERTIFICATE_IDENTITY:-}" ]] || die "source proof requires OTELCONTEXT_PROOF_CERTIFICATE_IDENTITY"
fi

previous_archive="${OTELCONTEXT_PREVIOUS_ARCHIVE:-}"
previous_tag="${OTELCONTEXT_PREVIOUS_TAG:-v0.4.0-beta.2}"
[[ "$previous_archive" = /* && -f "$previous_archive" ]] || die "OTELCONTEXT_PREVIOUS_ARCHIVE must name the signed previous Linux archive"
previous_identity="https://github.com/RandomCodeSpace/otelcontext/.github/workflows/release.yml@refs/heads/main"

verify_archive candidate "$archive" "$tag" "$candidate_version" "$candidate_identity" 1 "$candidate_sha"
verify_archive previous "$previous_archive" "$previous_tag" "$previous_tag" "$previous_identity" 0

systemd_major="$(systemd-analyze --version | awk 'NR == 1 {print $2}')"
[[ "$systemd_major" =~ ^[0-9]+$ && "$systemd_major" -ge 249 ]] || die "systemd $systemd_major is older than 249"
systemd-analyze --version >"$proof_dir/systemd-version.txt"

useradd --system --user-group --home-dir "$state_root" --shell /usr/sbin/nologin "$service_user"
service_uid="$(id -u "$service_user")"
service_gid="$(id -g "$service_user")"

install -d -o root -g root -m 0755 "$opt_root" "$opt_root/releases"
install -d -o root -g root -m 0755 "$opt_root/releases/$tag" "$opt_root/releases/$previous_tag"
install -o root -g root -m 0755 "$candidate_binary" "$opt_root/releases/$tag/otelcontext"
install -o root -g root -m 0755 "$previous_binary" "$opt_root/releases/$previous_tag/otelcontext"
install -d -o root -g "$service_user" -m 0750 "$config_root"
install -d -o root -g root -m 0750 "$backup_root"
install -d -o "$service_user" -g "$service_user" -m 0750 "$state_root" "$rollback_root"
mount -t tmpfs -o "size=256m,mode=0750,uid=$service_uid,gid=$service_gid" otelcontext-proof-state "$state_root"
mount -t tmpfs -o "size=256m,mode=0750,uid=$service_uid,gid=$service_gid" otelcontext-proof-rollback "$rollback_root"
install -d -o "$service_user" -g "$service_user" -m 0750 "$state_root/dlq"
install -o root -g root -m 0644 "$work_dir/candidate/deploy/systemd/otelcontext.service" "$unit_path"
unit_installed=1

write_environment() {
  local data_root="$1"
  cat >"$env_path" <<EOF
APP_ENV=production
AGGREGATE_MODE=legacy
DB_AUTOMIGRATE=false
DB_DRIVER=sqlite
DB_DSN=$data_root/otelcontext.db
OTELCONTEXT_ALLOW_SQLITE_PROD=true
OTELCONTEXT_ALLOW_INSECURE_GRPC=true
API_KEY=
HTTP_PORT=8080
GRPC_PORT=4317
DLQ_PATH=$data_root/dlq
TLS_CACHE_DIR=$data_root/tls
AGGREGATE_DB_PATH=$data_root/aggregate.db
DATA_DISK_PATH=$data_root
DATA_DISK_BUDGET_MB=256
HOT_RETENTION_DAYS=1
EXEMPLAR_RETENTION_DAYS=1
STORE_MIN_SEVERITY=INFO
INGEST_MIN_SEVERITY=INFO
SAMPLING_RATE=1.0
GRAPHRAG_WORKER_COUNT=1
GRAPHRAG_EVENT_QUEUE_SIZE=128
EOF
  chown root:"$service_user" "$env_path"
  chmod 0640 "$env_path"
  [[ ! -e "$state_root/.env" ]] || die "working-directory .env must be absent"
}

write_environment "$state_root"
sed -E 's/^(API_KEY|DB_DSN)=.*/\1=[redacted]/' "$env_path" | sha256sum >"$proof_dir/environment-redacted.sha256"
sha256sum "$unit_path" >"$proof_dir/unit.sha256"
stat -c '%U:%G %a %n' "$opt_root" "$opt_root/releases" "$opt_root/releases/$tag" \
  "$opt_root/releases/$tag/otelcontext" "$config_root" "$env_path" "$unit_path" \
  "$state_root" "$backup_root" >"$proof_dir/file-metadata.txt"

switch_release() {
  local release="$1" temporary="$opt_root/.current.$$"
  rm -f -- "$temporary"
  ln -s "releases/$release" "$temporary"
  chown -h root:root "$temporary"
  mv -Tf "$temporary" "$opt_root/current"
  [[ "$(readlink -f "$opt_root/current")" == "$opt_root/releases/$release" ]] || die "active release selector did not switch to $release"
}

# systemd verifies an absolute ExecStart only when the selector resolves. The
# unit is still disabled and stopped; migration remains an explicit one-shot.
switch_release "$tag"

run_capture "systemd-analyze-verify" 0 "$proof_dir/systemd-analyze.txt" "$proof_dir/systemd-analyze.stderr" \
  systemd-analyze verify "$unit_path"
systemctl daemon-reload
systemctl enable "$service_name" >"$proof_dir/systemctl-enable.txt"

systemctl show "$service_name" \
  -p Type -p User -p Group -p WorkingDirectory -p EnvironmentFiles -p ExecStart \
  -p StateDirectory -p StateDirectoryMode -p UMask -p Restart -p RestartUSec \
  -p KillSignal -p KillMode -p TimeoutStopUSec -p StandardOutput -p StandardError \
  -p SyslogIdentifier -p StartLimitIntervalUSec -p StartLimitBurst -p Wants -p After \
  -p PIDFile \
  >"$proof_dir/systemd-properties.txt"
for required in \
  'Type=exec' 'User=otelcontext' 'Group=otelcontext' 'WorkingDirectory=/var/lib/otelcontext' \
  'StateDirectory=otelcontext' 'StateDirectoryMode=0750' 'UMask=0027' 'Restart=on-failure' \
  'RestartUSec=5s' 'KillSignal=15' 'KillMode=control-group' 'TimeoutStopUSec=45s' \
  'StandardOutput=journal' 'StandardError=journal' 'SyslogIdentifier=otelcontext' \
  'StartLimitIntervalUSec=1min' 'StartLimitBurst=3' 'PIDFile='; do
  grep -Fxq "$required" "$proof_dir/systemd-properties.txt" || die "systemd property mismatch: $required"
done
if grep -Eq '^[[:space:]]*(ExecStartPre|ExecStop|ExecReload|PIDFile)=' "$unit_path"; then
  die "unit contains a forbidden lifecycle wrapper or PID file"
fi
grep -Fq "$env_path" "$proof_dir/systemd-properties.txt" || die "unit does not own the expected environment file"
grep -Fq "$opt_root/current/otelcontext" "$proof_dir/systemd-properties.txt" || die "unit does not execute the active selector"
grep -Eq '^Wants=.*network-online\.target( |$)' "$proof_dir/systemd-properties.txt" || die "unit does not want network-online.target"
grep -Eq '^After=.*network-online\.target( |$)' "$proof_dir/systemd-properties.txt" || die "unit is not ordered after network-online.target"
[[ -L "/etc/systemd/system/multi-user.target.wants/$service_name" ]] || die "unit is not enabled for multi-user.target"

transient_run() {
  local label="$1" expected="$2" role="$3" binary="$4"
  shift 4
  command_sequence=$((command_sequence + 1))
  local stdout_path="$proof_dir/$label.stdout" stderr_path="$proof_dir/$label.stderr"
  local -a properties=(
    --property=Type=exec
    --property="WorkingDirectory=$state_root"
    --property="EnvironmentFile=$env_path"
  )
  if [[ "$role" == "service" ]]; then
    properties+=(--property="User=$service_user" --property="Group=$service_user")
  fi
  run_capture "$label" "$expected" "$stdout_path" "$stderr_path" \
    systemd-run --quiet --wait --pipe --collect --unit="otelcontext-proof-$command_sequence" \
    "${properties[@]}" -- "$binary" "$@"
}

transient_run migrate-status-empty 10 service "$opt_root/releases/$tag/otelcontext" migrate status
grep -Fq 'state=empty' "$proof_dir/migrate-status-empty.stdout" || die "initial migration state is not empty"
transient_run migrate-up 0 service "$opt_root/releases/$tag/otelcontext" migrate up
grep -Fq 'result=ready' "$proof_dir/migrate-up.stdout" || die "migration did not report ready"
transient_run migrate-status-exact 0 service "$opt_root/releases/$tag/otelcontext" migrate status
grep -Fq 'state=exact' "$proof_dir/migrate-status-exact.stdout" || die "migration state is not exact"

wait_probe() {
  local label="$1" path="$2" expected_code="$3" timeout_seconds="$4"
  local output="$proof_dir/$label.json" deadline=$((SECONDS + timeout_seconds)) code=""
  local started_ms elapsed_ms
  started_ms="$(date +%s%3N)"
  while ((SECONDS < deadline)); do
    set +e
    code="$(curl --silent --show-error --max-time 2 --output "$output" --write-out '%{http_code}' "$http_base$path")"
    set -e
    if [[ "$code" == "$expected_code" ]]; then
      elapsed_ms=$(( $(date +%s%3N) - started_ms ))
      jq -n --arg path "$path" --argjson status "$code" --argjson elapsed_ms "$elapsed_ms" \
        --slurpfile body "$output" '{path:$path,status:$status,elapsed_ms:$elapsed_ms,body:$body[0]}' \
        >"$proof_dir/$label-proof.json"
      return 0
    fi
    sleep 1
  done
  die "$path did not return $expected_code within $timeout_seconds seconds; last status $code"
}

assert_probes_closed() {
  local label="$1"
  set +e
  curl --silent --show-error --max-time 2 "$http_base/live" >"$proof_dir/$label-live.stdout" 2>"$proof_dir/$label-live.stderr"
  local live_exit=$?
  curl --silent --show-error --max-time 2 "$http_base/ready" >"$proof_dir/$label-ready.stdout" 2>"$proof_dir/$label-ready.stderr"
  local ready_exit=$?
  set -e
  record_exit "$label-live-unavailable" "$live_exit"
  record_exit "$label-ready-unavailable" "$ready_exit"
  [[ "$live_exit" -ne 0 && "$ready_exit" -ne 0 ]] || die "probes remained available after stop"
}

service_started_epoch="$(date +%s)"
systemctl start "$service_name"
wait_probe initial-live /live 200 30
wait_probe initial-ready /ready 200 30
jq -e '.ready == true' "$proof_dir/initial-ready.json" >/dev/null || die "initial readiness payload is false"
initial_pid="$(systemctl show "$service_name" -p MainPID --value)"
[[ "$initial_pid" -gt 1 ]] || die "initial MainPID is invalid"

fixture_time="$(date +%s%N)"
fixture_end=$((fixture_time + 1000000))
cat >"$work_dir/traces.json" <<EOF
{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"systemd-proof"}}]},"scopeSpans":[{"spans":[{"traceId":"qqqqqqqqqqqqqqqqqqqqqg==","spanId":"u7u7u7u7u7s=","name":"systemd-proof-request","kind":2,"startTimeUnixNano":"$fixture_time","endTimeUnixNano":"$fixture_end"}]}]}]}
EOF
cat >"$work_dir/logs.json" <<EOF
{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"systemd-proof"}}]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"$fixture_time","observedTimeUnixNano":"$fixture_time","severityNumber":17,"severityText":"ERROR","body":{"stringValue":"systemd-proof fixture"},"traceId":"qqqqqqqqqqqqqqqqqqqqqg==","spanId":"u7u7u7u7u7s="}]}]}]}
EOF
cat >"$work_dir/metrics.json" <<EOF
{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"systemd-proof"}}]},"scopeMetrics":[{"metrics":[{"name":"systemd_proof_requests","gauge":{"dataPoints":[{"timeUnixNano":"$fixture_time","asDouble":7}]}}]}]}]}
EOF
for signal in traces logs metrics; do
  run_capture "otlp-$signal" 0 "$proof_dir/otlp-$signal.response" "$proof_dir/otlp-$signal.stderr" \
    curl --fail --silent --show-error --max-time 5 --request POST --header 'Content-Type: application/json' \
    --data-binary "@$work_dir/$signal.json" "$http_base/v1/$signal"
done

fingerprint_api() {
  # Legacy metrics become queryable only after the 30-second TSDB window is
  # persisted. Leave a full scheduling margin beyond that boundary.
  local label="$1" deadline=$((SECONDS + 45)) traces_json logs_json metrics_json trace_count log_count metric_count digest
  while ((SECONDS < deadline)); do
    traces_json="$(curl --fail --silent --show-error --max-time 3 "$http_base/api/traces?limit=50" || true)"
    logs_json="$(curl --fail --silent --show-error --max-time 3 "$http_base/api/logs?limit=50" || true)"
    metrics_json="$(curl --fail --silent --show-error --max-time 3 "$http_base/api/metrics?name=systemd_proof_requests&service_name=systemd-proof" || true)"
    trace_count="$(jq -r '[.traces[]? | select(.service_name == "systemd-proof")] | length' <<<"$traces_json" 2>/dev/null || echo 0)"
    log_count="$(jq -r '[.data[]? | select(.body == "systemd-proof fixture")] | length' <<<"$logs_json" 2>/dev/null || echo 0)"
    metric_count="$(jq -r '[.[]? | select(.name == "systemd_proof_requests" and .service_name == "systemd-proof")] | length' <<<"$metrics_json" 2>/dev/null || echo 0)"
    if [[ "$trace_count" -eq 1 && "$log_count" -eq 1 && "$metric_count" -eq 1 ]]; then
      jq -n --argjson traces "$trace_count" --argjson logs "$log_count" --argjson metrics "$metric_count" --arg service systemd-proof \
        '{service:$service,traces:$traces,logs:$logs,metrics:$metrics}' >"$proof_dir/$label.json"
      digest="$(jq -S -c . "$proof_dir/$label.json" | sha256sum | awk '{print $1}')"
      printf '%s\n' "$digest"
      return 0
    fi
    sleep 1
  done
  printf '%s\n' "$traces_json" >"$proof_dir/$label-last-traces.json"
  printf '%s\n' "$logs_json" >"$proof_dir/$label-last-logs.json"
  printf '%s\n' "$metrics_json" >"$proof_dir/$label-last-metrics.json"
  jq -n --argjson traces "$trace_count" --argjson logs "$log_count" --argjson metrics "$metric_count" \
    '{traces:$traces,logs:$logs,metrics:$metrics}' >"$proof_dir/$label-last-counts.json"
  die "fixture API fingerprint did not stabilize (traces=$trace_count logs=$log_count metrics=$metric_count)"
}

initial_fingerprint="$(fingerprint_api fingerprint-initial)"
shutdown_before="$(journalctl -u "$service_name" --since "@$service_started_epoch" --no-pager | grep -c 'msg=shutdown_complete' || true)"
systemctl restart "$service_name"
wait_probe restart-live /live 200 30
wait_probe restart-ready /ready 200 30
restart_pid="$(systemctl show "$service_name" -p MainPID --value)"
[[ "$restart_pid" -gt 1 && "$restart_pid" -ne "$initial_pid" ]] || die "same-version restart did not change MainPID"
restart_fingerprint="$(fingerprint_api fingerprint-restart)"
[[ "$restart_fingerprint" == "$initial_fingerprint" ]] || die "same-version restart changed the fixture fingerprint"
shutdown_after_restart="$(journalctl -u "$service_name" --since "@$service_started_epoch" --no-pager | grep -c 'msg=shutdown_complete' || true)"
[[ "$shutdown_after_restart" -gt "$shutdown_before" ]] || die "same-version restart did not log shutdown_complete"

automatic_restarts_before="$(systemctl show "$service_name" -p NRestarts --value)"
systemctl kill --kill-whom=main --signal=SIGKILL "$service_name"
wait_probe crash-live /live 200 30
wait_probe crash-ready /ready 200 30
crash_pid="$(systemctl show "$service_name" -p MainPID --value)"
automatic_restarts_after="$(systemctl show "$service_name" -p NRestarts --value)"
[[ "$crash_pid" -gt 1 && "$crash_pid" -ne "$restart_pid" ]] || die "crash recovery did not change MainPID"
[[ "$automatic_restarts_after" -eq $((automatic_restarts_before + 1)) ]] || die "crash recovery restart count is not exactly one"
crash_fingerprint="$(fingerprint_api fingerprint-crash)"
[[ "$crash_fingerprint" == "$initial_fingerprint" ]] || die "crash recovery changed the fixture fingerprint"

pressure_pid="$crash_pid"
pressure_restarts="$automatic_restarts_after"
dd if=/dev/zero of="$state_root/readiness-pressure.bin" bs=1M count=245 status=none
wait_probe pressure-ready /ready 503 45
wait_probe pressure-live /live 200 5
[[ "$(systemctl show "$service_name" -p MainPID --value)" == "$pressure_pid" ]] || die "readiness failure changed MainPID"
[[ "$(systemctl show "$service_name" -p NRestarts --value)" == "$pressure_restarts" ]] || die "readiness failure caused a restart"
rm -f -- "$state_root/readiness-pressure.bin"
wait_probe pressure-recovered /ready 200 45

pre_backup_fingerprint="$(fingerprint_api fingerprint-pre-backup)"
shutdown_before_backup="$(journalctl -u "$service_name" --since "@$service_started_epoch" --no-pager | grep -c 'msg=shutdown_complete' || true)"
systemctl stop "$service_name"
systemctl is-active --quiet "$service_name" && die "service remained active after stop"
assert_probes_closed stopped
shutdown_after_backup="$(journalctl -u "$service_name" --since "@$service_started_epoch" --no-pager | grep -c 'msg=shutdown_complete' || true)"
[[ "$shutdown_after_backup" -gt "$shutdown_before_backup" ]] || die "quiesced stop did not log shutdown_complete"

transient_run backup-create 0 root "$opt_root/releases/$tag/otelcontext" backup create --out "$backup_root"
bundle="$(jq -r '.bundle // empty' "$proof_dir/backup-create.stdout")"
[[ "$bundle" == "$backup_root"/* && -f "$bundle/manifest.json" ]] || die "backup create did not publish a complete bundle"
backup_manifest_sha="$(sha256sum "$bundle/manifest.json" | awk '{print $1}')"
cp "$bundle/manifest.json" "$proof_dir/backup-manifest.json"

write_environment "$rollback_root"
install -d -o "$service_user" -g "$service_user" -m 0750 "$rollback_root"
transient_run backup-restore 0 root "$opt_root/releases/$tag/otelcontext" backup restore --bundle "$bundle"
jq -e '.status == "restored" and .ready_seconds > 0' "$proof_dir/backup-restore.stdout" >/dev/null || die "fresh-target restore did not report ready"
chown -R "$service_user:$service_user" "$rollback_root"

switch_release "$previous_tag"
systemctl start "$service_name"
wait_probe rollback-live /live 200 30
wait_probe rollback-ready /ready 200 30
rollback_pid="$(systemctl show "$service_name" -p MainPID --value)"
rollback_fingerprint="$(fingerprint_api fingerprint-rollback)"
[[ "$rollback_fingerprint" == "$pre_backup_fingerprint" ]] || die "previous signed binary did not reach the restored fingerprint"
systemctl stop "$service_name"
assert_probes_closed rollback-stopped

transient_run candidate-upgrade-migrate-status-before 0 service "$opt_root/releases/$tag/otelcontext" migrate status
grep -Fq 'state=exact' "$proof_dir/candidate-upgrade-migrate-status-before.stdout" || die "restored target migration state is not exact before candidate upgrade"
transient_run candidate-upgrade-migrate-up 0 service "$opt_root/releases/$tag/otelcontext" migrate up
grep -Fq 'result=ready' "$proof_dir/candidate-upgrade-migrate-up.stdout" || die "candidate upgrade migration did not report ready"
transient_run candidate-upgrade-migrate-status-after 0 service "$opt_root/releases/$tag/otelcontext" migrate status
grep -Fq 'state=exact' "$proof_dir/candidate-upgrade-migrate-status-after.stdout" || die "candidate upgrade migration state is not exact"
switch_release "$tag"
systemctl start "$service_name"
wait_probe upgrade-live /live 200 30
wait_probe upgrade-ready /ready 200 30
upgrade_pid="$(systemctl show "$service_name" -p MainPID --value)"
upgrade_fingerprint="$(fingerprint_api fingerprint-upgrade)"
[[ "$upgrade_fingerprint" == "$pre_backup_fingerprint" ]] || die "candidate upgrade changed the restored fingerprint"

run_capture browser-index 0 "$proof_dir/browser-index.html" "$proof_dir/browser-index.stderr" \
  curl --fail --silent --show-error --max-time 5 "$http_base/"
grep -Fq 'OtelContext' "$proof_dir/browser-index.html" || die "browser root does not identify OtelContext"
run_capture browser-app 0 "$proof_dir/browser-app.js" "$proof_dir/browser-app.stderr" \
  curl --fail --silent --show-error --max-time 5 "$http_base/static/app.js"
[[ -s "$proof_dir/browser-app.js" ]] || die "embedded browser application is empty"
jq -n --arg index_sha256 "$(sha256sum "$proof_dir/browser-index.html" | awk '{print $1}')" \
  --arg app_sha256 "$(sha256sum "$proof_dir/browser-app.js" | awk '{print $1}')" \
  '{root_status:200,app_status:200,index_sha256:$index_sha256,app_sha256:$app_sha256}' >"$proof_dir/browser-smoke.json"

final_shutdown_before="$(journalctl -u "$service_name" --since "@$service_started_epoch" --no-pager | grep -c 'msg=shutdown_complete' || true)"
systemctl stop "$service_name"
assert_probes_closed final-stopped
final_shutdown_after="$(journalctl -u "$service_name" --since "@$service_started_epoch" --no-pager | grep -c 'msg=shutdown_complete' || true)"
[[ "$final_shutdown_after" -gt "$final_shutdown_before" ]] || die "final candidate stop did not log shutdown_complete"
capture_journal
grep -Fq 'otelcontext' "$proof_dir/journal.log" || die "journal identity evidence is missing"

cp "$env_path" "$proof_dir/environment.redacted"
sed -i -E 's/^(API_KEY|DB_DSN)=.*/\1=[redacted]/' "$proof_dir/environment.redacted"
stat -c '%U:%G %a %n' "$opt_root/current" "$rollback_root" "$bundle" >>"$proof_dir/file-metadata.txt"

perform_cleanup || die "disposable cleanup failed"
jq -e '.passed == true' "$proof_dir/cleanup.json" >/dev/null || die "cleanup proof is not complete"

jq -n \
  --arg schema_version "$proof_schema" --arg tag "$tag" --arg sha "$candidate_sha" \
  --arg previous_tag "$previous_tag" --arg candidate_archive_sha256 "$candidate_archive_sha" \
  --arg candidate_binary_sha256 "$candidate_binary_sha" --arg systemd_version "$systemd_major" \
  --arg initial_fingerprint "$initial_fingerprint" --arg restart_fingerprint "$restart_fingerprint" \
  --arg crash_fingerprint "$crash_fingerprint" --arg rollback_fingerprint "$rollback_fingerprint" \
  --arg upgrade_fingerprint "$upgrade_fingerprint" --arg backup_manifest_sha256 "$backup_manifest_sha" \
  --argjson source_proof "$source_proof" --argjson initial_pid "$initial_pid" --argjson restart_pid "$restart_pid" \
  --argjson crash_pid "$crash_pid" --argjson rollback_pid "$rollback_pid" --argjson upgrade_pid "$upgrade_pid" \
  --argjson automatic_restarts "$automatic_restarts_after" \
  '{
    schema_version:$schema_version,tag:$tag,sha:$sha,source_proof:($source_proof == 1),certifying:($source_proof == 0),
    previous_tag:$previous_tag,candidate_archive_sha256:$candidate_archive_sha256,candidate_binary_sha256:$candidate_binary_sha256,
    systemd_version:$systemd_version,pids:{initial:$initial_pid,restart:$restart_pid,crash_recovery:$crash_pid,rollback_previous:$rollback_pid,upgrade_candidate:$upgrade_pid},
    automatic_restarts:$automatic_restarts,
    fingerprints:{initial:$initial_fingerprint,restart:$restart_fingerprint,crash_recovery:$crash_fingerprint,rollback:$rollback_fingerprint,upgrade:$upgrade_fingerprint},
    backup_manifest_sha256:$backup_manifest_sha256,
    assertions:[
      {name:"signed_archive_verified",passed:true},{name:"systemd_249_or_newer",passed:true},{name:"unit_verified",passed:true},
      {name:"production_paths_and_modes_verified",passed:true},{name:"explicit_migration_exact",passed:true},{name:"live_and_ready_within_30_seconds",passed:true},
      {name:"same_version_restart_preserved_fingerprint",passed:true},{name:"one_bounded_crash_restart",passed:true},
      {name:"readiness_failure_did_not_restart",passed:true},{name:"quiesced_backup_complete",passed:true},
      {name:"fresh_target_previous_binary_rollback",passed:true},{name:"candidate_upgrade_preserved_fingerprint",passed:true},
      {name:"embedded_browser_smoke",passed:true},{name:"journal_captured",passed:true},{name:"cleanup_complete",passed:true}
    ]
  }' >"$proof_dir/systemd-proof-v1.json"

jq -e '
  .schema_version == "otelcontext.systemd-proof.v1" and
  .automatic_restarts == 1 and
  (.fingerprints | [.initial,.restart,.crash_recovery,.rollback,.upgrade] | unique | length) == 1 and
  all(.assertions[]; .passed == true)
' "$proof_dir/systemd-proof-v1.json" >/dev/null

echo "systemd proof passed for $tag at $candidate_sha"
