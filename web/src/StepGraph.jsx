// StepGraph draws a run's step graph (GET /api/runs/{id} → graph) as SVG:
// longest-path layering left→right, one node per step colored by status.
// Dependency-free on purpose — pipelines have a handful of steps, and the
// binary stays small (same reason /metrics is hand-rolled).
import { useMemo } from 'react'

export const STEP_COLOR = {
  pending: 'gray',
  running: 'blue',
  succeeded: 'green',
  failed: 'red',
  canceled: 'orange',
  skipped: 'gray',
}

const GLYPH = { pending: '○', running: '●', succeeded: '✓', failed: '✗', canceled: '⊘', skipped: '↷' }

const NODE_W = 156
const NODE_H = 48
const GAP_X = 40
const GAP_Y = 14
const PAD = 8

export function stepDuration(step, now = Date.now()) {
  if (!step.started_at) return ''
  const end = step.finished_at ? Date.parse(step.finished_at) : now
  return fmtSeconds(Math.max(0, (end - Date.parse(step.started_at)) / 1000))
}

// fmtSeconds: 12.3s · 4m05s · 9h47m
export function fmtSeconds(s) {
  if (s < 60) return `${s.toFixed(1)}s`
  if (s < 3600) return `${Math.floor(s / 60)}m${String(Math.floor(s % 60)).padStart(2, '0')}s`
  return `${Math.floor(s / 3600)}h${String(Math.floor((s % 3600) / 60)).padStart(2, '0')}m`
}

function layout(graph) {
  const steps = graph?.steps || []
  const byName = Object.fromEntries(steps.map(s => [s.name, s]))
  const depth = {}
  const depthOf = (name, seen = new Set()) => {
    if (depth[name] !== undefined) return depth[name]
    if (seen.has(name)) return 0 // defensive: plans are validated acyclic
    seen.add(name)
    const needs = (byName[name]?.needs || []).filter(n => byName[n])
    depth[name] = needs.length ? 1 + Math.max(...needs.map(n => depthOf(n, seen))) : 0
    return depth[name]
  }
  // No plan (older runs): no edges are known, so show the steps as a row in
  // the order they started rather than inventing dependencies.
  steps.forEach((s, i) => { if (!graph.planned) depth[s.name] = i; else depthOf(s.name) })

  const rows = {}
  const pos = {}
  for (const s of steps) {
    const col = depth[s.name]
    const row = rows[col] = (rows[col] ?? -1) + 1
    pos[s.name] = { x: PAD + col * (NODE_W + GAP_X), y: PAD + row * (NODE_H + GAP_Y) }
  }
  const cols = Math.max(0, ...Object.values(depth)) + 1
  const maxRows = Math.max(1, ...Object.values(rows).map(r => r + 1))
  return {
    steps, pos,
    width: PAD * 2 + cols * NODE_W + (cols - 1) * GAP_X,
    height: PAD * 2 + maxRows * NODE_H + (maxRows - 1) * GAP_Y,
  }
}

const truncate = (s, n) => (s.length > n ? s.slice(0, n - 1) + '…' : s)
const color = (status, shade) => `var(--mantine-color-${STEP_COLOR[status] || 'gray'}-${shade})`

export default function StepGraph({ graph, onSelect, now }) {
  const { steps, pos, width, height } = useMemo(() => layout(graph), [graph])
  if (!steps.length) return null

  return (
    <div style={{ overflowX: 'auto' }}>
      {/* Scales down to the drawer's width; never up past natural size. */}
      <svg viewBox={`0 0 ${width} ${height}`} role="img" aria-label="pipeline step graph"
        style={{ display: 'block', width: '100%', maxWidth: width, minWidth: Math.min(width, 520), height: 'auto' }}>
        <style>{`
          @keyframes petal-pulse { 0%,100% { opacity: 1 } 50% { opacity: .45 } }
          .petal-running { animation: petal-pulse 1.4s ease-in-out infinite }
          .petal-node { cursor: pointer }
          .petal-node:hover rect { stroke-width: 2.5 }
        `}</style>
        {graph.planned && steps.flatMap(s => (s.needs || []).filter(n => pos[n]).map(n => {
          const a = pos[n], b = pos[s.name]
          const x1 = a.x + NODE_W, y1 = a.y + NODE_H / 2, x2 = b.x, y2 = b.y + NODE_H / 2
          const mx = (x1 + x2) / 2
          const src = steps.find(t => t.name === n)
          const done = src?.status === 'succeeded'
          return (
            <path key={`${n}->${s.name}`} d={`M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}`}
              fill="none" strokeWidth={1.5}
              stroke={done ? color('succeeded', 5) : 'var(--mantine-color-gray-5)'}
              strokeDasharray={done ? undefined : '4 3'} />
          )
        }))}
        {steps.map(s => {
          const { x, y } = pos[s.name]
          const dur = stepDuration(s, now)
          const sub = [s.status, dur, s.uses ? `uses ${s.uses}` : ''].filter(Boolean).join(' · ')
          return (
            <g key={s.name} className="petal-node" transform={`translate(${x},${y})`}
              onClick={() => onSelect?.(s.name)}>
              <title>{[s.name, s.image, s.error].filter(Boolean).join('\n')}</title>
              <rect width={NODE_W} height={NODE_H} rx={8}
                fill={color(s.status, 'light')} stroke={color(s.status, 6)} strokeWidth={1.5}
                strokeDasharray={s.status === 'skipped' ? '4 3' : undefined}
                className={s.status === 'running' ? 'petal-running' : undefined} />
              {/* Theme-aware: Mantine's -light / -light-color pair adapts to both schemes. */}
              <text x={10} y={20} fontSize={13} fontWeight={600} fill={color(s.status, 'light-color')}>
                {GLYPH[s.status] || '○'} {truncate(s.name, 17)}
              </text>
              <text x={10} y={37} fontSize={11} fill="var(--mantine-color-dimmed)">{truncate(sub, 25)}</text>
            </g>
          )
        })}
      </svg>
    </div>
  )
}
