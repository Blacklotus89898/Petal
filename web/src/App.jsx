import { useCallback, useEffect, useState } from 'react'
import {
  Alert,
  AppShell,
  Badge,
  Button,
  Card,
  Group,
  Loader,
  Modal,
  Table,
  Text,
  Textarea,
  TextInput,
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

function TriggerModal({ opened, onClose, onTriggered }) {
  const [pipeline, setPipeline] = useState('docker-image')
  const [moduleFn, setModuleFn] = useState('build')
  const [args, setArgs] = useState('{}')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async () => {
    setBusy(true)
    setError('')
    try {
      const parsed = JSON.parse(args || '{}')
      const res = await fetch('/api/runs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ pipeline, module_fn: moduleFn, args: parsed }),
      })
      if (!res.ok) throw new Error((await res.json()).error || `HTTP ${res.status}`)
      onTriggered()
      onClose()
    } catch (e) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal opened={opened} onClose={onClose} title="Trigger run" centered>
      {error && <Alert color="red" mb="sm">{error}</Alert>}
      <TextInput label="Pipeline" value={pipeline} onChange={e => setPipeline(e.currentTarget.value)} mb="sm" />
      <TextInput label="Module function" value={moduleFn} onChange={e => setModuleFn(e.currentTarget.value)} mb="sm" />
      <Textarea label="Arguments (JSON)" value={args} onChange={e => setArgs(e.currentTarget.value)} rows={4} mb="sm" />
      <Group justify="flex-end">
        <Button variant="default" onClick={onClose}>Cancel</Button>
        <Button onClick={submit} loading={busy}>Run</Button>
      </Group>
    </Modal>
  )
}

export default function App() {
  const [runs, setRuns] = useState(null)
  const [error, setError] = useState('')
  const [modalOpened, { open: openModal, close: closeModal }] = useDisclosure(false)
  const [interval, setInterval] = useState(5000)

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

  useEffect(() => { refresh() }, [refresh])
  useInterval(refresh, interval)

  return (
    <AppShell header={{ height: 56 }} padding="md">
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between">
          <Group gap="xs">
            <Title order={4}>🌸 Petal</Title>
            <Text size="xs" c="dimmed">homelab control plane</Text>
          </Group>
          <Button size="xs" onClick={openModal}>Trigger run</Button>
        </Group>
      </AppShell.Header>

      <AppShell.Main>
        {error && <Alert color="red" mb="md">Cannot reach Petal API: {error}</Alert>}
        <Card withBorder padding={0} radius="md">
          {runs === null ? (
            <Group justify="center" p="xl"><Loader /></Group>
          ) : runs.length === 0 ? (
            <Text p="xl" ta="center" c="dimmed">No runs yet — trigger one to see the pipeline lifecycle.</Text>
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
                  <Table.Tr key={run.id}>
                    <Table.Td style={{ fontFamily: 'monospace' }}>{run.id}</Table.Td>
                    <Table.Td>{run.pipeline}{run.module_fn ? `:${run.module_fn}` : ''}</Table.Td>
                    <Table.Td><Badge variant="light" color="violet">{run.trigger_kind}</Badge></Table.Td>
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
          Executor: stub (Phase A) · auto-refresh 5s
        </Text>
      </AppShell.Main>

      <TriggerModal
        opened={modalOpened}
        onClose={closeModal}
        onTriggered={() => { refresh(); setInterval(1000) }}
      />
    </AppShell>
  )
}
