#!/usr/bin/env python3
"""Bounded main-commit smoke: isolated SQLite migration, readiness, clean stop."""
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return str(sock.getsockname()[1])


def main():
    if len(sys.argv) != 1:
        raise SystemExit("usage: python3 scripts/smoke-startup.py (no arguments)")
    # Only run the artifact built at this checkout's fixed CI output location.
    binary = str(Path(__file__).resolve().parents[1] / ".ci-bin" / "otelcontext")
    with tempfile.TemporaryDirectory(prefix="otelcontext-startup-") as directory:
        http_port, grpc_port = free_port(), free_port()
        while grpc_port == http_port:
            grpc_port = free_port()
        env = dict(os.environ, DB_DRIVER="sqlite", DB_DSN=f"{directory}/main.db",
                   AGGREGATE_MODE="aggregate", AGGREGATE_DB_PATH=f"{directory}/aggregate.db",
                   DLQ_PATH=f"{directory}/dlq", HTTP_PORT=http_port, GRPC_PORT=grpc_port,
                   PPROF_ADDR="", PROMETHEUS_URL="", PROMETHEUS_REQUIRED="false",
                   PROMQL_LOCAL_ENABLED="false", OTEL_EXPORTER_OTLP_ENDPOINT="")
        subprocess.run([binary, "migrate", "up"], env=env, cwd=directory,
                       check=True, timeout=30)
        log_path = Path(directory) / "startup.log"
        with log_path.open("w") as log:
            proc = subprocess.Popen([binary], env=env, cwd=directory,
                                    stdout=log, stderr=subprocess.STDOUT)
            try:
                deadline = time.monotonic() + 30
                for endpoint in ("live", "ready"):
                    last_error = None
                    while True:
                        if proc.poll() is not None:
                            raise RuntimeError(f"server exited before /{endpoint}")
                        try:
                            with urllib.request.urlopen(
                                f"http://127.0.0.1:{http_port}/{endpoint}", timeout=1
                            ) as response:
                                if response.status == 200:
                                    break
                                last_error = RuntimeError(f"HTTP {response.status}")
                        except (OSError, urllib.error.URLError) as error:
                            # Startup may briefly refuse connections or return 503.
                            # Preserve the last failure if the bounded retry expires.
                            last_error = error
                        if time.monotonic() >= deadline:
                            raise TimeoutError(f"/{endpoint} did not become healthy") from last_error
                        time.sleep(0.2)
                proc.terminate()
                if proc.wait(timeout=30) != 0:
                    raise RuntimeError("server did not shut down cleanly")
                print("PASS: migrate up, /live, /ready, SIGTERM exit 0")
            finally:
                if proc.poll() is None:
                    proc.kill()
                    proc.wait(timeout=5)
                print(log_path.read_text(), end="")


if __name__ == "__main__":
    main()
