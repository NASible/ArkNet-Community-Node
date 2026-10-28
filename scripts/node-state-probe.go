// Integration-test helper only: mounted into the disposable Go build container
// under /src/probe/main.go so it can call the Agent's internal state boundary.
// Never run against a normal development or production database.
package main

import (
 "context"
 "errors"
 "fmt"
 "os"
 "time"

 "arknet/node-agent/internal/localstate"
 "arknet/node-agent/internal/postgres"
)

func main() {
 ctx,cancel:=context.WithTimeout(context.Background(),10*time.Second);defer cancel()
 store,err:=postgres.Open(ctx,os.Getenv("ARKNET_TEST_DATABASE_URL"))
 if err!=nil {panic(err)};defer store.Close()
 before,err:=store.Snapshot(ctx);if err!=nil {panic(err)}
 if os.Getenv("ARKNET_TEST_INVALID_TRANSITION")=="1" {
  _,err=store.Transition(ctx,before.State,localstate.Revoked)
  if !errors.Is(err,localstate.ErrInvalidTransition) {panic("invalid transition was not rejected")}
  after,err:=store.Snapshot(ctx);if err!=nil {panic(err)}
  if before!=after {panic("invalid transition changed persisted state")}
  fmt.Println("PASS: invalid transition leaves state and timestamp unchanged")
  return
 }
 next:=localstate.State(os.Getenv("ARKNET_TEST_NEXT_STATE"))
 result,err:=store.Transition(ctx,before.State,next);if err!=nil {panic(err)}
 if result.State!=next || result.StateChangedAt.IsZero() {panic("transition not persisted")}
 fmt.Println("PASS: persisted explicit transition to",next)
}
