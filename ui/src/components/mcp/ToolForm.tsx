import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  Button,
  ButtonGroup,
  FormField,
  Input,
  NumberInput,
  Select,
  DatePicker,
  Textarea,
  Space,
  Alert,
} from '@ossrandom/design-system';
import type { McpTool } from '../../lib/mcpClient';
import {
  fieldsForTool,
  toArguments,
  SEVERITY_OPTIONS,
  DATE_PRESETS,
  type ToolField,
} from './toolFields';

interface ToolFormProps {
  readonly tool: McpTool;
  readonly running: boolean;
  readonly onRun: (args: Record<string, unknown>) => void;
  /** Pre-fill values (e.g. re-run from History). */
  readonly initialValues?: Record<string, string>;
}

// Layout-only escape hatches (sanctioned): grid + spacing for the field rows.
const fieldGrid: React.CSSProperties = {
  display: 'grid',
  gap: 12,
  gridTemplateColumns: 'minmax(0, 1fr)',
};
const presetRow: React.CSSProperties = { display: 'flex', gap: 8, marginTop: 6 };

function toRfc3339(d: Date | null): string {
  return d ? d.toISOString() : '';
}

/** Fetches the service catalog once for service / service_name pickers. */
function useServiceOptions() {
  const [services, setServices] = useState<string[]>([]);
  useEffect(() => {
    const ctrl = new AbortController();
    fetch('/api/metadata/services', { signal: ctrl.signal })
      .then((r) => (r.ok ? r.json() : []))
      .then((list: unknown) => {
        if (Array.isArray(list)) setServices(list.filter((s): s is string => typeof s === 'string'));
      })
      .catch(() => {
        /* non-fatal — the field falls back to free-text guidance below */
      });
    return () => ctrl.abort();
  }, []);
  return services;
}

export default function ToolForm({ tool, running, onRun, initialValues }: ToolFormProps) {
  const fields = useMemo(() => fieldsForTool(tool), [tool]);
  const services = useServiceOptions();
  // Field state seeds from initialValues. The parent re-mounts this component
  // (via `key`) whenever the tool or the re-run seed changes, so there is no
  // prop-sync effect to fall out of date.
  const [values, setValues] = useState<Record<string, string>>(initialValues ?? {});
  const [rawOpen, setRawOpen] = useState(false);
  const [rawText, setRawText] = useState('');
  const [rawError, setRawError] = useState<string | null>(null);

  const setField = useCallback((name: string, value: string) => {
    setValues((prev) => ({ ...prev, [name]: value }));
  }, []);

  const args = useMemo(() => toArguments(fields, values), [fields, values]);

  // Keep the raw-JSON editor in sync with the form while it's open & untouched.
  useEffect(() => {
    if (rawOpen) setRawText(JSON.stringify(args, null, 2));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rawOpen]);

  const applyRaw = useCallback(
    (text: string) => {
      setRawText(text);
      try {
        const parsed = text.trim() ? JSON.parse(text) : {};
        if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
          setRawError('Arguments must be a JSON object.');
          return;
        }
        const next: Record<string, string> = {};
        for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
          next[k] = typeof v === 'string' ? v : String(v);
        }
        setValues(next);
        setRawError(null);
      } catch {
        setRawError('Invalid JSON.');
      }
    },
    [],
  );

  const missingRequired = fields
    .filter((f) => f.required && !values[f.name])
    .map((f) => f.name);

  const handleRun = () => {
    if (missingRequired.length > 0) return;
    onRun(args);
  };

  return (
    <Space direction="vertical" size="md" style={{ width: '100%' }}>
      <div style={fieldGrid}>
        {fields.length === 0 && (
          <Alert severity="info" title="No parameters">
            This tool takes no arguments — run it directly.
          </Alert>
        )}
        {fields.map((field) => (
          <FieldControl
            key={field.name}
            field={field}
            value={values[field.name] ?? ''}
            services={services}
            onChange={(v) => setField(field.name, v)}
          />
        ))}
      </div>

      <div>
        <Button variant="link" size="sm" onClick={() => setRawOpen((o) => !o)}>
          {rawOpen ? 'Hide raw arguments' : 'Edit raw arguments'}
        </Button>
        {rawOpen && (
          <div style={{ marginTop: 8 }}>
            <Textarea
              value={rawText}
              rows={6}
              status={rawError ? 'error' : 'default'}
              onChange={(v) => applyRaw(v)}
            />
            {rawError && (
              <Alert severity="danger" style={{ marginTop: 8 }}>
                {rawError}
              </Alert>
            )}
          </div>
        )}
      </div>

      <Space direction="horizontal" size="sm" align="center">
        <Button
          variant="primary"
          loading={running}
          disabled={missingRequired.length > 0}
          onClick={handleRun}
        >
          Run tool
        </Button>
        {missingRequired.length > 0 && (
          <span style={{ fontSize: 13, opacity: 0.7 }}>
            Required: {missingRequired.join(', ')}
          </span>
        )}
      </Space>
    </Space>
  );
}

interface FieldControlProps {
  readonly field: ToolField;
  readonly value: string;
  readonly services: string[];
  readonly onChange: (value: string) => void;
}

function FieldControl({ field, value, services, onChange }: FieldControlProps) {
  const required = field.required;

  let control: React.ReactNode;
  switch (field.kind) {
    case 'service':
      control =
        services.length > 0 ? (
          <Select
            options={services.map((s) => ({ label: s, value: s }))}
            value={value || undefined}
            searchable
            clearable
            placeholder="Select a service"
            onChange={(v) => onChange(String(v))}
          />
        ) : (
          <Input
            value={value}
            placeholder="service name"
            onChange={(v) => onChange(v)}
          />
        );
      break;
    case 'severity':
      control = (
        <Select
          options={SEVERITY_OPTIONS.map((s) => ({ label: s, value: s }))}
          value={value || undefined}
          clearable
          placeholder="Any severity"
          onChange={(v) => onChange(String(v))}
        />
      );
      break;
    case 'datetime':
      control = (
        <div>
          <DatePicker
            value={value ? new Date(value) : undefined}
            format="yyyy-MM-dd HH:mm"
            placeholder="now"
            onChange={(d) => onChange(toRfc3339(d))}
          />
          <div style={presetRow}>
            <ButtonGroup size="sm">
              {DATE_PRESETS.map((p) => (
                <Button
                  key={p.label}
                  variant="secondary"
                  size="sm"
                  onClick={() => onChange(new Date(Date.now() - p.ms).toISOString())}
                >
                  -{p.label}
                </Button>
              ))}
            </ButtonGroup>
            {value && (
              <Button variant="ghost" size="sm" onClick={() => onChange('')}>
                Clear
              </Button>
            )}
          </div>
        </div>
      );
      break;
    case 'duration':
      control = (
        <Input
          value={value}
          placeholder="15m"
          onChange={(v) => onChange(v)}
        />
      );
      break;
    case 'number':
      control = (
        <NumberInput
          value={value === '' ? undefined : Number(value)}
          min={field.min}
          max={field.max}
          onChange={(n) => {
            let next = n;
            if (field.max !== undefined && next > field.max) next = field.max;
            if (field.min !== undefined && next < field.min) next = field.min;
            onChange(Number.isNaN(next) ? '' : String(next));
          }}
        />
      );
      break;
    default:
      control = (
        <Input value={value} onChange={(v) => onChange(v)} placeholder={field.name} />
      );
  }

  return (
    <FormField label={field.name} required={required} hint={field.description}>
      {control}
    </FormField>
  );
}
