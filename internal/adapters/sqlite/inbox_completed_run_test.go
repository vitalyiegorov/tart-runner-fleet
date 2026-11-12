package sqlite

import (
 "context"
 "testing"
 "time"

 "github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
 "github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func TestCompletedRunRetiresUncorroboratedAndOrphanStartedDemand(t *testing.T) {
 for _, kind := range []operations.DemandEventKind{operations.DemandJobAvailable, operations.DemandJobAssigned, operations.DemandJobStarted} {
  t.Run(string(kind), func(t *testing.T) {
   store := overdueStore(t)
   event := demandEvent(kind, 1)
   if _, err := store.ApplyDemandBatch(context.Background(), overdueScaleSet, 1, []operations.DemandEvent{event}); err != nil { t.Fatal(err) }
   now := time.Now()
   run := githubscaleset.WorkflowRun{ID:77, Repository:githubscaleset.Repository{Owner:"owner",Name:"repo"}, Status:"completed"}
   n, err := store.ExpireCompletedRunDemands(context.Background(), overdueScaleSet, now, run)
   if err != nil || n != 1 { t.Fatalf("retired=%d err=%v", n,err) }
   active, err := store.ActiveDemands(context.Background(),overdueScaleSet)
   if err != nil || len(active)!=0 { t.Fatalf("active=%v err=%v",active,err) }
  })
 }
}
