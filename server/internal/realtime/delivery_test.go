package realtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

type deliveryAuthorizerFunc func(context.Context, string, string, []byte) (bool, error)

func (f deliveryAuthorizerFunc) AuthorizeDelivery(ctx context.Context, user, workspace string, frame []byte) (bool, error) {
	return f(ctx, user, workspace, frame)
}

func TestWriteBatchAuthorizesEveryQueuedFrame(t *testing.T) {
	h := NewHub()
	h.SetMembershipChecker(&mockMembershipChecker{})
	conn, peer := membershipSocketPair(t)
	c := &Client{hub: h, conn: conn, userID: testUserID, workspaceID: testWorkspaceID, send: make(chan []byte, 4)}
	h.registerClient(c)
	t.Cleanup(func() { h.removeClient(c); close(h.writeMembership) })
	calls := 0
	h.SetDeliveryAuthorizer(deliveryAuthorizerFunc(func(_ context.Context, _, _ string, frame []byte) (bool, error) {
		calls++
		return !strings.Contains(string(frame), "blocked"), nil
	}))
	c.send <- []byte(`{"type":"test:blocked"}`)
	c.send <- []byte(`{"type":"test:barrier"}`)
	if !c.writeBatch([]byte(`{"type":"test:allowed"}`)) {
		t.Fatal("authorized batch failed")
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	for _, expected := range []string{"test:allowed", "test:barrier"} {
		_, frame, err := peer.ReadMessage()
		if err != nil || !strings.Contains(string(frame), expected) {
			t.Fatalf("queued private frame escaped: want=%s got=%s err=%v", expected, frame, err)
		}
	}
	if calls != 3 {
		t.Fatalf("authorization calls=%d want 3", calls)
	}
}

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
