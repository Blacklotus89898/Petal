package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Blacklotus89898/Petal/internal/pipeline"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

// fakeRunner stands in for Docker (ADR 0005 tests): each step sleeps for its
// configured duration (respecting ctx, like a killed container) and exits with
// its configured code. It records start/end order and peak concurrency.
type fakeRunner struct {
	mu       sync.Mutex
	dur      map[string]time.Duration
	exit     map[string]int
	started  []string
	finished []string
	log      []string // "start:x" / "end:x", in real order
	inflight atomic.Int32
	peak     atomic.Int32
}

func (f *fakeRunner) run(ctx context.Context, _ string, st pipeline.Step, _, _ string, _ map[string]string) (int, error) {
	f.mu.Lock()
	f.started = append(f.started, st.Name)
	f.log = append(f.log, "start:"+st.Name)
	f.mu.Unlock()
	n := f.inflight.Add(1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	defer func() {
		f.inflight.Add(-1)
		f.mu.Lock()
		f.finished = append(f.finished, st.Name)
		f.log = append(f.log, "end:"+st.Name)
		f.mu.Unlock()
	}()
	select {
	case <-time.After(f.dur[st.Name]):
		return f.exit[st.Name], nil
	case <-ctx.Done():
		return -1, nil // like runStep: container killed, caller reads ctx
	}
}

type dagHarness struct {
	store *runs.Store
	steps *Steps
	fake  *fakeRunner
}

func newDAGHarness(t *testing.T, pipelineYAML string, fake *fakeRunner) *dagHarness {
	t.Helper()
	store, err := runs.Open(context.Background(), filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	workdir := t.TempDir()
	dir := filepath.Join(workdir, ".petal", "pipelines")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(pipelineYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSteps(store, workdir, t.TempDir(), "tcp://127.0.0.1:1", nil, nil)
	s.runner = fake.run
	return &dagHarness{store: store, steps: s, fake: fake}
}

func (h *dagHarness) execute(t *testing.T, ctx context.Context) (runs.Run, Result) {
	t.Helper()
	run, err := h.store.Enqueue(context.Background(), "p", "", "{}", "api", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return *run, h.steps.Execute(ctx, *run)
}

func (h *dagHarness) events(t *testing.T, runID string) string {
	t.Helper()
	evs, err := h.store.ListEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Kind + " " + e.Payload + "\n")
	}
	return b.String()
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

const stepLine = "    image: alpine:3.21\n    run: 'true'\n"

func TestFileOrderIsSequential(t *testing.T) {
	fake := &fakeRunner{dur: map[string]time.Duration{"a": 20 * time.Millisecond, "b": 5 * time.Millisecond, "c": 5 * time.Millisecond}}
	h := newDAGHarness(t, "steps:\n  - name: a\n"+stepLine+"  - name: b\n"+stepLine+"  - name: c\n"+stepLine, fake)

	_, res := h.execute(t, context.Background())
	if res.Status != runs.StatusSucceeded {
		t.Fatalf("result = %+v", res)
	}
	if strings.Join(fake.started, ",") != "a,b,c" || fake.peak.Load() != 1 {
		t.Errorf("legacy mode must run in file order one at a time: started=%v peak=%d", fake.started, fake.peak.Load())
	}
}

func TestDAGRunsRootsConcurrentlyAndHonoursNeeds(t *testing.T) {
	fake := &fakeRunner{dur: map[string]time.Duration{
		"lint": 60 * time.Millisecond, "test": 60 * time.Millisecond, "deploy": 5 * time.Millisecond,
	}}
	// deploy is listed FIRST — only needs decide the order.
	h := newDAGHarness(t, `steps:
  - name: deploy
`+stepLine+`    needs: [lint, test]
  - name: lint
`+stepLine+`    needs: []
  - name: test
`+stepLine+`    needs: []
`, fake)

	_, res := h.execute(t, context.Background())
	if res.Status != runs.StatusSucceeded {
		t.Fatalf("result = %+v", res)
	}
	if fake.peak.Load() != 2 {
		t.Errorf("lint and test should overlap (peak 2), got peak %d", fake.peak.Load())
	}
	d := indexOf(fake.log, "start:deploy")
	if d < indexOf(fake.log, "end:lint") || d < indexOf(fake.log, "end:test") {
		t.Errorf("deploy must start only after lint and test ended: %v", fake.log)
	}
}

func TestDAGConcurrencyCap(t *testing.T) {
	fake := &fakeRunner{dur: map[string]time.Duration{"a": 30 * time.Millisecond, "b": 30 * time.Millisecond, "c": 30 * time.Millisecond, "d": 30 * time.Millisecond}}
	y := "max_parallel: 2\nsteps:\n"
	for _, n := range []string{"a", "b", "c", "d"} {
		y += "  - name: " + n + "\n" + stepLine + "    needs: []\n"
	}
	h := newDAGHarness(t, y, fake)
	if _, res := h.execute(t, context.Background()); res.Status != runs.StatusSucceeded {
		t.Fatalf("result = %+v", res)
	}
	if fake.peak.Load() != 2 || len(fake.finished) != 4 {
		t.Errorf("4 independent steps with max_parallel 2: peak=%d finished=%v", fake.peak.Load(), fake.finished)
	}
}

func TestDAGFailFastCancelsSiblingsAndSkipsDependents(t *testing.T) {
	fake := &fakeRunner{
		dur:  map[string]time.Duration{"bad": 10 * time.Millisecond, "slow": 5 * time.Second, "after": time.Millisecond},
		exit: map[string]int{"bad": 3},
	}
	h := newDAGHarness(t, `steps:
  - name: bad
`+stepLine+`    needs: []
  - name: slow
`+stepLine+`    needs: []
  - name: after
`+stepLine+`    needs: [bad]
`, fake)

	start := time.Now()
	run, res := h.execute(t, context.Background())
	if res.Status != runs.StatusFailed || !strings.Contains(res.Error, `step "bad" failed (exit 3)`) {
		t.Fatalf("result = %+v", res)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the slow sibling should have been canceled, not waited for")
	}
	ev := h.events(t, run.ID)
	if !strings.Contains(ev, `step.finished {"canceled":true,"cause":"another step failed","step":"slow"}`) {
		t.Errorf("slow should be recorded as canceled by the sibling failure:\n%s", ev)
	}
	if !strings.Contains(ev, `step.skipped {"step":"after"}`) {
		t.Errorf("after (needs bad) should be skipped:\n%s", ev)
	}
}

func TestStepTimeoutFailsTheRun(t *testing.T) {
	fake := &fakeRunner{dur: map[string]time.Duration{"hang": 5 * time.Second}}
	h := newDAGHarness(t, "steps:\n  - name: hang\n"+stepLine+"    timeout: 50ms\n", fake)
	run, res := h.execute(t, context.Background())
	if res.Status != runs.StatusFailed || res.Error != `step "hang" timed out after 50ms` {
		t.Fatalf("result = %+v", res)
	}
	if ev := h.events(t, run.ID); !strings.Contains(ev, `"timed_out":true`) {
		t.Errorf("timeout should be recorded on the step:\n%s", ev)
	}
}

func TestRunTimeout(t *testing.T) {
	fake := &fakeRunner{dur: map[string]time.Duration{"a": 5 * time.Second}}
	h := newDAGHarness(t, "timeout: 60ms\nsteps:\n  - name: a\n"+stepLine, fake)
	if _, res := h.execute(t, context.Background()); res.Status != runs.StatusFailed || res.Error != "run timed out after 60ms" {
		t.Fatalf("result = %+v", res)
	}
}

func TestCancelRequestCancelsTheRunAndKeepsTheAuditTrail(t *testing.T) {
	fake := &fakeRunner{dur: map[string]time.Duration{"a": 5 * time.Second, "b": time.Millisecond}}
	h := newDAGHarness(t, "steps:\n  - name: a\n"+stepLine+"  - name: b\n"+stepLine, fake)

	ctx, cancel := context.WithCancelCause(context.Background())
	time.AfterFunc(50*time.Millisecond, func() { cancel(ErrCancelRequested) })
	run, res := h.execute(t, ctx)
	if res.Status != runs.StatusCanceled || res.Error != "canceled on request" {
		t.Fatalf("result = %+v", res)
	}
	ev := h.events(t, run.ID)
	// Written AFTER the context was canceled — proves the WithoutCancel writes.
	if !strings.Contains(ev, `step.finished {"canceled":true,"cause":"cancel requested","step":"a"}`) ||
		!strings.Contains(ev, `step.skipped {"step":"b"}`) {
		t.Errorf("cancellation events lost:\n%s", ev)
	}
}
