package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
)

func passkeyOnlyServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("MULTICA_PASSKEY_ONLY", "true")
	t.Setenv("MULTICA_APP_URL", "https://multica.test.ts.net")
	hub := realtime.NewHub()
	go hub.Run()
	srv := httptest.NewServer(NewRouter(testPool, hub, events.New(), analytics.NoopClient{}, nil))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// In passkey-only mode no other way for a person to sign in or create an
// account is mounted, and passkey routes exist only in that mode.
func TestPasskeyOnlyUnmountsOtherSignIn(t *testing.T) {
	srv := passkeyOnlyServer(t)
	for _, path := range []string{"/auth/send-code", "/auth/verify-code", "/auth/google"} {
		if got := post(t, srv.URL+path, ""); got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
			t.Errorf("passkey-only POST %s = %d, want it unmounted", path, got)
		}
	}
	if got := post(t, srv.URL+"/auth/passkey/login/begin", ""); got != http.StatusOK {
		t.Errorf("passkey-only login begin = %d, want 200", got)
	}
	if got := post(t, testServer.URL+"/auth/passkey/login/begin", ""); got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
		t.Errorf("default router mounts passkey login (%d)", got)
	}
}

// Minting an enrollment link is an admin action; a plain member is stopped by
// the workspace role middleware before the handler runs.
func TestPasskeyLinkRequiresWorkspaceAdmin(t *testing.T) {
	srv := passkeyOnlyServer(t)
	ctx := context.Background()
	email := "passkey-member-" + uuid.NewString()[:8] + "@multica.test"
	var memberID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Member', $1) RETURNING id`, email).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM member WHERE user_id = $1`, memberID)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, memberID)
	})
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, memberID); err != nil {
		t.Fatal(err)
	}
	token, err := generateTestJWT(memberID, email, "Member")
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/workspaces/" + testWorkspaceID + "/invitations/" + uuid.NewString() + "/passkey-link"
	if got := post(t, srv.URL+path, token); got != http.StatusForbidden {
		t.Errorf("member POST passkey-link = %d, want 403", got)
	}
	if got := post(t, srv.URL+path, ""); got != http.StatusUnauthorized {
		t.Errorf("anonymous POST passkey-link = %d, want 401", got)
	}
}

func TestPasskeyOnlyRefusesOriginWithoutHTTPS(t *testing.T) {
	t.Setenv("MULTICA_PASSKEY_ONLY", "true")
	defer func() {
		if recover() == nil {
			t.Fatal("passkey-only with an http origin must not start")
		}
	}()
	passkeyRelyingPartyFromEnv("http://100.64.0.1:3000")
}
