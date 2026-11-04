import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
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
  SegmentedControl,
  SimpleGrid,
  Table,
  Text,
  Timeline,
  Title,
  Tooltip,
} from '@mantine/core'
import { useDisclosure, useInterval } from '@mantine/hooks'
import StepGraph, { STEP_COLOR, fmtSeconds, stepDuration } from './StepGraph'

const STATUS_COLOR = { queued: 'gray', running: 'blue', succeeded: 'green', failed: 'red', canceled: 'orange' }

// Date + time in the viewer's locale/zone ("Nov 4, 2026, 06:41:12"); the
// exact UTC instant is in the tooltip (TimeCell).
const DATE_TIME = { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' }
const fmtTime = ts => (ts ? new Date(ts).toLocaleString(undefined, DATE_TIME) : '—')

function TimeCell({ ts }) {
  if (!ts) return '—'
  return (
    <Tooltip label={new Date(ts).toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC')}>
      <Text size="sm" style={{ whiteSpace: 'nowrap' }}>{fmtTime(ts)}</Text>
    </Tooltip>
  )
}

// durationMs: wall time of a run (running runs count up to now); null = not started.
function durationMs(run) {
  if (!run.started_at) return null
  const end = run.finished_at ? new Date(run.finished_at) : new Date()
  return end - new Date(run.started_at)
}

function fmtDuration(run) {
  const ms = durationMs(run)
  if (ms == null) return '—'
  return fmtSeconds(ms / 1000)
}

// Sortable run-table columns: key → value to compare (nulls always sort last).
const STATUS_ORDER = { running: 0, queued: 1, failed: 2, canceled: 3, succeeded: 4 }
const SORT_VALUE = {
  id: r => r.id,
  pipeline: r => `${r.repo || 'self'}/${r.pipeline}`,
  trigger: r => r.trigger_kind,
  status: r => STATUS_ORDER[r.status] ?? 9,
  queued: r => (r.queued_at ? Date.parse(r.queued_at) : null),
  finished: r => (r.finished_at ? Date.parse(r.finished_at) : null),
  duration: r => durationMs(r),
}

function sortRuns(list, { key, dir }) {
  const val = SORT_VALUE[key] || SORT_VALUE.queued
  const sign = dir === 'asc' ? 1 : -1
  return [...list].sort((a, b) => {
    const x = val(a), y = val(b)
    if (x == null && y == null) return 0
    if (x == null) return 1
    if (y == null) return -1
    return (x < y ? -1 : x > y ? 1 : 0) * sign
  })
}

function SortTh({ col, sort, onSort, children }) {
  const active = sort.key === col
  return (
    <Table.Th
      onClick={() => onSort(active ? { key: col, dir: sort.dir === 'asc' ? 'desc' : 'asc' } : { key: col, dir: col === 'id' || col === 'pipeline' || col === 'trigger' ? 'asc' : 'desc' })}
      style={{ cursor: 'pointer', userSelect: 'none', whiteSpace: 'nowrap' }}
      aria-sort={active ? (sort.dir === 'asc' ? 'ascending' : 'descending') : 'none'}>
      {children} <Text span size="xs" c={active ? undefined : 'dimmed'}>{active ? (sort.dir === 'asc' ? '▲' : '▼') : '↕'}</Text>
    </Table.Th>
  )
}

function eventLabel(ev) {
  const p = JSON.parse(ev.payload || '{}')
  switch (ev.kind) {
    case 'queued': return `queued (${p.trigger || 'manual'})`
    case 'step.started': return `step "${p.step}" started (${p.image})`
    case 'step.finished':
      return p.canceled ? `step "${p.step}" canceled${p.cause ? ` (${p.cause})` : ''}`
        : p.error ? `step "${p.step}" failed: ${p.error}`
        : `step "${p.step}" finished (exit ${p.exit})`
    case 'output': return `output: ${p.output}`
    case 'succeeded': return 'run succeeded'
    case 'failed': return 'run failed'
    // ADR 0005: DAG/fail-fast + cancellation, ADR 0004: checkout
    case 'step.skipped': return `step "${p.step}" skipped (a dependency didn't succeed)`
    case 'canceled': return p.while === 'queued' ? 'canceled before it started' : 'run canceled'
    case 'interrupted': return 'interrupted (control plane restarted)'
    case 'plan': return `plan: ${(p.steps || []).length} steps${p.dag ? ` (DAG, max ${p.max_parallel} parallel)` : ' (in order)'}`
    case 'source.resolved': return `checked out ${p.repo}@${p.ref} → ${(p.sha || '').slice(0, 7)}`
    default: return ev.kind
  }
}

const ACTIVE = new Set(['queued', 'running'])
const POLL_MS = 2500

function RunDrawer({ runId, opened, onClose }) {
  const [detail, setDetail] = useState(null)
  const [error, setError] = useState('')
  const [logs, setLogs] = useState({})
  const [now, setNow] = useState(Date.now())
  const finalLogs = useRef(new Set()) // steps whose log was fetched after they finished
  const logRefs = useRef({})

  const load = useCallback(async (alive) => {
    try {
      const res = await fetch(`/api/runs/${runId}`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const data = await res.json()
      if (!alive()) return
      setDetail(data); setError(''); setNow(Date.now())
      // Logs: refetch while a step runs; once it's finished, fetch one last time.
      const steps = (data.graph?.steps || []).filter(s => s.started_at && !finalLogs.current.has(s.name))
      await Promise.all(steps.map(async s => {
        const r = await fetch(`/api/runs/${runId}/logs/${s.name}`)
        const text = r.ok ? await r.text() : `(no log: HTTP ${r.status})`
        if (!alive()) return
        if (s.status !== 'running') finalLogs.current.add(s.name)
        setLogs(prev => ({ ...prev, [s.name]: text }))
      }))
    } catch (e) {
      if (alive()) setError(e.message)
    }
  }, [runId])

  useEffect(() => {
    if (!opened || !runId) return
    setDetail(null); setLogs({}); setError(''); finalLogs.current = new Set()
    let alive = true
    load(() => alive)
    return () => { alive = false }
  }, [opened, runId, load])

  // Live: poll while the run is queued/running; stop once it's terminal.
  const active = Boolean(detail && ACTIVE.has(detail.run.status))
  useEffect(() => {
    if (!opened || !active) return
    let alive = true
    const t = setInterval(() => load(() => alive), POLL_MS)
    const tick = setInterval(() => setNow(Date.now()), 500)
    return () => { alive = false; clearInterval(t); clearInterval(tick) }
  }, [opened, active, load])

  const graph = detail?.graph
  const steps = graph?.steps || []
  const counts = steps.reduce((m, s) => ({ ...m, [s.status]: (m[s.status] || 0) + 1 }), {})
  const jumpTo = name => logRefs.current[name]?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  const run = detail?.run

  return (
    <Drawer opened={opened} onClose={onClose} title={<span style={{ fontFamily: 'monospace' }}>{runId}</span>} position="right" size="xl">
      {error && <Alert color="red" mb="md">{error}</Alert>}
      {!detail ? (
        <Group justify="center" p="xl"><Loader /></Group>
      ) : (
        <>
          <Group gap="xs" mb="sm">
            <Badge color={STATUS_COLOR[run.status] || 'gray'} variant="filled">{run.status}</Badge>
            <Text size="sm" fw={600}>{run.pipeline}</Text>
            {run.repo && <Text size="xs" c="dimmed" ff="monospace">{run.repo}@{run.git_sha ? run.git_sha.slice(0, 7) : run.ref}</Text>}
            <Badge variant="light" color="violet">{run.trigger_kind}</Badge>
            <Text size="xs" c="dimmed">{fmtDuration(run)}</Text>
            {active && <Group gap={4}><Loader size={12} /><Text size="xs" c="dimmed">live</Text></Group>}
          </Group>
          {run.error && <Alert color="red" variant="light" mb="sm"><Text size="xs" ff="monospace">{run.error}</Text></Alert>}

          {steps.length > 0 && (
            <Card withBorder radius="md" mb="md" p="sm">
              <Group justify="space-between" mb="xs">
                <Text size="sm" fw={600}>Steps</Text>
                <Group gap={6}>
                  {Object.entries(counts).map(([st, n]) => (
                    <Badge key={st} size="xs" variant="light" color={STEP_COLOR[st]}>{n} {st}</Badge>
                  ))}
                  {graph.planned && graph.dag && <Badge size="xs" variant="outline" color="gray">DAG · max {graph.max_parallel}</Badge>}
                </Group>
              </Group>
              <StepGraph graph={graph} now={now} onSelect={jumpTo} />
              {!graph.planned && <Text size="xs" c="dimmed" mt={4}>No recorded plan for this run (older Petal) — steps shown in start order, without edges.</Text>}
            </Card>
          )}

          {steps.filter(s => s.started_at).map(s => (
            <div key={s.name} ref={el => { logRefs.current[s.name] = el }} style={{ marginBottom: 12, scrollMarginTop: 8 }}>
              <Group justify="space-between" mb={4}>
                <Group gap={6}>
                  <Badge size="xs" color={STEP_COLOR[s.status]} variant="dot">{s.status}</Badge>
                  <Text size="sm" fw={600}>{s.name}</Text>
                  <Text size="xs" c="dimmed">{stepDuration(s, now)}{s.exit != null && s.status !== 'running' ? ` · exit ${s.exit}` : ''}</Text>
                </Group>
                <Text size="xs" c="dimmed" ff="monospace">{s.image}</Text>
              </Group>
              {s.error && <Text size="xs" c="red" mb={4}>{s.error}</Text>}
              <ScrollArea.Autosize mah={300}>
                <Code block ta="left" style={{ whiteSpace: 'pre-wrap' }}>
                  {logs[s.name] ?? 'loading…'}
                </Code>
              </ScrollArea.Autosize>
            </div>
          ))}

          <Text size="sm" fw={600} mb="xs" mt="md">Timeline</Text>
          <Timeline active={detail.events.length} bulletSize={12} lineWidth={2} mb="md">
            {detail.events.map(ev => (
              <Timeline.Item key={ev.id} title={eventLabel(ev)}>
                <Text size="xs" c="dimmed">{fmtTime(ev.ts)}</Text>
              </Timeline.Item>
            ))}
          </Timeline>
        </>
      )}
    </Drawer>
  )
}

// PipelineHealth: one card per pipeline (per repo) from the recent runs —
// last status, the last 10 outcomes, success rate. Click to filter the table.
function PipelineHealth({ runs, selected, onSelect }) {
  const groups = useMemo(() => {
    const m = new Map()
    for (const r of runs) { // newest first
      const key = `${r.repo || 'self'}/${r.pipeline}`
      if (!m.has(key)) m.set(key, { key, repo: r.repo || 'self', pipeline: r.pipeline, runs: [] })
      m.get(key).runs.push(r)
    }
    return [...m.values()]
  }, [runs])
  if (!groups.length) return null

  return (
    <SimpleGrid cols={{ base: 1, sm: 2, md: 4 }} mb="md">
      {groups.map(g => {
        const last = g.runs[0]
        const done = g.runs.filter(r => !ACTIVE.has(r.status))
        const ok = done.filter(r => r.status === 'succeeded').length
        const isSel = selected === g.key
        return (
          <Card key={g.key} withBorder radius="md" p="sm" onClick={() => onSelect(isSel ? null : g.key)}
            style={{ cursor: 'pointer', outline: isSel ? '2px solid var(--mantine-color-violet-5)' : undefined }}>
            <Group justify="space-between" mb={6} wrap="nowrap">
              <div style={{ minWidth: 0 }}>
                <Text size="sm" fw={600} truncate>{g.pipeline}</Text>
                <Text size="xs" c="dimmed" ff="monospace">{g.repo}</Text>
              </div>
              <Badge color={STATUS_COLOR[last.status] || 'gray'} variant="dot">{last.status}</Badge>
            </Group>
            <Group gap={3} mb={6}>
              {g.runs.slice(0, 10).reverse().map(r => (
                <Tooltip key={r.id} label={`${r.id} · ${r.status}`}>
                  <span style={{
                    width: 10, height: 10, borderRadius: 2, display: 'inline-block',
                    background: `var(--mantine-color-${STATUS_COLOR[r.status] || 'gray'}-6)`,
                  }} />
                </Tooltip>
              ))}
            </Group>
            <Text size="xs" c="dimmed">
              {done.length ? `${Math.round((100 * ok) / done.length)}% ok of last ${done.length}` : 'no finished runs'} · last {fmtDuration(last)}
            </Text>
          </Card>
        )
      })}
    </SimpleGrid>
  )
}

export default function App() {
  const [runs, setRuns] = useState(null)
  const [pipelines, setPipelines] = useState([])
  const [error, setError] = useState('')
  const [selectedRun, setSelectedRun] = useState(null)
  const [filter, setFilter] = useState(null) // "<repo|self>/<pipeline>"
  const [sort, setSort] = useState({ key: 'queued', dir: 'desc' })
  const [limit, setLimit] = useState('50')
  const [drawerOpened, { open: openDrawer, close: closeDrawer }] = useDisclosure(false)

  const refresh = useCallback(async () => {
    try {
      const res = await fetch(`/api/runs?limit=${limit}`)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      setRuns(await res.json())
      setError('')
    } catch (e) {
      setError(e.message)
    }
  }, [limit])

  const shown = useMemo(
    () => sortRuns((runs || []).filter(r => !filter || `${r.repo || 'self'}/${r.pipeline}` === filter), sort),
    [runs, filter, sort],
  )

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
        {runs?.length > 0 && <PipelineHealth runs={runs} selected={filter} onSelect={setFilter} />}
        <Card withBorder padding={0} radius="md">
          {runs === null ? (
            <Group justify="center" p="xl"><Loader /></Group>
          ) : runs.length === 0 ? (
            <Text p="xl" ta="center" c="dimmed">No runs yet — trigger one from Backstage to see the pipeline lifecycle.</Text>
          ) : (
            <Table highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <SortTh col="id" sort={sort} onSort={setSort}>Run</SortTh>
                  <SortTh col="pipeline" sort={sort} onSort={setSort}>Pipeline</SortTh>
                  <SortTh col="trigger" sort={sort} onSort={setSort}>Trigger</SortTh>
                  <SortTh col="status" sort={sort} onSort={setSort}>Status</SortTh>
                  <SortTh col="queued" sort={sort} onSort={setSort}>Queued</SortTh>
                  <SortTh col="finished" sort={sort} onSort={setSort}>Finished</SortTh>
                  <SortTh col="duration" sort={sort} onSort={setSort}>Duration</SortTh>
                  <Table.Th>Error</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {shown.map(run => (
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
                    <Table.Td><TimeCell ts={run.queued_at} /></Table.Td>
                    <Table.Td><TimeCell ts={run.finished_at} /></Table.Td>
                    <Table.Td style={{ whiteSpace: 'nowrap' }}>{fmtDuration(run)}</Table.Td>
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
        <Group justify="space-between" mt="sm">
          <Text size="xs" c="dimmed">
            {runs ? `${shown.length} of ${runs.length} loaded runs` : ''}
            {filter ? ` · filtered to ${filter}` : ''}
            {pipelines.length > 0
              ? ` · self pipelines: ${pipelines.map(p => p.name).join(', ')}`
              : ''} · auto-refresh 5s · old finished runs are pruned (the newest per pipeline are always kept)
          </Text>
          <Group gap={6}>
            <Text size="xs" c="dimmed">load</Text>
            <SegmentedControl size="xs" value={limit} onChange={setLimit} data={['50', '200', '500']} />
          </Group>
        </Group>
      </AppShell.Main>

      <RunDrawer runId={selectedRun} opened={drawerOpened} onClose={closeDrawer} />
    </AppShell>
  )
}
