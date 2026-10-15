package runs

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEnqueueClaimFinish(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	run, err := s.Enqueue(ctx, "docker-image", "build", `{"ref":"petal"}`, "manual", "test", "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if run.Status != StatusQueued {
		t.Fatalf("expected queued, got %s", run.Status)
	}

	claimed, err := s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != run.ID {
		t.Fatalf("expected to claim %s", run.ID)
	}
	if claimed.Status != StatusRunning || claimed.StartedAt == nil {
		t.Fatal("claim should mark run running with started_at")
	}

	if again, _ := s.ClaimNext(ctx); again != nil {
		t.Fatal("queue should be empty after claim")
	}

	if err := s.Finish(ctx, run.ID, StatusSucceeded, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ := s.Get(ctx, run.ID)
	if got.Status != StatusSucceeded || got.FinishedAt == nil {
		t.Fatalf("expected succeeded with finished_at, got %+v", got)
	}
}

func TestEnqueueDedup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first, err := s.Enqueue(ctx, "docker-image", "build", "{}", "webhook", "gha:123", "sha:abc:pipeline")
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	second, err := s.Enqueue(ctx, "docker-image", "build", "{}", "webhook", "gha:123", "sha:abc:pipeline")
	if err != nil {
		t.Fatalf("enqueue dup: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("dedup key should return existing run: %s vs %s", first.ID, second.ID)
	}

	all, _ := s.List(ctx, 10)
	if len(all) != 1 {
		t.Fatalf("expected 1 run, got %d", len(all))
	}
}

func TestListEvents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	run, _ := s.Enqueue(ctx, "p", "f", "{}", "manual", "", "")
	events, err := s.ListEvents(ctx, run.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 || events[0].Kind != "queued" {
		t.Fatalf("expected a queued event, got %+v", events)
	}
}

func TestListFiltered(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mustEnqueue := func(pipeline, kind, source string) {
		t.Helper()
		if _, err := s.Enqueue(ctx, pipeline, "", "{}", kind, source, ""); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	mustEnqueue("docker-image", "backstage", "backstage:component:default/petal#user:default/jo")
	mustEnqueue("docker-image", "backstage", "backstage:component:default/petal-docs#user:default/jo")
	mustEnqueue("docker-image", "webhook", "gha:https://x/1")
	mustEnqueue("lint", "api", "")

	cases := []struct {
		name string
		f    Filter
		want int
	}{
		{"all", Filter{}, 4},
		{"pipeline", Filter{Pipeline: "docker-image"}, 3},
		{"trigger kind", Filter{TriggerKind: "backstage"}, 2},
		// the "#" separator keeps "petal" from matching "petal-docs"
		{"source prefix", Filter{SourcePrefix: "backstage:component:default/petal#"}, 1},
		{"combined", Filter{Pipeline: "lint", TriggerKind: "webhook"}, 0},
		{"limit", Filter{Limit: 2}, 2},
	}
	for _, tc := range cases {
		got, err := s.ListFiltered(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: expected %d runs, got %d", tc.name, tc.want, len(got))
		}
	}
}
