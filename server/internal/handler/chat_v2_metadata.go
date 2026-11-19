package handler

import (
	"crypto/hmac"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/multica-ai/multica/server/internal/chataccess"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Metadata changes and conversion previews use the same lock and revision as
// membership/content. A confirmation is bound to user, direction and revision.
func (h *Handler) ChangeChatMetadataV2(w http.ResponseWriter, r *http.Request) {
	s, ok := h.loadChatConversationV2(w, r)
	if !ok {
		return
	}
	if r.Header.Get("X-Actor-Source") == "task_token" {
		writeError(w, 403, "human metadata action required")
		return
	}
	var req struct {
		ExpectedRevision int64   `json:"expected_revision"`
		Name             *string `json:"name"`
		Topic            *string `json:"topic"`
		Description      *string `json:"description"`
		Kind             *string `json:"kind"`
		Preview          bool    `json:"preview"`
		Confirmation     string  `json:"confirmation"`
		Expires          int64   `json:"expires"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil || req.ExpectedRevision < 1 {
		writeError(w, 400, "expected_revision required")
		return
	}
	kind := s.Kind
	if req.Kind != nil {
		kind = *req.Kind
	}
	if kind != s.Kind && !(s.Kind == "private_channel" && kind == "public_channel" || s.Kind == "public_channel" && kind == "private_channel") {
		writeError(w, 400, "unsupported conversion")
		return
	}
	name, topic, description := s.Name, s.Topic, s.Description
	if req.Name != nil {
		name = *req.Name
	}
	if req.Topic != nil {
		topic = *req.Topic
	}
	if req.Description != nil {
		description = *req.Description
	}
	var expires int64
	var confirmation string
	err := chataccess.Mutate(r.Context(), h.TxStarter, h.Queries, parseUUID(requestUserID(r)), s.WorkspaceID, s.ID, req.ExpectedRevision, func(q *db.Queries, a db.GetChatAccessRow) error {
		if req.Name != nil {
			if err := chataccess.Check(a, chataccess.Rename); err != nil {
				return err
			}
		}
		if req.Topic != nil || req.Description != nil {
			if err := chataccess.Check(a, chataccess.Topic); err != nil {
				return err
			}
		}
		if kind != s.Kind {
			action := chataccess.MakePrivate
			if kind == "public_channel" {
				action = chataccess.MakePublic
			}
			if err := chataccess.Check(a, action); err != nil {
				return err
			}
			expires = req.Expires
			if req.Preview {
				expires = time.Now().Add(time.Minute).Unix()
			}
			values := url.Values{"user": {requestUserID(r)}, "conversation": {kind}, "acl_version": {strconv.FormatInt(a.Revision, 10)}, "exp": {strconv.FormatInt(expires, 10)}, "dl": {"privacy-preview"}}
			confirmation = signChatAttachment(uuidToString(s.ID), values)
			if !req.Preview && (expires <= time.Now().Unix() || expires > time.Now().Add(time.Minute).Unix() || !hmac.Equal([]byte(confirmation), []byte(req.Confirmation))) {
				return chataccess.ErrForbidden
			}
		}
		if req.Preview {
			return nil
		}
		_, err := q.UpdateChatConversationMetadata(r.Context(), db.UpdateChatConversationMetadataParams{ID: s.ID, Name: name, Topic: topic, Description: description, Kind: kind})
		return err
	})
	if err != nil {
		chatAccessError(w, err)
		return
	}
	if req.Preview {
		writeJSON(w, 200, map[string]any{"revision": s.Revision, "from_kind": s.Kind, "to_kind": kind, "confirmation": confirmation, "expires": expires, "workspace_readable": kind == "public_channel"})
		return
	}
	if kind != s.Kind {
		// Private conversion also invalidates tickets/previews held by nonparticipants.
		members, err := h.Queries.ListMembers(r.Context(), s.WorkspaceID)
		if err == nil {
			for _, m := range members {
				if _, err := chataccess.Read(r.Context(), h.Queries, m.UserID, s.WorkspaceID, s.ID); err != nil {
					h.publishChatAccessRevoked(uuidToString(m.UserID), uuidToString(s.WorkspaceID), uuidToString(s.ID))
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"revision": s.Revision + 1, "acl_version": s.AclVersion + boolIncrement(kind != s.Kind)})
}
func boolIncrement(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
