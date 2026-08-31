CREATE TABLE IF NOT EXISTS traces (
    id bigserial PRIMARY KEY,
    tenant_id varchar(64) NOT NULL DEFAULT 'default',
    trace_id varchar(32) NOT NULL,
    service_name varchar(255),
    duration bigint,
    status varchar(50),
    timestamp timestamptz,
    created_at timestamptz,
    updated_at timestamptz,
    deleted_at timestamptz
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
    id bigserial PRIMARY KEY,
    tenant_id varchar(64) NOT NULL DEFAULT 'default',
    trace_id varchar(32) NOT NULL,
    span_id varchar(16) NOT NULL,
    parent_span_id varchar(16),
    operation_name varchar(255),
    start_time timestamptz,
    end_time timestamptz,
    duration bigint,
    service_name varchar(255),
    status varchar(50) DEFAULT 'STATUS_CODE_UNSET',
    attributes_json bytea
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
    id bigserial PRIMARY KEY,
    tenant_id varchar(64) NOT NULL DEFAULT 'default',
    name varchar(255) NOT NULL,
    service_name varchar(255) NOT NULL,
    time_bucket timestamptz NOT NULL,
    min numeric,
    max numeric,
    sum numeric,
    count bigint,
    attributes_json bytea
)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_metric_buckets_time_bucket ON metric_buckets (time_bucket)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_metrics_tenant_service_bucket ON metric_buckets (tenant_id, service_name, time_bucket)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_metrics_tenant_name_bucket ON metric_buckets (tenant_id, name, time_bucket)
-- migrate:split
CREATE TABLE IF NOT EXISTS logs (
    id bigserial PRIMARY KEY,
    tenant_id varchar(64) NOT NULL DEFAULT 'default',
    trace_id varchar(32),
    span_id varchar(16),
    severity varchar(50),
    body text,
    service_name varchar(255),
    attributes_json bytea,
    ai_insight bytea,
    timestamp timestamptz
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
    tenant_id varchar(64) NOT NULL DEFAULT 'default',
    id varchar(64) PRIMARY KEY,
    created_at timestamptz,
    status varchar(20),
    severity varchar(20),
    trigger_service varchar(255),
    trigger_operation varchar(255),
    error_message text,
    root_service varchar(255),
    root_operation varchar(255),
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
    tenant_id varchar(64) NOT NULL DEFAULT 'default',
    id bigint NOT NULL,
    tokens text NOT NULL,
    count bigint,
    first_seen timestamptz,
    last_seen timestamptz,
    sample text,
    PRIMARY KEY (tenant_id, id)
)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_drain_templates_last_seen ON drain_templates (last_seen)
-- migrate:split
CREATE INDEX IF NOT EXISTS idx_drain_templates_first_seen ON drain_templates (first_seen)
