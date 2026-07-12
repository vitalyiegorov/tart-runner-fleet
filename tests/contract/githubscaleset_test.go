package contract_test

import (
	"context"
	"testing"
	"time"

	"github.com/actions/scaleset"
	adapter "github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
)

// protocolFake is intentionally typed against actions/scaleset v0.4.0. This
// contract fails to compile if preview API churn reaches beyond the adapter.
type protocolFake struct {
	last    []int
	deleted []int
}

func (f *protocolFake) GetMessage(_ context.Context, last, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	f.last = append(f.last, last)
	return &scaleset.RunnerScaleSetMessage{MessageID: 42, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2}}, nil
}
func (f *protocolFake) DeleteMessage(_ context.Context, id int) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *protocolFake) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	return append([]int64(nil), ids...), nil
}
func (f *protocolFake) GenerateJitRunnerConfig(_ context.Context, _ *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "secret"}, nil
}

func TestOfficialScaleSetV040Boundary(t *testing.T) {
	f := &protocolFake{}
	s, err := adapter.NewScaleSet(adapter.ScaleSetConfig{Messages: f, JIT: f, ScaleSetID: 1, MaxCapacity: 2, PollTimeout: time.Second, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Demand.MessageID != 42 || m.Demand.Assigned != 2 {
		t.Fatalf("bad official mapping: %+v", m.Demand)
	}
	if err := m.Ack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.last) != 1 || f.last[0] != 0 || len(f.deleted) != 1 || f.deleted[0] != 42 || s.LastMessageID() != 42 {
		t.Fatalf("cursor/ack contract failed: %+v", f)
	}
	jit, err := s.GenerateJIT(context.Background(), "runner", "_work")
	if err != nil {
		t.Fatal(err)
	}
	if jit.Reveal() != "secret" {
		t.Fatal("JIT mapping failed")
	}
}
