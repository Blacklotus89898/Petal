package runs

import (
	"context"
	"testing"
)

func TestSyncDeclaredSchedules(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	api, err := s.CreateSchedule(ctx, "docker-image", "", "{}", "0 * * * *", "2030-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	nightly := Declared{ID: "nightly", NextRunAt: "2030-01-01T03:00:00Z",
		NewSchedule: NewSchedule{Pipeline: "validate", Cron: "0 3 * * *", Repo: "homelab", Ref: "main"}}

	if err := s.SyncDeclaredSchedules(ctx, []Declared{nightly}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSchedule(ctx, DeclaredPrefix+"nightly")
	if got == nil || got.Repo != "homelab" || got.Pipeline != "validate" || !got.Enabled || got.NextRunAt != "2030-01-01T03:00:00Z" {
		t.Fatalf("inserted = %+v", got)
	}

	// Same cron + someone disabled it via the API: a restart re-enables it but
	// keeps next_run_at (no re-fire, no skip).
	_ = s.SetScheduleEnabled(ctx, got.ID, false)
	nightly.NextRunAt = "2099-01-01T00:00:00Z"
	if err := s.SyncDeclaredSchedules(ctx, []Declared{nightly}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSchedule(ctx, got.ID)
	if !got.Enabled || got.NextRunAt != "2030-01-01T03:00:00Z" {
		t.Errorf("unchanged cron: enabled=%v next=%s (want true, 2030-01-01T03:00:00Z)", got.Enabled, got.NextRunAt)
	}

	// Cron changed: next_run_at is recomputed.
	nightly.Cron = "30 4 * * *"
	if err := s.SyncDeclaredSchedules(ctx, []Declared{nightly}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSchedule(ctx, got.ID)
	if got.Cron != "30 4 * * *" || got.NextRunAt != "2099-01-01T00:00:00Z" {
		t.Errorf("changed cron: %+v", got)
	}

	// Removed from the file: deleted; the API-created schedule survives.
	if err := s.SyncDeclaredSchedules(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSchedule(ctx, DeclaredPrefix+"nightly"); got != nil {
		t.Errorf("undeclared schedule should be deleted, got %+v", got)
	}
	if got, _ := s.GetSchedule(ctx, api.ID); got == nil {
		t.Error("API-created schedule must not be touched by the declared sync")
	}
}
