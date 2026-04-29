import { useState } from 'react'
import { Alert, Badge, Button, CodeBlock, Grid, Modal, Space, Textarea } from '@ossrandom/design-system'
import { Play } from 'lucide-react'
import type { MCPTool } from '@/types/api'

interface Props {
  tool: MCPTool
  onClose: () => void
  onCall: (name: string, args: Record<string, unknown>) => Promise<unknown>
}

function buildDefaultArgs(tool: MCPTool): Record<string, unknown> {
  const args: Record<string, unknown> = {}
  const props = tool.inputSchema?.properties || {}
  const req = tool.inputSchema?.required || []
  for (const [key, value] of Object.entries(props)) {
    args[key] = req.includes(key) ? (value.type === 'number' ? 0 : value.type === 'boolean' ? false : '') : null
  }
  return args
}

export default function ToolCallModal({ tool, onClose, onCall }: Props) {
  const [argsText, setArgsText] = useState(() => JSON.stringify(buildDefaultArgs(tool), null, 2))
  const [resultText, setResultText] = useState('')
  const [calling, setCalling] = useState(false)
  const [timing, setTiming] = useState('')
  const [error, setError] = useState('')

  const handleCall = async () => {
    let args: Record<string, unknown>
    try {
      args = JSON.parse(argsText || '{}')
    } catch (e) {
      setError(`Invalid JSON: ${String(e)}`)
      return
    }
    setCalling(true)
    setError('')
    const t0 = performance.now()
    try {
      const result = await onCall(tool.name, args)
      setResultText(JSON.stringify(result, null, 2))
      setTiming(`${Math.round(performance.now() - t0)}ms`)
    } catch (e) {
      setResultText('')
      setError(String(e))
    } finally {
      setCalling(false)
    }
  }

  const title = (
    <Space size="xs" align="center">
      <Play size={12} />
      <span>Call</span>
      <code>{tool.name}</code>
    </Space>
  )

  return (
    <Modal open onClose={onClose} title={title} description={tool.description} size="lg">
      <Space direction="vertical" size="md">
        {error && <Alert severity="danger">{error}</Alert>}
        <Grid columns={2} gap="md">
          <Grid.Col span={1}>
            <Space direction="vertical" size="sm">
              <Textarea
                value={argsText}
                onChange={(value) => setArgsText(value)}
                rows={14}
                aria-label="Arguments"
              />
              <Button variant="primary" block loading={calling} disabled={calling} onClick={handleCall}>
                {calling ? 'Executing' : 'Execute Tool'}
              </Button>
            </Space>
          </Grid.Col>
          <Grid.Col span={1}>
            <Space direction="vertical" size="sm">
              {timing && <Badge tone="subtle" size="sm">{timing}</Badge>}
              <CodeBlock language="json" code={resultText || '—'} wrap copyable />
            </Space>
          </Grid.Col>
        </Grid>
      </Space>
    </Modal>
  )
}
