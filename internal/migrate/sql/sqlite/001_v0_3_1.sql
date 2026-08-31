CREATE TABLE IF NOT EXISTS traces (
    id integer PRIMARY KEY AUTOINCREMENT,
    tenant_id text NOT NULL DEFAULT 'default',
    trace_id text NOT NULL,
    service_name text,
    duration integer,
    status text,
    timestamp datetime,
    created_at datetime,
    updated_at datetime,
    deleted_at datetime
)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_traces_deleted_at ON traces (deleted_at)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_traces_timestamp ON traces (timestamp)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_traces_duration ON traces (duration)
-- migrate:split
CREATE UNIQUE INDEX IF NOT EXISTS idx_traces_tenant_trace_id ON traces (tenant_id, trace_id)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_traces_tenant_service ON traces (tenant_id, service_name)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_traces_tenant_ts ON traces (tenant_id, timestamp)
-- migrate:split
CREATE TABLE IF NOT EXISTS spans (
    id integer PRIMARY KEY AUTOINCREMENT,
    tenant_id text NOT NULL DEFAULT 'default',
    trace_id text NOT NULL,
    span_id text NOT NULL,
    parent_span_id text,
    operation_name text,
    start_time datetime,
    end_time datetime,
    duration integer,
    service_name text,
    status text DEFAULT 'STATUS_CODE_UNSET',
    attributes_json blob
)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_spans_status ON spans (status)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_spans_operation_name ON spans (operation_name)
-- migrate:split
CREATE UNIQUE INDEX IF NOT EXISTS idx_spans_tenant_trace_span ON spans (tenant_id, trace_id, span_id)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_spans_tenant_service_start ON spans (tenant_id, service_name, start_time)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_spans_tenant_trace ON spans (tenant_id, trace_id)
-- migrate:split
CREATE TABLE IF NOT EXISTS metric_buckets (
    id integer PRIMARY KEY AUTOINCREMENT,
    tenant_id text NOT NULL DEFAULT 'default',
    name text NOT NULL,
    service_name text NOT NULL,
    time_bucket datetime NOT NULL,
    min real,
    max real,
    sum real,
    count integer,
    attributes_json blob
)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_metric_buckets_time_bucket ON metric_buckets (time_bucket)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_metrics_tenant_service_bucket ON metric_buckets (tenant_id, service_name, time_bucket)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_metrics_tenant_name_bucket ON metric_buckets (tenant_id, name, time_bucket)
-- migrate:split
CREATE TABLE IF NOT EXISTS logs (
    id integer PRIMARY KEY AUTOINCREMENT,
    tenant_id text NOT NULL DEFAULT 'default',
    trace_id text,
    span_id text,
    severity text,
    body text,
    service_name text,
    attributes_json blob,
    ai_insight blob,
    timestamp datetime
)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON logs (timestamp)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_logs_trace_id ON logs (trace_id)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_logs_tenant_severity ON logs (tenant_id, severity)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_logs_tenant_service ON logs (tenant_id, service_name)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_logs_tenant_ts ON logs (tenant_id, timestamp)
-- migrate:split
CREATE TABLE IF NOT EXISTS investigations (
    tenant_id text NOT NULL DEFAULT 'default',
    id text PRIMARY KEY,
    created_at datetime,
    status text,
    severity text,
    trigger_service text,
    trigger_operation text,
    error_message text,
    root_service text,
    root_operation text,
    causal_chain text,
    trace_ids text,
    error_logs text,
    anomalous_metrics text,
    affected_services text,
    span_chain text
)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_investigations_trigger_service ON investigations (trigger_service)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_investigations_tenant_created ON investigations (tenant_id, created_at)
-- migrate:split
CREATE TABLE IF NOT EXISTS drain_templates (
    tenant_id text NOT NULL DEFAULT 'default',
    id integer,
    tokens text NOT NULL,
    count integer,
    first_seen datetime,
    last_seen datetime,
    sample text,
    PRIMARY KEY (tenant_id, id)
)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_drain_templates_last_seen ON drain_templates (last_seen)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_drain_templates_first_seen ON drain_templates (first_seen)
