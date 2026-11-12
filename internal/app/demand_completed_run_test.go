package app_test

import (
 "context"
 "path/filepath"
 "testing"
 "time"

 "github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
 "github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
 "github.com/vitalyiegorov/tart-runner-fleet/internal/app"
 "github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
 "github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func TestCompletedRunGhostLeavesQueue(t *testing.T) {
 ctx:=context.Background()
 store,err:=sqlite.Open(ctx,filepath.Join(t.TempDir(),"fleet.db"))
 if err!=nil {t.Fatal(err)}
 defer func(){_ = store.Close()}()
 now:=time.Now()
 event:=operations.DemandEvent{Kind:operations.DemandJobAvailable,RunnerRequestID:1,Owner:"o",Repository:"r",WorkflowRunID:77,JobID:"uuid",Labels:[]string{"linux"},QueueTime:now.Add(-26*time.Hour)}
 if _,err=store.ApplyDemandBatch(ctx,1,1,[]operations.DemandEvent{event});err!=nil{t.Fatal(err)}
 binding:=app.Binding{ScaleSetID:1,Profile:domain.Profile{ID:"linux",Route:"linux",Platform:domain.PlatformLinux}}
 c:=app.DemandCoordinator{Store:store,Now:func()time.Time{return now}}
 n,err:=c.RetireCompletedRuns(ctx,[]app.Binding{binding},func(_ context.Context,runs []githubscaleset.WorkflowRun)([]githubscaleset.WorkflowRun,error){
  if len(runs)!=1 {t.Fatalf("candidates=%v",runs)}
  runs[0].Status="completed"
  return runs,nil
 })
 if err!=nil||n!=1{t.Fatalf("retired=%d err=%v",n,err)}
 queued,err:=c.QueuedDemands(ctx,binding)
 if err!=nil||len(queued)!=0{t.Fatalf("ghost still heads queue: %v %v",queued,err)}
 active,err:=store.ActiveDemands(ctx,1)
 if err!=nil||len(active)!=0{t.Fatalf("ghost still blocks quiescence: %v %v",active,err)}
}
