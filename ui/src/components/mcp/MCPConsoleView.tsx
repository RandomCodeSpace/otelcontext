import { useMemo, useState } from 'react';
import {
  Menu,
  Select,
  PageHeader,
  Tabs,
  Button,
  IconButton,
  Alert,
  Skeleton,
  Badge,
  CodeBlock,
  Space,
} from '@ossrandom/design-system';
import { Settings } from 'lucide-react';
import { useMediaQuery } from '../../hooks/useMediaQuery';
import { BP } from '../../lib/breakpoints';
import { useMcpTools } from '../../hooks/useMcpTools';
import { useMcpCall, type HistoryEntry } from '../../hooks/useMcpCall';
import type { McpTool } from '../../lib/mcpClient';
import ToolForm from './ToolForm';
import ToolResult from './ToolResult';
import ToolHistory from './ToolHistory';
import LiveStream from './LiveStream';
import SettingsDrawer, { useMcpSettings } from './SettingsDrawer';

// Tools are grouped by data source per the design spec: instant in-memory
// GraphRAG tools vs DB / fallback tools. Order within each group is fixed; any
// tool the live catalog returns that isn't listed here lands under "Other".
const INSTANT_TOOLS = [
  'get_anomaly_timeline',
  'get_service_map',
  'get_service_health',
  'root_cause_analysis',
  'impact_analysis',
];
const DB_TOOLS = ['trace_graph', 'search_logs'];

type WorkbenchTab = 'run' | 'definition' | 'history';

// Layout-only escape hatches (sanctioned): the list-detail split + scroll.
const layout: React.CSSProperties = { display: 'flex', height: '100%', minHeight: 0 };
const rail: React.CSSProperties = {
  width: 280,
  flex: '0 0 280px',
  borderRight: '1px solid var(--rcs-color-border, rgba(127,127,127,0.18))',
  overflowY: 'auto',
  padding: 12,
};
const pane: React.CSSProperties = {
  flex: 1,
  minWidth: 0,
  display: 'flex',
  flexDirection: 'column',
  overflow: 'hidden',
};
const paneBody: React.CSSProperties = { flex: 1, minHeight: 0, overflowY: 'auto', padding: 16 };

function groupTools(tools: McpTool[]) {
  const byName = new Map(tools.map((t) => [t.name, t]));
  const instant = INSTANT_TOOLS.filter((n) => byName.has(n)).map((n) => byName.get(n)!);
  const db = DB_TOOLS.filter((n) => byName.has(n)).map((n) => byName.get(n)!);
  const known = new Set([...INSTANT_TOOLS, ...DB_TOOLS]);
  const other = tools.filter((t) => !known.has(t.name));
  return { instant, db, other };
}

export default function MCPConsoleView() {
  const isMobile = useMediaQuery(BP.coarse);
  const { settings, save } = useMcpSettings();
  const callOpts = useMemo(
    () => ({ apiKey: settings.apiKey || undefined, tenant: settings.tenant || undefined }),
    [settings.apiKey, settings.tenant],
  );

  const { tools, loading, error, reload } = useMcpTools(callOpts);
  const { state, call, reset, history, clearHistory } = useMcpCall(callOpts);

  const [selected, setSelected] = useState<string | null>(null);
  const [tab, setTab] = useState<WorkbenchTab>('run');
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [rerunSeed, setRerunSeed] = useState<Record<string, string> | undefined>(undefined);
  // Bumped on each re-run so the keyed ToolForm re-mounts and re-seeds even
  // when the same tool is re-run with identical arguments.
  const [formNonce, setFormNonce] = useState(0);

  const groups = useMemo(() => groupTools(tools), [tools]);

  // Effective selection: the user's pick if it's still in the catalog,
  // otherwise the first advertised tool. Derived — no effect/setState needed,
  // so a volatile catalog never leaves a dangling selection.
  const effectiveSelected =
    selected && tools.some((t) => t.name === selected)
      ? selected
      : (tools[0]?.name ?? null);

  const activeTool = tools.find((t) => t.name === effectiveSelected) ?? null;

  const selectTool = (name: string) => {
    setSelected(name);
    setTab('run');
    setRerunSeed(undefined);
    setFormNonce((n) => n + 1);
    reset();
  };

  const handleRerun = (entry: HistoryEntry) => {
    const seed: Record<string, string> = {};
    for (const [k, v] of Object.entries(entry.args)) {
      seed[k] = typeof v === 'string' ? v : String(v);
    }
    setSelected(entry.tool);
    setRerunSeed(seed);
    setTab('run');
    setFormNonce((n) => n + 1);
    reset();
  };

  const menuItems = useMemo(() => {
    type MenuEntry = Parameters<typeof Menu>[0]['items'][number];
    const items: MenuEntry[] = [];
    if (groups.instant.length > 0) {
      items.push({ type: 'label', label: 'In-memory (instant)' });
      for (const t of groups.instant) items.push({ key: t.name, label: t.name });
    }
    if (groups.db.length > 0) {
      items.push({ type: 'label', label: 'DB / fallback' });
      for (const t of groups.db) items.push({ key: t.name, label: t.name });
    }
    if (groups.other.length > 0) {
      items.push({ type: 'label', label: 'Other' });
      for (const t of groups.other) items.push({ key: t.name, label: t.name });
    }
    return items;
  }, [groups]);

  const selectOptions = useMemo(
    () => tools.map((t) => ({ label: t.name, value: t.name })),
    [tools],
  );

  // --- Rail (desktop) / Select (mobile) ---
  const railContent = loading ? (
    <Space direction="vertical" size="sm" style={{ width: '100%' }}>
      <Skeleton variant="text" lines={7} />
    </Space>
  ) : (
    <Menu
      items={menuItems}
      mode="inline"
      selectedKeys={effectiveSelected ? [effectiveSelected] : []}
      onSelect={selectTool}
    />
  );

  const headerActions = (
    <Space direction="horizontal" size="sm" align="center">
      {settings.apiKey && <Badge tone="info">auth on</Badge>}
      {settings.tenant && <Badge tone="subtle">tenant: {settings.tenant}</Badge>}
      <IconButton
        aria-label="Connection settings"
        variant="ghost"
        size="sm"
        icon={<Settings size={15} />}
        onClick={() => setSettingsOpen(true)}
      />
    </Space>
  );

  return (
    <div style={layout}>
      {!isMobile && <div style={rail}>{railContent}</div>}

      <div style={pane}>
        <PageHeader
          size="sm"
          title={activeTool ? activeTool.name : 'MCP console'}
          subtitle={activeTool?.description}
          actions={headerActions}
        />

        <div style={paneBody}>
          {error && (
            <Alert
              severity="danger"
              title="Can't reach the MCP endpoint"
              action={
                <Button size="sm" variant="secondary" onClick={reload}>
                  Retry
                </Button>
              }
              style={{ marginBottom: 16 }}
            >
              {error} — POST /mcp failed. Check the server is up and your API key
              under settings.
            </Alert>
          )}

          {isMobile && !loading && tools.length > 0 && (
            <div style={{ marginBottom: 16 }}>
              <Select
                options={selectOptions}
                value={effectiveSelected ?? undefined}
                searchable
                placeholder="Select a tool"
                onChange={(v) => selectTool(String(v))}
              />
            </div>
          )}

          {loading && <Skeleton variant="rect" height={160} />}

          {!loading && tools.length === 0 && !error && (
            <Alert severity="warning" title="No tools advertised">
              tools/list returned an empty surface.
            </Alert>
          )}

          {activeTool && (
            <>
              <Tabs
                variant="line"
                value={tab}
                items={[
                  { key: 'run', label: 'Run' },
                  { key: 'definition', label: 'Definition' },
                  { key: 'history', label: 'History' },
                ]}
                onChange={(k) => setTab(k as WorkbenchTab)}
              />

              <div style={{ marginTop: 16 }}>
                {tab === 'run' && (
                  <Space direction="vertical" size="lg" style={{ width: '100%' }}>
                    <ToolForm
                      key={`${activeTool.name}#${formNonce}`}
                      tool={activeTool}
                      running={state.loading}
                      initialValues={rerunSeed}
                      onRun={(args) => call(activeTool.name, args)}
                    />
                    <ToolResult tool={activeTool.name} state={state} />
                    <LiveStream apiKey={settings.apiKey} />
                  </Space>
                )}

                {tab === 'definition' && (
                  <CodeBlock
                    code={JSON.stringify(activeTool, null, 2)}
                    language="json"
                    copyable
                    wrap
                  />
                )}

                {tab === 'history' && (
                  <ToolHistory
                    history={history}
                    tenant={settings.tenant || undefined}
                    onClear={clearHistory}
                    onRerun={handleRerun}
                  />
                )}
              </div>
            </>
          )}
        </div>
      </div>

      <SettingsDrawer
        open={settingsOpen}
        settings={settings}
        onClose={() => setSettingsOpen(false)}
        onSave={save}
      />
    </div>
  );
}
