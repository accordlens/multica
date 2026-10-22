package handler

import "net/http"

// Team creation/send is advertised only by the later integrated feature slices.
// This expansion adds their shared storage/read/ACL contract without exposing an
// unfinished communicator to installed clients.
func (h *Handler) GetChatCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"protocol": 2, "team_enabled": false,
		"supported_kinds": []string{"agent_dm"}, "schema_kinds": []string{"agent_dm", "public_channel", "private_channel", "dm", "self_dm", "group_dm"},
		"limits":                             map[string]any{"message_codepoints": 40000, "history_default": 50, "history_max": 100, "group_members": 9, "attachments": 10, "upload_bytes": maxUploadSize},
		"private_download_identity_required": true, "edit_policy": "own_unlimited", "delete_policy": "own_or_accessible_channel_moderator"})
}
