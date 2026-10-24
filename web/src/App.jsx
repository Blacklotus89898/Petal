import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  AppShell,
  Badge,
  Card,
  Code,
  Drawer,
  Group,
  Loader,
  ScrollArea,
  Table,
  Text,
  Timeline,
  Title,
  Tooltip,
} from '@mantine/core'
import { useDisclosure, useInterval } from '@mantine/hooks'

const STATUS_COLOR = { queued: 'gray', running: 'blue', succeeded: 'green', failed: 'red' }

const fmtTime = ts => (ts ? new Date(ts).toLocaleTimeString() : '—')

function fmtDuration(run) {
  if (!run.started_at) return '—'
  const end = run.finished_at ? new Date(run.finished_at) : new Date()
  return `${((end - new Date(run.started_at)) / 1000).toFixed(1)}s`
}

function eventLabel(ev) {
  const p = JSON.parse(ev.payload || '{}')
  switch (ev.kind) {
    case 'queued': return `queued (${p.trigger || 'manual'})`
    case 'step.started': return `step "${p.step}" started (${p.image})`
    case 'step.finished':
      return p.canceled ? `step "${p.step}" canceled`
        : p.error ? `step "${p.step}" failed: ${p.error}`
        : `step "${p.step}" finished (exit ${p.exit})`
    case 'output': return `output: ${p.output}`
    case 'succeeded': return 'run succeeded'
    case 'failed': return 'run failed'
    default: return ev.kind
  }
}

function RunDrawer({ runId, opened, onClose }) {
  const [detail, setDetail] = useState(null)
  const [error, setError] = useState('')
  const [logs, setLogs] = useState({})

  useEffect(() => {
    if (!opened || !runId) return
    setDetail(null); setLogs({}); setError('')
    let alive = true
    ;(async () => {
      try {
        const res = await fetch(`/api/runs/${runId}`)
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        const data = await res.json()
        if (!alive) return
        setDetail(data)
        const steps = data.events.filter(e => e.kind === 'step.started').map(e => JSON.parse(e.payload).step)
        await Promise.all(steps.map(async step => {
          const r = await fetch(`/api/runs/${runId}/logs/${step}`)
          const text = r.ok ? await r.text() : `(no log: HTTP ${r.status})`
          if (alive) setLogs(prev => ({ ...prev, [step]: text }))
        }))
      } catch (e) {
        if (alive) setError(e.message)
      }
    })()
    return () => { alive = false }
  }, [opened, runId])

  const stepNames = [...new Set((detail?.events || [])
    .filter(e => e.kind === 'step.started')
    .map(e => JSON.parse(e.payload).step))]

  return (
    <Drawer opened={opened} onClose={onClose} title={<span style={{ fontFamily: 'monospace' }}>{runId}</span>} position="right" size="xl">
      {error && <Alert color="red" mb="md">{error}</Alert>}
      {!detail ? (
        <Group justify="center" p="xl"><Loader /></Group>
      ) : (
        <>
          <Text size="sm" fw={600} mb="xs">Timeline</Text>
          <Timeline active={detail.events.length} bulletSize={12} lineWidth={2} mb="md">
            {detail.events.map(ev => (
              <Timeline.Item key={ev.id} title={eventLabel(ev)}>
                <Text size="xs" c="dimmed">{new Date(ev.ts).toLocaleTimeString()}</Text>
              </Timeline.Item>
            ))}
          </Timeline>

          {stepNames.map(step => (
            <div key={step} mb="md">
              <Group justify="space-between" mb={4}>
                <Text size="sm" fw={600}>{step} — logs</Text>
              </Group>
              <ScrollArea.Autosize mah={300}>
                <Code block ta="left" style={{ whiteSpace: 'pre-wrap' }}>
                  {logs[step] ?? 'loading…'}
                </Code>
              </ScrollArea.Autosize>
            </div>
          ))}
        </>
      )}
    </Drawer>
  )
}

export default function App() {
  const [runs, setRuns] = useState(null)
  const [pipelines, setPipelines] = useState([])
  const [error, setError] = useState('')
  const [selectedRun, setSelectedRun] = useState(null)
  const [drawerOpened, { open: openDrawer, close: closeDrawer }] = useDisclosure(false)

  const refresh = useCallback(async () => {
    try {
      const res = await fetch('/api/runs?limit=50')
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      setRuns(await res.json())
      setError('')
    } catch (e) {
      setError(e.message)
    }
  }, [])

  useEffect(() => {
    refresh()
    fetch('/api/pipelines')
      .then(res => (res.ok ? res.json() : []))
      .then(setPipelines)
      .catch(() => {})
  }, [refresh])
  useInterval(refresh, 5000)

  const openRun = id => { setSelectedRun(id); openDrawer() }

  // Deep link: /?run=<id> opens that run's drawer (Backstage's petal:run links here).
  useEffect(() => {
    const id = new URLSearchParams(window.location.search).get('run')
    if (id) openRun(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <AppShell header={{ height: 56 }} padding="md">
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between">
          <Group gap="xs">
            <Title order={4}>🌸 Petal</Title>
            <Text size="xs" c="dimmed">homelab control plane</Text>
          </Group>
          <Tooltip label="Petal is the orchestrator; runs are triggered from Backstage, the signed webhook, or cron (ADR 0003)">
            <Badge variant="light" color="gray">read-only console</Badge>
          </Tooltip>
        </Group>
      </AppShell.Header>

      <AppShell.Main>
        {error && <Alert color="red" mb="md">Cannot reach Petal API: {error}</Alert>}
        <Card withBorder padding={0} radius="md">
          {runs === null ? (
            <Group justify="center" p="xl"><Loader /></Group>
          ) : runs.length === 0 ? (
            <Text p="xl" ta="center" c="dimmed">No runs yet — trigger one from Backstage to see the pipeline lifecycle.</Text>
          ) : (
            <Table highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Run</Table.Th>
                  <Table.Th>Pipeline</Table.Th>
                  <Table.Th>Trigger</Table.Th>
                  <Table.Th>Status</Table.Th>
                  <Table.Th>Queued</Table.Th>
                  <Table.Th>Duration</Table.Th>
                  <Table.Th>Error</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {runs.map(run => (
                  <Table.Tr key={run.id} onClick={() => openRun(run.id)} style={{ cursor: 'pointer' }}>
                    <Table.Td style={{ fontFamily: 'monospace' }}>{run.id}</Table.Td>
                    <Table.Td>
                      {run.pipeline}{run.module_fn ? `:${run.module_fn}` : ''}
                      {run.repo ? (
                        // multi-repo run (ADR 0004): alias@commit once checked out, alias@ref before
                        <Text size="xs" c="dimmed" ff="monospace">
                          {run.repo}@{run.git_sha ? run.git_sha.slice(0, 7) : run.ref}
                        </Text>
                      ) : null}
                    </Table.Td>
                    <Table.Td>
                      <Tooltip label={run.trigger_source || 'no source recorded'}>
                        <Badge variant="light" color="violet">{run.trigger_kind}</Badge>
                      </Tooltip>
                    </Table.Td>
                    <Table.Td>
                      <Badge color={STATUS_COLOR[run.status] || 'gray'} variant="dot">
                        {run.status}
                      </Badge>
                    </Table.Td>
                    <Table.Td>{fmtTime(run.queued_at)}</Table.Td>
                    <Table.Td>{fmtDuration(run)}</Table.Td>
                    <Table.Td>
                      {run.error ? (
                        <Tooltip label={run.error}><Text size="xs" c="red" truncate maw={220}>{run.error}</Text></Tooltip>
                      ) : '—'}
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          )}
        </Card>
        <Text size="xs" c="dimmed" mt="sm">
          {pipelines.length > 0
            ? `Pipelines: ${pipelines.map(p => p.name).join(', ')}`
            : 'No pipelines found in .petal/pipelines/'} · auto-refresh 5s
        </Text>
      </AppShell.Main>

      <RunDrawer runId={selectedRun} opened={drawerOpened} onClose={closeDrawer} />
    </AppShell>
  )
}
