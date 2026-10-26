package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/passkey"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	testPasskeyOrigin = "https://multica.test.ts.net"
	testPasskeyRPID   = "multica.test.ts.net"
)

// usePasskeyRP switches the shared test handler into passkey-only mode for
// one test.
func usePasskeyRP(t *testing.T) {
	t.Helper()
	cfg, err := passkey.ConfigFromAppURL(testPasskeyOrigin, "")
	if err != nil {
		t.Fatal(err)
	}
	rp, err := cfg.WebAuthn()
	if err != nil {
		t.Fatal(err)
	}
	prevRP, prevURL := testHandler.Passkey, testHandler.cfg.AppURL
	testHandler.Passkey, testHandler.cfg.AppURL = rp, testPasskeyOrigin
	t.Cleanup(func() { testHandler.Passkey, testHandler.cfg.AppURL = prevRP, prevURL })
}

// passkeyTestUser inserts a user with no passkeys and removes it, its
// passkeys and its enrollment tokens afterwards.
func passkeyTestUser(t *testing.T) db.User {
	t.Helper()
	email := fmt.Sprintf("passkey-%s@multica.test", uuid.NewString()[:8])
	id := dbfx.Insert(t, "user", testutil.Cols{"name": "Passkey Tester", "email": email})
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM webauthn_credential WHERE user_id = $1`, id)
		testPool.Exec(ctx, `DELETE FROM passkey_enrollment_token WHERE user_id = $1`, id)
		testPool.Exec(ctx, `DELETE FROM webauthn_challenge WHERE user_id = $1`, id)
	})
	user, err := testHandler.Queries.GetUser(context.Background(), parseUUID(id))
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func enrollmentToken(t *testing.T, user db.User) string {
	t.Helper()
	token, _, err := passkey.IssueEnrollmentToken(context.Background(), testHandler.Queries, user.ID, pgtype.UUID{})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func beginCeremony(t *testing.T, h http.HandlerFunc, body any, headers ...string) ceremony {
	t.Helper()
	var c ceremony
	req := testutil.WithHeaders(testutil.JSONRequest(http.MethodPost, "/", body), headers...)
	testutil.Call(t, h, req).Want(http.StatusOK).JSON(&c)
	if c.ChallengeID == "" || c.Options.PublicKey.Challenge == "" {
		t.Fatalf("begin returned no challenge: %+v", c)
	}
	return c
}

func finishBody(c ceremony, cred json.RawMessage) map[string]any {
	return map[string]any{"challenge_id": c.ChallengeID, "credential": cred, "name": "Test key"}
}

// enrollPasskey registers a first passkey with a fresh enrollment link and
// returns the authenticator plus the session token the enrollment issued.
func enrollPasskey(t *testing.T, user db.User) (*fakeAuthenticator, string) {
	t.Helper()
	a := newFakeAuthenticator(t)
	c := beginCeremony(t, testHandler.PasskeyEnrollBegin, map[string]string{"token": enrollmentToken(t, user)})
	var out LoginResponse
	testutil.Call(t, testHandler.PasskeyEnrollFinish, testutil.JSONRequest(http.MethodPost, "/", finishBody(c, a.register(t, c, signing{})))).
		Want(http.StatusOK).JSON(&out)
	if out.User.ID != uuidToString(user.ID) || out.Token == "" {
		t.Fatalf("enroll logged in as %q, want %q", out.User.ID, uuidToString(user.ID))
	}
	return a, out.Token
}

func passkeyLogin(t *testing.T, a *fakeAuthenticator, s signing, want int) *testutil.Response {
	t.Helper()
	c := beginCeremony(t, testHandler.PasskeyLoginBegin, map[string]any{})
	return testutil.Call(t, testHandler.PasskeyLoginFinish, testutil.JSONRequest(http.MethodPost, "/", finishBody(c, a.assert(t, c, s)))).Want(want)
}

func sessionHeaders(user db.User, token string) []string {
	return []string{"X-User-ID", uuidToString(user.ID), "Authorization", "Bearer " + token}
}

func TestPasskeyEnrollThenSignIn(t *testing.T) {
	usePasskeyRP(t)
	user := passkeyTestUser(t)
	a, enrollToken := enrollPasskey(t, user)

	claims, err := auth.ParseSessionToken(enrollToken)
	if err != nil || auth.PasskeyAuthTime(claims).IsZero() {
		t.Fatalf("enrollment session lacks passkey auth time: %v %v", claims, err)
	}

	var out LoginResponse
	resp := passkeyLogin(t, a, signing{}, http.StatusOK).JSON(&out)
	if out.User.ID != uuidToString(user.ID) {
		t.Fatalf("signed in as %q, want %q", out.User.ID, uuidToString(user.ID))
	}
	if !strings.Contains(resp.Header().Get("Set-Cookie"), auth.AuthCookieName+"=") {
		t.Fatalf("sign-in did not set the auth cookie: %q", resp.Header().Values("Set-Cookie"))
	}

	var lastUsed pgtype.Timestamptz
	dbfx.QueryRow(t, `SELECT last_used_at FROM webauthn_credential WHERE user_id = $1`, uuidToString(user.ID)).Scan(&lastUsed)
	if !lastUsed.Valid {
		t.Fatal("sign-in did not record last_used_at")
	}
}

func TestPasskeyEnrollmentLinkIsSingleUse(t *testing.T) {
	usePasskeyRP(t)
	user := passkeyTestUser(t)
	token := enrollmentToken(t, user)

	a := newFakeAuthenticator(t)
	c := beginCeremony(t, testHandler.PasskeyEnrollBegin, map[string]string{"token": token})
	// A second ceremony begun with the same link before the first finishes.
	c2 := beginCeremony(t, testHandler.PasskeyEnrollBegin, map[string]string{"token": token})
	testutil.Call(t, testHandler.PasskeyEnrollFinish, testutil.JSONRequest(http.MethodPost, "/", finishBody(c, a.register(t, c, signing{})))).Want(http.StatusOK)

	b := newFakeAuthenticator(t)
	testutil.Call(t, testHandler.PasskeyEnrollFinish, testutil.JSONRequest(http.MethodPost, "/", finishBody(c2, b.register(t, c2, signing{})))).Want(http.StatusBadRequest)
	testutil.Call(t, testHandler.PasskeyEnrollBegin, testutil.JSONRequest(http.MethodPost, "/", map[string]string{"token": token})).Want(http.StatusBadRequest)

	if n, _ := testHandler.Queries.CountWebauthnCredentialsByUser(context.Background(), user.ID); n != 1 {
		t.Fatalf("passkeys after reusing a link = %d, want 1", n)
	}
}

func TestPasskeyEnrollmentRejectsBadLinks(t *testing.T) {
	usePasskeyRP(t)
	user := passkeyTestUser(t)
	expired := enrollmentToken(t, user)
	dbfx.Exec(t, `UPDATE passkey_enrollment_token SET expires_at = now() - interval '1 minute' WHERE token_hash = $1`, auth.HashToken(expired))

	for name, token := range map[string]string{"unknown": "not-a-real-token", "expired": expired, "empty": ""} {
		t.Run(name, func(t *testing.T) {
			testutil.Call(t, testHandler.PasskeyEnrollBegin, testutil.JSONRequest(http.MethodPost, "/", map[string]string{"token": token})).Want(http.StatusBadRequest)
		})
	}
}

func TestPasskeySignInRejectsTamperedCeremonies(t *testing.T) {
	usePasskeyRP(t)
	user := passkeyTestUser(t)
	a, _ := enrollPasskey(t, user)

	cases := map[string]signing{
		"wrong origin":    {origin: "https://evil.example"},
		"http origin":     {origin: "http://multica.test.ts.net"},
		"wrong rp id":     {rpID: "evil.example"},
		"parent rp id":    {rpID: "ts.net"},
		"wrong challenge": {challenge: b64.EncodeToString([]byte("a challenge we never issued!"))},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			passkeyLogin(t, a, s, http.StatusUnauthorized)
		})
	}
	t.Run("unknown credential", func(t *testing.T) {
		stranger := newFakeAuthenticator(t)
		stranger.userHandle = user.ID.Bytes[:]
		passkeyLogin(t, stranger, signing{}, http.StatusUnauthorized)
	})
	t.Run("user handle of another account", func(t *testing.T) {
		other := passkeyTestUser(t)
		saved := a.userHandle
		a.userHandle = other.ID.Bytes[:]
		t.Cleanup(func() { a.userHandle = saved })
		passkeyLogin(t, a, signing{}, http.StatusUnauthorized)
	})
	// The honest ceremony still works after all the rejected ones.
	passkeyLogin(t, a, signing{}, http.StatusOK)
}

func TestPasskeySignInChallengeIsSingleUseAndExpires(t *testing.T) {
	usePasskeyRP(t)
	user := passkeyTestUser(t)
	a, _ := enrollPasskey(t, user)

	t.Run("replayed response", func(t *testing.T) {
		c := beginCeremony(t, testHandler.PasskeyLoginBegin, map[string]any{})
		body := finishBody(c, a.assert(t, c, signing{}))
		testutil.Call(t, testHandler.PasskeyLoginFinish, testutil.JSONRequest(http.MethodPost, "/", body)).Want(http.StatusOK)
		testutil.Call(t, testHandler.PasskeyLoginFinish, testutil.JSONRequest(http.MethodPost, "/", body)).Want(http.StatusBadRequest)
	})
	t.Run("expired challenge", func(t *testing.T) {
		c := beginCeremony(t, testHandler.PasskeyLoginBegin, map[string]any{})
		dbfx.Exec(t, `UPDATE webauthn_challenge SET expires_at = now() - interval '1 second' WHERE id = $1`, c.ChallengeID)
		testutil.Call(t, testHandler.PasskeyLoginFinish, testutil.JSONRequest(http.MethodPost, "/", finishBody(c, a.assert(t, c, signing{})))).Want(http.StatusBadRequest)
	})
	t.Run("registration challenge used for sign-in", func(t *testing.T) {
		c := beginCeremony(t, testHandler.PasskeyEnrollBegin, map[string]string{"token": enrollmentToken(t, user)})
		testutil.Call(t, testHandler.PasskeyLoginFinish, testutil.JSONRequest(http.MethodPost, "/", finishBody(c, a.assert(t, c, signing{})))).Want(http.StatusBadRequest)
	})
	t.Run("malformed challenge id", func(t *testing.T) {
		c := beginCeremony(t, testHandler.PasskeyLoginBegin, map[string]any{})
		body := finishBody(c, a.assert(t, c, signing{}))
		body["challenge_id"] = "nope"
		testutil.Call(t, testHandler.PasskeyLoginFinish, testutil.JSONRequest(http.MethodPost, "/", body)).Want(http.StatusBadRequest)
	})
}

func TestPasskeySignInRejectsCounterRegression(t *testing.T) {
	usePasskeyRP(t)
	user := passkeyTestUser(t)
	a, _ := enrollPasskey(t, user)
	passkeyLogin(t, a, signing{}, http.StatusOK)
	passkeyLogin(t, a, signing{}, http.StatusOK)
	a.counter = 0 // a cloned authenticator replaying an older counter
	passkeyLogin(t, a, signing{}, http.StatusUnauthorized)
}

func TestPasskeyBackupKeyOutlivesRemovedFirstKey(t *testing.T) {
	usePasskeyRP(t)
	user := passkeyTestUser(t)
	first, token := enrollPasskey(t, user)
	headers := sessionHeaders(user, token)

	backup := newFakeAuthenticator(t)
	c := beginCeremony(t, testHandler.BeginAddPasskey, map[string]any{}, headers...)
	var added PasskeyResponse
	testutil.Call(t, testHandler.FinishAddPasskey, testutil.WithHeaders(testutil.JSONRequest(http.MethodPost, "/", finishBody(c, backup.register(t, c, signing{}))), headers...)).
		Want(http.StatusCreated).JSON(&added)

	var list []PasskeyResponse
	testutil.Call(t, testHandler.ListMyPasskeys, testutil.WithHeaders(testutil.JSONRequest(http.MethodGet, "/", nil), headers...)).Want(http.StatusOK).JSON(&list)
	if len(list) != 2 {
		t.Fatalf("passkeys = %d, want 2", len(list))
	}
	firstID := list[0].ID
	if firstID == added.ID {
		firstID = list[1].ID
	}

	del := func(id string, want int) {
		t.Helper()
		req := testutil.WithURLParams(testutil.WithHeaders(testutil.JSONRequest(http.MethodDelete, "/", nil), headers...), "passkeyId", id)
		testutil.Call(t, testHandler.DeleteMyPasskey, req).Want(want)
	}
	del(firstID, http.StatusNoContent)
	passkeyLogin(t, first, signing{}, http.StatusUnauthorized)
	passkeyLogin(t, backup, signing{}, http.StatusOK)

	// The remaining key is the only way in, so it can not be removed.
	del(added.ID, http.StatusConflict)
	del(uuid.NewString(), http.StatusNotFound)
}

func TestPasskeyConcurrentRemovalKeepsOneCredential(t *testing.T) {
	usePasskeyRP(t)
	user := passkeyTestUser(t)
	enrollPasskey(t, user)
	_, token := enrollPasskey(t, user)
	rows, err := testHandler.Queries.ListWebauthnCredentialsByUser(context.Background(), user.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("setup credentials = %d, err %v", len(rows), err)
	}
	start := make(chan struct{})
	results := make(chan int, 2)
	for _, row := range rows {
		req := testutil.WithURLParams(testutil.WithHeaders(testutil.JSONRequest(http.MethodDelete, "/", nil), sessionHeaders(user, token)...), "passkeyId", uuidToString(row.ID))
		go func() {
			<-start
			rec := httptest.NewRecorder()
			testHandler.DeleteMyPasskey(rec, req)
			results <- rec.Code
		}()
	}
	close(start)
	statuses := map[int]int{}
	for range 2 {
		select {
		case status := <-results:
			statuses[status]++
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent passkey removal did not finish")
		}
	}
	if statuses[http.StatusNoContent] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("removal statuses = %v, want one removal and one refusal", statuses)
	}
	remaining, err := testHandler.Queries.CountWebauthnCredentialsByUser(context.Background(), user.ID)
	if err != nil || remaining != 1 {
		t.Fatalf("remaining credentials = %d, err %v", remaining, err)
	}
}

func TestPasskeyAdminLinkCannotClaimAlreadyEnrolledAccount(t *testing.T) {
	usePasskeyRP(t)
	user := passkeyTestUser(t)
	link, _, err := passkey.IssueEnrollmentToken(context.Background(), testHandler.Queries, user.ID, parseUUID(testUserID))
	if err != nil {
		t.Fatal(err)
	}
	a := newFakeAuthenticator(t)
	c := beginCeremony(t, testHandler.PasskeyEnrollBegin, map[string]string{"token": link})
	// The host's enrollment claims this account while the admin link's
	// browser ceremony is still pending. That link may no longer add a key.
	enrollPasskey(t, user)
	testutil.Call(t, testHandler.PasskeyEnrollFinish, testutil.JSONRequest(http.MethodPost, "/", finishBody(c, a.register(t, c, signing{})))).Want(http.StatusBadRequest)
	remaining, err := testHandler.Queries.CountWebauthnCredentialsByUser(context.Background(), user.ID)
	if err != nil || remaining != 1 {
		t.Fatalf("credentials = %d, err %v", remaining, err)
	}
}

func TestPasskeyManagementRequiresFreshBrowserSession(t *testing.T) {
	usePasskeyRP(t)
	user := passkeyTestUser(t)
	_, token := enrollPasskey(t, user)

	stale, err := testHandler.issueJWTWithClaims(user, jwt.MapClaims{auth.PasskeyAuthTimeClaim: time.Now().Add(-passkey.FreshAuthWindow - time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	cliToken, err := testHandler.issueJWT(user) // what /api/cli-token mints
	if err != nil {
		t.Fatal(err)
	}
	otherUser := passkeyTestUser(t)

	for name, headers := range map[string][]string{
		"stale passkey session": sessionHeaders(user, stale),
		"cli token":             sessionHeaders(user, cliToken),
		"personal access token": sessionHeaders(user, "mul_0123456789abcdef"),
		"token of another user": {"X-User-ID", uuidToString(otherUser.ID), "Authorization", "Bearer " + token},
	} {
		t.Run(name, func(t *testing.T) {
			testutil.Call(t, testHandler.BeginAddPasskey, testutil.WithHeaders(testutil.JSONRequest(http.MethodPost, "/", map[string]any{}), headers...)).Want(http.StatusForbidden)
		})
	}

	t.Run("challenge begun by another account", func(t *testing.T) {
		_, otherToken := enrollPasskey(t, otherUser)
		c := beginCeremony(t, testHandler.BeginAddPasskey, map[string]any{}, sessionHeaders(otherUser, otherToken)...)
		a := newFakeAuthenticator(t)
		testutil.Call(t, testHandler.FinishAddPasskey, testutil.WithHeaders(testutil.JSONRequest(http.MethodPost, "/", finishBody(c, a.register(t, c, signing{}))), sessionHeaders(user, token)...)).
			Want(http.StatusBadRequest)
	})
}

func TestInvitationPasskeyLink(t *testing.T) {
	usePasskeyRP(t)

	invite := func(t *testing.T, email string) string {
		t.Helper()
		return dbfx.Insert(t, "workspace_invitation", testutil.Cols{
			"workspace_id": testWorkspaceID, "inviter_id": testUserID, "invitee_email": email,
			"role": "member", "status": "pending", "expires_at": testutil.Raw("now() + interval '1 day'"),
		})
	}
	call := func(t *testing.T, invitationID string, want int) *testutil.Response {
		t.Helper()
		req := testutil.WithURLParams(
			testutil.WithHeaders(testutil.JSONRequest(http.MethodPost, "/", nil), "X-User-ID", testUserID),
			"id", testWorkspaceID, "invitationId", invitationID)
		return testutil.Call(t, testHandler.CreateInvitationPasskeyLink, req).Want(want)
	}

	t.Run("new invitee gets an account and a working link", func(t *testing.T) {
		email := fmt.Sprintf("invitee-%s@multica.test", uuid.NewString()[:8])
		invID := invite(t, email)
		var link PasskeyLinkResponse
		call(t, invID, http.StatusCreated).JSON(&link)
		user, err := testHandler.Queries.GetUserByEmail(context.Background(), email)
		if err != nil {
			t.Fatalf("invitee account not created: %v", err)
		}
		t.Cleanup(func() {
			ctx := context.Background()
			testPool.Exec(ctx, `DELETE FROM webauthn_credential WHERE user_id = $1`, user.ID)
			testPool.Exec(ctx, `DELETE FROM passkey_enrollment_token WHERE user_id = $1`, user.ID)
			testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, user.ID)
		})
		prefix := testPasskeyOrigin + "/login?enroll="
		if !strings.HasPrefix(link.URL, prefix) {
			t.Fatalf("link = %q, want prefix %q", link.URL, prefix)
		}
		token := strings.TrimPrefix(link.URL, prefix)
		a := newFakeAuthenticator(t)
		c := beginCeremony(t, testHandler.PasskeyEnrollBegin, map[string]string{"token": token})
		testutil.Call(t, testHandler.PasskeyEnrollFinish, testutil.JSONRequest(http.MethodPost, "/", finishBody(c, a.register(t, c, signing{})))).Want(http.StatusOK)

		// Once the invitee holds a passkey, admins can no longer mint links for them.
		call(t, invID, http.StatusConflict)
	})
	t.Run("existing workspace member can not be taken over", func(t *testing.T) {
		call(t, invite(t, handlerTestEmail), http.StatusConflict)
	})
	t.Run("existing account without memberships or passkeys stays operator controlled", func(t *testing.T) {
		user := passkeyTestUser(t)
		call(t, invite(t, user.Email), http.StatusConflict)
		var links int
		if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM passkey_enrollment_token WHERE user_id = $1`, user.ID).Scan(&links); err != nil {
			t.Fatal(err)
		}
		if links != 0 {
			t.Fatalf("admin minted %d enrollment links for an existing account", links)
		}
	})
	t.Run("invitation from another workspace", func(t *testing.T) {
		otherWS := dbfx.Insert(t, "workspace", testutil.Cols{"name": "Other", "slug": "passkey-other-" + uuid.NewString()[:8], "issue_prefix": "OTH"})
		inv := dbfx.Insert(t, "workspace_invitation", testutil.Cols{
			"workspace_id": otherWS, "inviter_id": testUserID, "invitee_email": "x-" + uuid.NewString()[:8] + "@multica.test",
			"role": "member", "status": "pending", "expires_at": testutil.Raw("now() + interval '1 day'"),
		})
		call(t, inv, http.StatusNotFound)
	})
	t.Run("expired invitation", func(t *testing.T) {
		inv := invite(t, "late-"+uuid.NewString()[:8]+"@multica.test")
		dbfx.Exec(t, `UPDATE workspace_invitation SET expires_at = now() - interval '1 minute' WHERE id = $1`, inv)
		call(t, inv, http.StatusConflict)
	})
}

func TestConfigReportsPasskeyOnly(t *testing.T) {
	usePasskeyRP(t)
	var cfg AppConfig
	testutil.Call(t, testHandler.GetConfig, testutil.JSONRequest(http.MethodGet, "/", nil)).Want(http.StatusOK).JSON(&cfg)
	if !cfg.PasskeyOnly || cfg.AllowSignup || cfg.GoogleClientID != "" {
		t.Fatalf("config = %+v, want passkey_only without signup or Google", cfg)
	}
}
