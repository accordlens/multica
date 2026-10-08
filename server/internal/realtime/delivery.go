package realtime

import (
	"context"
	"encoding/json"
)

type deliveryGate struct{ authorizer DeliveryAuthorizer }

type DeliveryAuthorizer interface {
	AuthorizeDelivery(context.Context, string, string, []byte) (bool, error)
}

func (h *Hub) SetDeliveryAuthorizer(a DeliveryAuthorizer) {
	h.deliveryAuthorizer.Store(&deliveryGate{authorizer: a})
}

// Recheck both on fanout and immediately before writing queued data. All relay
// transports ultimately use these paths. Positive results are never cached.
func (h *Hub) canDeliver(c *Client, frame []byte) bool {
	gate := h.deliveryAuthorizer.Load()
	if gate == nil {
		return true
	} // hub-only tests; production installs the DB gate
	ok, err := gate.authorizer.AuthorizeDelivery(context.Background(), c.userID, c.workspaceID, frame)
	return err == nil && ok
}

// Called from the writer, outside hub locks. Remove revoked resource rooms so
// relay subscriptions cannot retain access until the next reconnect.
func (h *Hub) pruneRevokedScopes(c *Client, frame []byte) {
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(frame, &event) != nil || (event.Type != "chat:access_revoked" && event.Type != "member:removed") {
		return
	}
	h.mu.RLock()
	keys := make([]scopeKey, 0, len(c.subscriptions))
	for key := range c.subscriptions {
		keys = append(keys, key)
	}
	h.mu.RUnlock()
	for _, key := range keys {
		var candidate []byte
		switch key.Type {
		case ScopeChat:
			candidate, _ = json.Marshal(map[string]string{"type": "chat:acl_probe", "conversation_id": key.ID, "workspace_id": c.workspaceID})
		case ScopeTask:
			candidate, _ = json.Marshal(map[string]string{"type": "task:acl_probe", "task_id": key.ID, "workspace_id": c.workspaceID})
		default:
			continue
		}
		if !h.canDeliver(c, candidate) {
			h.unsubscribe(c, key.Type, key.ID)
		}
	}
}
