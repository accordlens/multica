package chataccess

import (
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"testing"
)

func TestRolesAndPrivacyDirections(t *testing.T) {
	cases := []struct {
		kind, participant, workspace string
		action                       Action
		allow                        bool
	}{
		{"public_channel", "", "member", Send, false},
		{"public_channel", "participant", "member", Send, true},
		{"private_channel", "participant", "member", Invite, true},
		{"private_channel", "participant", "member", Remove, false},
		{"private_channel", "manager", "member", Remove, true},
		{"private_channel", "participant", "admin", Remove, true},
		{"private_channel", "", "admin", Remove, false},
		{"private_channel", "participant", "member", Rename, false},
		{"private_channel", "manager", "member", Rename, true},
		{"group_dm", "participant", "member", Rename, true},
		{"dm", "participant", "owner", Rename, false},
		{"private_channel", "participant", "member", Topic, true},
		{"private_channel", "manager", "admin", MakePublic, false},
		{"private_channel", "participant", "owner", MakePublic, true},
		{"private_channel", "", "owner", MakePublic, false},
		{"public_channel", "manager", "member", MakePrivate, true},
		{"public_channel", "", "admin", MakePrivate, true},
		{"dm", "participant", "owner", Moderate, false},
		{"self_dm", "participant", "owner", Invite, false},
		{"group_dm", "participant", "admin", Moderate, true},
		{"private_channel", "participant", "admin", Moderate, true},
		{"private_channel", "manager", "member", Moderate, false},
		{"agent_dm", "manager", "owner", Send, false},
	}
	for _, c := range cases {
		t.Run(c.kind+"/"+c.participant+"/"+c.workspace+"/"+string(c.action), func(t *testing.T) {
			a := db.GetChatAccessRow{Kind: c.kind, ParticipantRole: c.participant, WorkspaceRole: c.workspace, Status: "active"}
			if got := Check(a, c.action) == nil; got != c.allow {
				t.Fatalf("allowed=%v want %v", got, c.allow)
			}
		})
	}
	if Check(db.GetChatAccessRow{Kind: "public_channel", IsGeneral: true, ParticipantRole: "manager", WorkspaceRole: "owner"}, MakePrivate) == nil {
		t.Fatal("general became private")
	}
	if Check(db.GetChatAccessRow{Kind: "private_channel", ParticipantRole: "participant", Status: "archived"}, Send) == nil {
		t.Fatal("send into archived channel")
	}
}
