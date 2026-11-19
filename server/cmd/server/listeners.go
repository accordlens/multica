package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/multica-ai/multica/server/internal/chataccess"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// internalOnlyPayloadKeys lists payload keys that exist purely for in-process
// listeners and must never be serialized to a WebSocket client.
//
// `issue:updated` carries prev_description and prev_title so the in-process
// listeners can diff against the new values: subscriber_listeners.go adds newly
// @mentioned users, notification_listeners.go builds mention notifications, and
// activity_listeners.go records the title change. Those all run on
// bus.Subscribe, which Publish dispatches BEFORE the SubscribeAll forwarder
// below, so removing the keys on the way out cannot affect them.
//
// No client reads either key — IssueUpdatedPayload in
// packages/core/types/events.ts does not declare them. They reached the wire
// only because the forwarder reuses the producer's payload map verbatim, which
// meant every description autosave broadcast TWO full copies of the description
// (the new one inside `issue`, plus prev_description) to every connection in the
// workspace, including users who did not have the issue open. The DB write is
// O(1); the fanout was O(workspace connections × description size) (MUL-5492).
//
// This is a table rather than an `if` on one event type because the bug was
// structural, not a typo: the next large field added to a published payload
// inherits the same cost silently. Keeping the list declarative puts the
// internal/external payload boundary in one reviewable place.
var internalOnlyPayloadKeys = map[string][]string{
	protocol.EventIssueUpdated: {"prev_description", "prev_title"},
	// task:failed error text is consumed synchronously by channel outbounds.
	// It may contain provider/runtime detail that belongs in the originating
	// chat transcript, not in the workspace-wide realtime fanout.
	protocol.EventTaskFailed: {"error"},
}

// projectOutbound returns payload with the event type's internal-only keys
// removed, ready to serialize for external consumers.
//
// The input map is never mutated. In-process listeners have already run by the
// time this is called, but the producer still owns the map and a second
// forwarder may yet read it, so mutating it in place would be a landmine.
func projectOutbound(eventType string, payload any) any {
	keys := internalOnlyPayloadKeys[eventType]
	if len(keys) == 0 {
		return payload
	}
	m, ok := payload.(map[string]any)
	if !ok {
		return payload
	}
	projected := make(map[string]any, len(m))
	for k, v := range m {
		projected[k] = v
	}
	for _, k := range keys {
		delete(projected, k)
	}
	return projected
}

// registerListeners wires up event bus listeners for WS broadcasting.
// Personal events (inbox, invites) are sent only to the target user via
// SendToUser. All other events are broadcast to the workspace room.
//
// The broadcaster parameter is intentionally typed as the realtime.Broadcaster
// interface (not *realtime.Hub) so that this layer can later be swapped out
// for a Redis-backed relay or a feature-flagged dual-write implementation
// without touching any of the event listeners below. This is Phase 0 of the
// horizontal-scaling plan tracked in MUL-1138.
func registerListeners(bus *events.Bus, b realtime.Broadcaster, routing ...*db.Queries) {
	// Personal events should NOT be broadcast to the whole workspace.
	personalEvents := map[string]bool{
		"chat:access_revoked":            true,
		protocol.EventInboxNew:           true,
		protocol.EventInboxRead:          true,
		protocol.EventInboxArchived:      true,
		protocol.EventInboxUnarchived:    true,
		protocol.EventInboxBatchRead:     true,
		protocol.EventInboxBatchArchived: true,
		protocol.EventInvitationCreated:  true,
		protocol.EventInvitationRevoked:  true,
		protocol.EventChatSessionCreated: true,
		protocol.EventChatSessionUpdated: true,
		protocol.EventChatSessionDeleted: true,
	}

	// Helper: marshal event and send to a specific user.
	sendToRecipient := func(b realtime.Broadcaster, e events.Event, recipientID string) {
		if recipientID == "" {
			return
		}
		data, err := json.Marshal(map[string]any{"type": e.Type, "payload": projectOutbound(e.Type, e.Payload), "actor_id": e.ActorID, "actor_type": e.ActorType, "recipient_id": recipientID, "conversation_id": e.ChatSessionID, "task_id": e.TaskID, "workspace_id": e.WorkspaceID})
		if err != nil {
			return
		}
		realtime.M.RecordEvent(e.Type)
		b.SendToUser(recipientID, data)
	}
	bus.Subscribe("chat:access_revoked", func(e events.Event) {
		if p, ok := e.Payload.(map[string]any); ok {
			id, _ := p["recipient_id"].(string)
			sendToRecipient(b, e, id)
		}
	})

	// inbox:new — extract recipient from nested item
	bus.Subscribe(protocol.EventInboxNew, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		item, ok := payload["item"].(map[string]any)
		if !ok {
			return
		}
		recipientID, _ := item["recipient_id"].(string)
		sendToRecipient(b, e, recipientID)
	})

	// inbox:read, inbox:archived, inbox:unarchived, inbox:batch-read,
	// inbox:batch-archived — extract recipient from top-level payload
	for _, eventType := range []string{
		protocol.EventInboxRead, protocol.EventInboxArchived, protocol.EventInboxUnarchived,
		protocol.EventInboxBatchRead, protocol.EventInboxBatchArchived,
	} {
		bus.Subscribe(eventType, func(e events.Event) {
			payload, ok := e.Payload.(map[string]any)
			if !ok {
				return
			}
			recipientID, _ := payload["recipient_id"].(string)
			sendToRecipient(b, e, recipientID)
		})
	}

	// invitation:created — send to the invitee so they see the invitation in real time.
	bus.Subscribe(protocol.EventInvitationCreated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		inv, ok := payload["invitation"].(handler.InvitationResponse)
		if !ok {
			// Fallback for map encoding.
			if invMap, ok := payload["invitation"].(map[string]any); ok {
				if uid, _ := invMap["invitee_user_id"].(*string); uid != nil && *uid != "" {
					data, err := json.Marshal(map[string]any{"type": e.Type, "payload": projectOutbound(e.Type, e.Payload), "actor_id": e.ActorID, "actor_type": e.ActorType, "conversation_id": e.ChatSessionID, "task_id": e.TaskID, "workspace_id": e.WorkspaceID})
					if err != nil {
						return
					}
					realtime.M.RecordEvent(e.Type)
					b.SendToUser(*uid, data)
				}
			}
			return
		}
		if inv.InviteeUserID != nil && *inv.InviteeUserID != "" {
			data, err := json.Marshal(map[string]any{"type": e.Type, "payload": projectOutbound(e.Type, e.Payload), "actor_id": e.ActorID, "actor_type": e.ActorType, "conversation_id": e.ChatSessionID, "task_id": e.TaskID, "workspace_id": e.WorkspaceID})
			if err != nil {
				return
			}
			realtime.M.RecordEvent(e.Type)
			b.SendToUser(*inv.InviteeUserID, data)
		}
	})

	// invitation:revoked — send to the invitee so their pending list updates.
	bus.Subscribe(protocol.EventInvitationRevoked, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		uid, _ := payload["invitee_user_id"].(*string)
		if uid != nil && *uid != "" {
			sendToRecipient(b, e, *uid)
		}
	})

	// invitation:accepted / invitation:declined — also send to the invitee so
	// their pending list updates. The actor is the invitee on every producer
	// path, but they are usually NOT in the workspace room yet: a client binds
	// its socket to the workspace it currently has open, so a user concluding
	// an invitation with no workspace open (or a different one open) never
	// receives the broadcast below and their stale pending row survives until
	// restart (#8432). Pass excludeWorkspace so clients already in the room
	// (reached via BroadcastToWorkspace in SubscribeAll) don't get it twice.
	// invitation:revoked keeps its invitee_user_id routing above: its actor is
	// the revoking admin, not the affected invitee.
	for _, eventType := range []string{protocol.EventInvitationAccepted, protocol.EventInvitationDeclined} {
		bus.Subscribe(eventType, func(e events.Event) {
			if e.ActorID == "" {
				return
			}
			data, err := json.Marshal(map[string]any{"type": e.Type, "payload": projectOutbound(e.Type, e.Payload), "actor_id": e.ActorID, "actor_type": e.ActorType, "conversation_id": e.ChatSessionID, "task_id": e.TaskID, "workspace_id": e.WorkspaceID})
			if err != nil {
				return
			}
			realtime.M.RecordEvent(e.Type)
			b.SendToUser(e.ActorID, data, e.WorkspaceID)
		})
	}

	// A Chat session is creator-private. Its initial title may be derived from
	// the creator's first message, so the list-invalidation event must not be
	// broadcast to every workspace member. ActorID is the creator on every
	// producer path for this event.
	for _, eventType := range []string{protocol.EventChatSessionCreated, protocol.EventChatSessionUpdated, protocol.EventChatSessionDeleted} {
		bus.Subscribe(eventType, func(e events.Event) {
			sendToRecipient(b, e, e.ActorID)
		})
	}

	// member:added — also send to the invited user so they discover the new workspace.
	// Pass excludeWorkspace so clients already in the target room (reached via
	// BroadcastToWorkspace in SubscribeAll) don't receive the event twice.
	bus.Subscribe(protocol.EventMemberAdded, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		var userID string
		switch m := payload["member"].(type) {
		case handler.MemberWithUserResponse:
			userID = m.UserID
		case map[string]any:
			userID, _ = m["user_id"].(string)
		default:
			slog.Warn("member:added: unexpected member payload type", "type", fmt.Sprintf("%T", payload["member"]))
		}
		if userID == "" {
			return
		}
		data, err := json.Marshal(map[string]any{"type": e.Type, "payload": projectOutbound(e.Type, e.Payload), "actor_id": e.ActorID, "actor_type": e.ActorType, "conversation_id": e.ChatSessionID, "task_id": e.TaskID, "workspace_id": e.WorkspaceID})
		if err != nil {
			return
		}
		realtime.M.RecordEvent(e.Type)
		b.SendToUser(userID, data, e.WorkspaceID)
	})

	// SubscribeAll handles workspace-broadcast for non-personal events.
	bus.SubscribeAll(func(e events.Event) {
		// Skip personal events — they are handled by type-specific listeners above.
		if personalEvents[e.Type] {
			return
		}

		msg := map[string]any{
			"type":            e.Type,
			"payload":         projectOutbound(e.Type, e.Payload),
			"actor_id":        e.ActorID,
			"actor_type":      e.ActorType,
			"conversation_id": e.ChatSessionID, "task_id": e.TaskID, "workspace_id": e.WorkspaceID,
		}
		data, err := json.Marshal(msg)
		if err != nil {
			slog.Error("failed to marshal event", "event_type", e.Type, "error", err)
			return
		}

		// User targeting preserves delivery to installed clients that do not
		// subscribe to resource scopes. The hub reauthorizes queued/relay frames
		// at delivery, so a recipient list is never a revocable capability.
		chatID := e.ChatSessionID
		if chatID != "" || strings.HasPrefix(e.Type, "chat:") || strings.HasPrefix(e.Type, "task:") {
			if len(routing) == 0 || routing[0] == nil {
				return
			}
			q := routing[0]
			if e.TaskID != "" {
				taskUUID, err := util.ParseUUID(e.TaskID)
				if err != nil {
					return
				}
				task, err := q.GetAgentTask(context.Background(), taskUUID)
				if err != nil {
					return
				}
				conversation, err := chataccess.TaskConversation(context.Background(), q, task)
				if err != nil {
					return
				}
				if conversation.Valid {
					chatID = util.UUIDToString(conversation)
				}
			}
			if chatID != "" {
				cid, err := util.ParseUUID(chatID)
				if err != nil {
					return
				}
				wid, err := util.ParseUUID(e.WorkspaceID)
				if err != nil {
					return
				}
				recipients, err := q.ListChatRecipientIDs(context.Background(), db.ListChatRecipientIDsParams{ChatSessionID: cid, WorkspaceID: wid})
				if err != nil {
					return
				}
				msg["conversation_id"] = chatID
				data, err = json.Marshal(msg)
				if err != nil {
					return
				}
				for _, uid := range recipients {
					if _, err := chataccess.Read(context.Background(), q, uid, wid, cid); err == nil {
						b.SendToUser(util.UUIDToString(uid), data)
					}
				}
				return
			}
			if strings.HasPrefix(e.Type, "chat:") || e.TaskID == "" {
				return
			}
		}

		if e.WorkspaceID != "" {
			realtime.M.RecordEvent(e.Type)
			b.BroadcastToWorkspace(e.WorkspaceID, data)
		} else if strings.HasPrefix(e.Type, "daemon:") {
			realtime.M.RecordEvent(e.Type)
			b.Broadcast(data)
		}
		// Otherwise drop — no global broadcast for non-daemon events without a workspace.
	})
}
