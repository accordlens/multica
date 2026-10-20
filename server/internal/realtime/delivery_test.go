package realtime

import (
	"context"
	"testing"
	"time"
)

type blockedDeliveryAuthorizer struct{ entered, release chan struct{} }

func (a blockedDeliveryAuthorizer) AuthorizeDelivery(context.Context, string, string, []byte) (bool, error) {
	close(a.entered)
	<-a.release
	return true, nil
}

func TestDeliveryAuthorizationDoesNotBlockClientRemoval(t *testing.T) {
	for _, target := range []string{"workspace", "user", "all"} {
		t.Run(target, func(t *testing.T) {
			h := NewHub()
			scope, id := ScopeWorkspace, "workspace-1"
			if target == "user" {
				scope, id = ScopeUser, "user-1"
			}
			c := attachRealtimeTestClient(h, scope, id)
			M.ActiveConnections.Add(1)
			gate := blockedDeliveryAuthorizer{make(chan struct{}), make(chan struct{})}
			h.SetDeliveryAuthorizer(gate)
			done := make(chan struct{})
			go func() {
				defer close(done)
				switch target {
				case "workspace":
					h.BroadcastToWorkspace(id, []byte(`{"type":"task:message"}`))
				case "user":
					h.SendToUser(id, []byte(`{"type":"task:message"}`))
				default:
					h.fanoutAll([]byte(`{"type":"task:message"}`), "")
				}
			}()
			select {
			case <-gate.entered:
			case <-time.After(time.Second):
				close(gate.release)
				t.Fatal("delivery gate was not reached")
			}
			removed := make(chan struct{})
			go func() { h.removeClient(c); close(removed) }()
			blocked := false
			select {
			case <-removed:
			case <-time.After(time.Second):
				blocked = true
			}
			close(gate.release)
			<-done
			<-removed
			if blocked {
				t.Fatal("slow ACL lookup held the hub lock and blocked client removal")
			}
			if _, ok := <-c.send; ok {
				t.Fatal("disconnected client received a queued frame")
			}
		})
	}
}
