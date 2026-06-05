import { useCallback, useEffect, useState } from 'react';
import { Drawer, FormField, Input, Button, Alert, Space } from '@ossrandom/design-system';

export interface McpSettings {
  apiKey: string;
  tenant: string;
}

const STORAGE_KEY = 'otelcontext.mcp.settings';

function read(): McpSettings {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEY);
    if (!raw) return { apiKey: '', tenant: '' };
    const parsed = JSON.parse(raw) as Partial<McpSettings>;
    return { apiKey: parsed.apiKey ?? '', tenant: parsed.tenant ?? '' };
  } catch {
    return { apiKey: '', tenant: '' };
  }
}

/**
 * Reads/persists the MCP auth + tenant settings. Persisted to sessionStorage
 * ONLY — never localStorage — so the API key never outlives the browser tab.
 */
export function useMcpSettings() {
  const [settings, setSettings] = useState<McpSettings>(() => read());

  const save = useCallback((next: McpSettings) => {
    setSettings(next);
    try {
      sessionStorage.setItem(STORAGE_KEY, JSON.stringify(next));
    } catch {
      /* sessionStorage unavailable — settings stay in-memory only. */
    }
  }, []);

  return { settings, save };
}

interface SettingsDrawerProps {
  readonly open: boolean;
  readonly settings: McpSettings;
  readonly onClose: () => void;
  readonly onSave: (next: McpSettings) => void;
}

export default function SettingsDrawer({ open, settings, onClose, onSave }: SettingsDrawerProps) {
  const [apiKey, setApiKey] = useState(settings.apiKey);
  const [tenant, setTenant] = useState(settings.tenant);

  // Re-seed local edit state from the saved settings each time the drawer opens.
  useEffect(() => {
    if (open) {
      setApiKey(settings.apiKey);
      setTenant(settings.tenant);
    }
  }, [open, settings.apiKey, settings.tenant]);

  return (
    <Drawer
      open={open}
      title="Connection settings"
      placement="right"
      width={400}
      onClose={onClose}
      footer={
        <Space direction="horizontal" size="sm" justify="end">
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            onClick={() => {
              onSave({ apiKey: apiKey.trim(), tenant: tenant.trim() });
              onClose();
            }}
          >
            Save
          </Button>
        </Space>
      }
    >
      <Space direction="vertical" size="md" style={{ width: '100%' }}>
        <Alert severity="info" icon={false}>
          Stored in sessionStorage only — cleared when this tab closes. Leave
          the API key empty for dev servers with auth disabled.
        </Alert>
        <FormField label="API key" hint="Sent as Authorization: Bearer <key>.">
          <Input
            type="password"
            value={apiKey}
            placeholder="(none)"
            onChange={(v) => setApiKey(v)}
          />
        </FormField>
        <FormField label="Tenant" hint="Sent as X-Tenant-ID. Defaults to the server's default tenant.">
          <Input value={tenant} placeholder="default" onChange={(v) => setTenant(v)} />
        </FormField>
      </Space>
    </Drawer>
  );
}
