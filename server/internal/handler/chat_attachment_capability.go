package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/chataccess"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func signChatAttachment(id string, q url.Values) string {
	mac := hmac.New(sha256.New, attachmentCapabilitySigningKey())
	mac.Write([]byte(strings.Join([]string{"chat-v2", id, q.Get("user"), q.Get("conversation"), q.Get("acl_version"), q.Get("exp"), q.Get("dl")}, "|")))
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *Handler) chatAttachmentCapabilityPath(r *http.Request, att db.Attachment, download bool) string {
	if !att.ChatSessionID.Valid && att.ChatMessageID.Valid {
		m, err := h.Queries.GetChatMessageByID(r.Context(), att.ChatMessageID)
		if err != nil {
			return ""
		}
		att.ChatSessionID = m.ChatSessionID
	}
	user := r.Header.Get("X-User-ID")
	a, err := chataccess.Read(r.Context(), h.Queries, parseUUID(user), att.WorkspaceID, att.ChatSessionID)
	if err != nil {
		return ""
	}
	q := url.Values{"user": {user}, "conversation": {uuidToString(att.ChatSessionID)}, "acl_version": {strconv.FormatInt(a.AclVersion, 10)}, "exp": {strconv.FormatInt(time.Now().Add(60*time.Second).Unix(), 10)}}
	if download {
		q.Set("dl", "1")
	}
	id := uuidToString(att.ID)
	q.Set("chat_sig", signChatAttachment(id, q))
	return "/api/attachments/" + id + "/signed-download?" + q.Encode()
}

func (h *Handler) redeemChatAttachment(w http.ResponseWriter, r *http.Request, att db.Attachment) bool {
	if !att.ChatSessionID.Valid && att.ChatMessageID.Valid {
		m, err := h.Queries.GetChatMessageByID(r.Context(), att.ChatMessageID)
		if err != nil {
			writeError(w, 404, "attachment not found")
			return false
		}
		att.ChatSessionID = m.ChatSessionID
	}
	q := r.URL.Query()
	expires, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	supplied, decodeErr := hex.DecodeString(q.Get("chat_sig"))
	expected, _ := hex.DecodeString(signChatAttachment(uuidToString(att.ID), q))
	if err != nil || decodeErr != nil || expires <= time.Now().Unix() || expires > time.Now().Add(60*time.Second).Unix() || !hmac.Equal(supplied, expected) || q.Get("user") == "" || q.Get("user") != r.Header.Get("X-User-ID") || q.Get("conversation") != uuidToString(att.ChatSessionID) {
		writeError(w, http.StatusNotFound, "attachment not found")
		return false
	}
	if !h.authorizeChatAttachment(w, r, att) {
		return false
	}
	a, err := chataccess.Read(r.Context(), h.Queries, parseUUID(q.Get("user")), att.WorkspaceID, att.ChatSessionID)
	if err != nil || q.Get("acl_version") != strconv.FormatInt(a.AclVersion, 10) {
		writeError(w, http.StatusNotFound, "attachment not found")
		return false
	}
	return true
}
