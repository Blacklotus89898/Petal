package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

func TestDaggerUnknownPipelineFailsFast(t *testing.T) {
	d := NewDagger(".")
	run := runs.Run{ID: "run_test", Pipeline: "does-not-exist", ArgsJSON: "{}"}
	res := d.Execute(context.Background(), run)
	if res.Status != runs.StatusFailed {
		t.Fatalf("expected failed, got %s", res.Status)
	}
	if !strings.Contains(res.Error, "unknown pipeline") || !strings.Contains(res.Error, "docker-image") {
		t.Fatalf("error should name the unknown pipeline and registered ones, got: %s", res.Error)
	}
}
