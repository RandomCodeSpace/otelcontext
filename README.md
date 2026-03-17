# OtelContext

[![Security Scan](https://github.com/RandomCodeSpace/otelcontext/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/RandomCodeSpace/otelcontext/actions/workflows/ci.yml)
[![OpenSSF Scan](https://github.com/RandomCodeSpace/otelcontext/actions/workflows/ci.yml/badge.svg?branch=main&label=OpenSSF%20scan)](https://github.com/RandomCodeSpace/otelcontext/actions/workflows/ci.yml)
[![OpenSSF Score](https://api.scorecard.dev/projects/github.com/RandomCodeSpace/otelcontext/badge)](https://scorecard.dev/viewer/?uri=github.com/RandomCodeSpace/otelcontext)
[![Release](https://img.shields.io/github/v/release/RandomCodeSpace/otelcontext)](https://github.com/RandomCodeSpace/otelcontext/releases)
[![Beta](https://img.shields.io/github/v/release/RandomCodeSpace/otelcontext?include_prereleases&label=beta)](https://github.com/RandomCodeSpace/otelcontext/releases)
![Go Version](https://img.shields.io/github/go-mod/go-version/RandomCodeSpace/otelcontext)
![React](https://img.shields.io/badge/frontend-React%20v18-61dafb?logo=react)

OtelContext is an integrated observability and AI analysis platform.

## Getting Started

### Installation
```bash
go install github.com/RandomCodeSpace/otelcontext@latest
```

### Running
Simply run the binary:
```bash
otelcontext
```
By default, OtelContext will use an embedded SQLite database (`otelcontext.db`) in the current directory. No configuration is required.

### Configuration (Optional)
You can configure the database using environment variables or a `.env` file:

- **MySQL**:
  ```bash
  DB_DRIVER=mysql
  DB_DSN=root:password@tcp(localhost:3306)/otelcontext?charset=utf8mb4&parseTime=True&loc=Local
  ```
- **SQLite** (Default):
  ```bash
  DB_DRIVER=sqlite
  DB_DSN=otelcontext.db
  ```

## Features
- **Traces**: OTLP Trace ingestion and visualization.
- **Logs**: Structured logging with AI-powered insights.
- **Dashboard**: Real-time metrics and traffic analysis.

## OTLP Integration
OtelContext acts as an OTLP Receiver (gRPC) on port `4317` by default.

### As an OTel Collector Target
You can configure any OpenTelemetry Collector to export data to OtelContext.

```yaml
exporters:
  otlp/otelcontext:
    endpoint: "localhost:4317"
    tls:
      insecure: true

service:
  pipelines:
    traces:
      exporters: [otlp/otelcontext]
    logs:
      exporters: [otlp/otelcontext]
```
See `docs/otel-collector-example.yaml` for a full configuration example.