package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/passkey"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Passkey-only sign-in for people (ACCO-55).
//
// Public ceremonies:
//   - login:  POST /auth/passkey/login/{begin,finish} — discoverable, no email.
//   - enroll: POST /auth/passkey/enroll/{begin,finish} — registers a first
//     passkey with a one-time link minted by the host operator
//     (cmd/passkey-enroll) or by a workspace admin for a pending invitation.
//
// Signed-in people manage their own passkeys under /api/me/passkeys. Adding
// or removing one needs a browser session that presented a passkey within
// passkey.FreshAuthWindow; PATs, daemon tokens and CLI-minted JWTs never
// qualify. Every begin call stores its challenge server-side and every finish
// call consumes it, so a challenge answers at most one ceremony.

const (
	challengePurposeLogin    = "login"
	challengePurposeRegister = "register"

	errPasskeyChallenge = "passkey challenge is invalid or expired"
	errPasskeyLogin     = "passkey sign-in failed"
	errPasskeyEnrollURL = "enrollment link is invalid or expired"
)

type passkeyCeremonyResponse struct {
	ChallengeID string `json:"challenge_id"`
	Options     any    `json:"options"`
}

type passkeyFinishRequest struct {
	ChallengeID string          `json:"challenge_id"`
	Credential  json.RawMessage `json:"credential"`
	Name        string          `json:"name"`
}

type passkeyEnrollBeginRequest struct {
	Token string `json:"token"`
}

type PasskeyResponse struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}

type PasskeyLinkResponse struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

func passkeyToResponse(c db.WebauthnCredential) PasskeyResponse {
	return PasskeyResponse{
		ID:         uuidToString(c.ID),
		Name:       c.Name,
		CreatedAt:  timestampToString(c.CreatedAt),
		LastUsedAt: timestampToPtr(c.LastUsedAt),
	}
}

// passkeyName bounds the user-chosen label for a passkey.
func passkeyName(name string) string {
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > 64 {
		name = string(r[:64])
	}
	if name == "" {
		name = "Passkey"
	}
	return name
}

// ---------------------------------------------------------------------------
// Challenge storage
// ---------------------------------------------------------------------------

func (h *Handler) saveChallenge(ctx context.Context, purpose string, userID, tokenID pgtype.UUID, session *webauthn.SessionData) (string, error) {
	data, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	_ = h.Queries.DeleteExpiredWebauthnChallenges(ctx)
	row, err := h.Queries.CreateWebauthnChallenge(ctx, db.CreateWebauthnChallengeParams{
		Purpose:           purpose,
		UserID:            userID,
		EnrollmentTokenID: tokenID,
		SessionData:       data,
		ExpiresAt:         pgtype.Timestamptz{Time: time.Now().Add(passkey.ChallengeTTL), Valid: true},
	})
	if err != nil {
		return "", err
	}
	return uuidToString(row.ID), nil
}

// consumeChallenge deletes and returns the stored ceremony state. Unknown,
// already-used, wrong-purpose and expired challenges are indistinguishable to
// the caller.
func (h *Handler) consumeChallenge(w http.ResponseWriter, r *http.Request, challengeID, purpose string) (db.WebauthnChallenge, webauthn.SessionData, bool) {
	id, err := parseUUIDStrict(challengeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errPasskeyChallenge)
		return db.WebauthnChallenge{}, webauthn.SessionData{}, false
	}
	row, err := h.Queries.ConsumeWebauthnChallenge(r.Context(), db.ConsumeWebauthnChallengeParams{ID: id, Purpose: purpose})
	if err != nil {
		if !isNotFound(err) {
			slog.Warn("passkey: consume challenge failed", append(logger.RequestAttrs(r), "error", err)...)
		}
		writeError(w, http.StatusBadRequest, errPasskeyChallenge)
		return db.WebauthnChallenge{}, webauthn.SessionData{}, false
	}
	var session webauthn.SessionData
	if !row.ExpiresAt.Valid || !row.ExpiresAt.Time.After(time.Now()) || json.Unmarshal(row.SessionData, &session) != nil {
		writeError(w, http.StatusBadRequest, errPasskeyChallenge)
		return db.WebauthnChallenge{}, webauthn.SessionData{}, false
	}
	return row, session, true
}

func decodePasskeyFinish(w http.ResponseWriter, r *http.Request) (passkeyFinishRequest, bool) {
	var req passkeyFinishRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || len(req.Credential) == 0 {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return req, false
	}
	return req, true
}

// ---------------------------------------------------------------------------
// Sign-in
// ---------------------------------------------------------------------------

// PasskeyLoginBegin starts a discoverable sign-in: the browser lets the person
// pick any passkey they hold for this relying party.
func (h *Handler) PasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	assertion, session, err := h.Passkey.BeginDiscoverableLogin()
	if err != nil {
		slog.Error("passkey: begin login failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to start passkey sign-in")
		return
	}
	id, err := h.saveChallenge(r.Context(), challengePurposeLogin, pgtype.UUID{}, pgtype.UUID{}, session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start passkey sign-in")
		return
	}
	writeJSON(w, http.StatusOK, passkeyCeremonyResponse{ChallengeID: id, Options: assertion})
}

func (h *Handler) PasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePasskeyFinish(w, r)
	if !ok {
		return
	}
	_, session, ok := h.consumeChallenge(w, r, req.ChallengeID, challengePurposeLogin)
	if !ok {
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(req.Credential)
	if err != nil {
		writeError(w, http.StatusBadRequest, errPasskeyLogin)
		return
	}

	var stored db.WebauthnCredential
	lookup := func(rawID, userHandle []byte) (webauthn.User, error) {
		row, err := h.Queries.GetWebauthnCredentialByCredentialID(r.Context(), rawID)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(row.UserID.Bytes[:], userHandle) {
			return nil, errors.New("user handle does not own credential")
		}
		user, err := h.Queries.GetUser(r.Context(), row.UserID)
		if err != nil {
			return nil, err
		}
		stored = row
		return passkey.LoadUser(r.Context(), h.Queries, user)
	}
	waUser, cred, err := h.Passkey.ValidatePasskeyLogin(lookup, session, parsed)
	if err != nil {
		slog.Info("passkey: sign-in rejected", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusUnauthorized, errPasskeyLogin)
		return
	}
	if cred.Authenticator.CloneWarning {
		slog.Warn("passkey: sign-in rejected, signature counter went backwards", append(logger.RequestAttrs(r), "credential", uuidToString(stored.ID))...)
		writeError(w, http.StatusUnauthorized, errPasskeyLogin)
		return
	}
	if data, err := json.Marshal(cred); err == nil {
		if err := h.Queries.UpdateWebauthnCredentialAfterLogin(r.Context(), db.UpdateWebauthnCredentialAfterLoginParams{ID: stored.ID, Credential: data}); err != nil {
			slog.Warn("passkey: failed to record use", append(logger.RequestAttrs(r), "error", err)...)
		}
	}
	h.completePasskeyLogin(w, r, waUser.(passkey.User).Row)
}

// completePasskeyLogin issues the session exactly as the other login paths do
// (auth cookies, CloudFront cookies, LoginResponse) and stamps it with the
// passkey presentation time.
func (h *Handler) completePasskeyLogin(w http.ResponseWriter, r *http.Request, user db.User) {
	tokenString, err := h.issueJWTWithClaims(user, jwt.MapClaims{auth.PasskeyAuthTimeClaim: time.Now().Unix()})
	if err != nil {
		if errors.Is(err, auth.ErrTemporarilyDisabledUser) {
			writeError(w, http.StatusForbidden, auth.TemporarilyDisabledUserError)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	if err := auth.SetAuthCookies(w, tokenString); err != nil {
		slog.Warn("failed to set auth cookies", "error", err)
	}
	if h.CFSigner != nil {
		for _, cookie := range h.CFSigner.SignedCookies(time.Now().Add(auth.AuthTokenTTL())) {
			http.SetCookie(w, cookie)
		}
	}
	slog.Info("user logged in via passkey", append(logger.RequestAttrs(r), "user_id", uuidToString(user.ID))...)
	writeJSON(w, http.StatusOK, LoginResponse{Token: tokenString, User: h.userToResponse(user)})
}

// ---------------------------------------------------------------------------
// Enrollment with a one-time link
// ---------------------------------------------------------------------------

func (h *Handler) PasskeyEnrollBegin(w http.ResponseWriter, r *http.Request) {
	var req passkeyEnrollBeginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || strings.TrimSpace(req.Token) == "" {
		writeError(w, http.StatusBadRequest, errPasskeyEnrollURL)
		return
	}
	token, err := h.Queries.GetUsablePasskeyEnrollmentToken(r.Context(), auth.HashToken(strings.TrimSpace(req.Token)))
	if err != nil {
		writeError(w, http.StatusBadRequest, errPasskeyEnrollURL)
		return
	}
	user, err := h.Queries.GetUser(r.Context(), token.UserID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errPasskeyEnrollURL)
		return
	}
	h.beginRegistration(w, r, user, token.ID)
}

func (h *Handler) PasskeyEnrollFinish(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePasskeyFinish(w, r)
	if !ok {
		return
	}
	challenge, session, ok := h.consumeChallenge(w, r, req.ChallengeID, challengePurposeRegister)
	if !ok {
		return
	}
	if !challenge.EnrollmentTokenID.Valid {
		writeError(w, http.StatusBadRequest, errPasskeyChallenge)
		return
	}
	user, cred, ok := h.verifyRegistration(w, r, challenge, session, req.Credential)
	if !ok {
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register passkey")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if _, err := tx.Exec(r.Context(), `SELECT id FROM "user" WHERE id = $1 FOR UPDATE`, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register passkey")
		return
	}
	var createdBy pgtype.UUID
	if err := tx.QueryRow(r.Context(), `SELECT created_by FROM passkey_enrollment_token WHERE id = $1`, challenge.EnrollmentTokenID).Scan(&createdBy); err != nil {
		writeError(w, http.StatusBadRequest, errPasskeyEnrollURL)
		return
	}
	if createdBy.Valid {
		creds, err1 := qtx.CountWebauthnCredentialsByUser(r.Context(), user.ID)
		members, err2 := qtx.CountMembershipsByUser(r.Context(), user.ID)
		if err1 != nil || err2 != nil {
			writeError(w, http.StatusInternalServerError, "failed to register passkey")
			return
		}
		if creds > 0 || members > 0 {
			writeError(w, http.StatusBadRequest, errPasskeyEnrollURL)
			return
		}
	}
	used, err := qtx.UsePasskeyEnrollmentToken(r.Context(), challenge.EnrollmentTokenID)
	if err != nil || used != 1 {
		writeError(w, http.StatusBadRequest, errPasskeyEnrollURL)
		return
	}
	if _, ok := h.storeCredential(w, r, qtx, user, cred, req.Name); !ok {
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register passkey")
		return
	}
	h.completePasskeyLogin(w, r, user)
}

// beginRegistration starts a registration ceremony for user, excluding the
// passkeys they already hold so the same authenticator is not enrolled twice.
func (h *Handler) beginRegistration(w http.ResponseWriter, r *http.Request, user db.User, tokenID pgtype.UUID) {
	if auth.IsTemporarilyDisabledUser(uuidToString(user.ID), user.Email) {
		writeError(w, http.StatusForbidden, auth.TemporarilyDisabledUserError)
		return
	}
	waUser, err := passkey.LoadUser(r.Context(), h.Queries, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start passkey registration")
		return
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(waUser.Credentials))
	for _, c := range waUser.Credentials {
		exclude = append(exclude, c.Descriptor())
	}
	creation, session, err := h.Passkey.BeginRegistration(waUser, webauthn.WithExclusions(exclude))
	if err != nil {
		slog.Error("passkey: begin registration failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to start passkey registration")
		return
	}
	id, err := h.saveChallenge(r.Context(), challengePurposeRegister, user.ID, tokenID, session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start passkey registration")
		return
	}
	writeJSON(w, http.StatusOK, passkeyCeremonyResponse{ChallengeID: id, Options: creation})
}

func (h *Handler) verifyRegistration(w http.ResponseWriter, r *http.Request, challenge db.WebauthnChallenge, session webauthn.SessionData, raw json.RawMessage) (db.User, *webauthn.Credential, bool) {
	user, err := h.Queries.GetUser(r.Context(), challenge.UserID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errPasskeyChallenge)
		return db.User{}, nil, false
	}
	waUser, err := passkey.LoadUser(r.Context(), h.Queries, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register passkey")
		return db.User{}, nil, false
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "passkey registration failed")
		return db.User{}, nil, false
	}
	cred, err := h.Passkey.CreateCredential(waUser, session, parsed)
	if err != nil {
		slog.Info("passkey: registration rejected", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusBadRequest, "passkey registration failed")
		return db.User{}, nil, false
	}
	return user, cred, true
}

func (h *Handler) storeCredential(w http.ResponseWriter, r *http.Request, q *db.Queries, user db.User, cred *webauthn.Credential, name string) (db.WebauthnCredential, bool) {
	data, err := json.Marshal(cred)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to register passkey")
		return db.WebauthnCredential{}, false
	}
	row, err := q.CreateWebauthnCredential(r.Context(), db.CreateWebauthnCredentialParams{
		UserID:       user.ID,
		CredentialID: cred.ID,
		Credential:   data,
		Name:         passkeyName(name),
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "this passkey is already registered")
			return db.WebauthnCredential{}, false
		}
		writeError(w, http.StatusInternalServerError, "failed to register passkey")
		return db.WebauthnCredential{}, false
	}
	return row, true
}

// ---------------------------------------------------------------------------
// Managing your own passkeys
// ---------------------------------------------------------------------------

// sessionUser resolves the caller from an interactive session JWT. Machine
// credentials (PATs, daemon and task tokens) are refused: they authenticate
// fine elsewhere, but never manage a person's passkeys. With fresh set, the
// session must also have presented a passkey within FreshAuthWindow.
func (h *Handler) sessionUser(w http.ResponseWriter, r *http.Request, fresh bool) (db.User, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return db.User{}, false
	}
	claims, err := auth.ParseSessionToken(rawSessionToken(r))
	if err != nil || claims["sub"] != userID {
		writeError(w, http.StatusForbidden, "passkeys can only be managed from a signed-in browser session")
		return db.User{}, false
	}
	if fresh {
		at := auth.PasskeyAuthTime(claims)
		if at.IsZero() || time.Since(at) > passkey.FreshAuthWindow {
			writeErrorCode(w, http.StatusForbidden, "passkey_reauth_required", "sign in with a passkey again to change your passkeys")
			return db.User{}, false
		}
	}
	user, err := h.Queries.GetUser(r.Context(), parseUUID(userID))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "user not found")
		return db.User{}, false
	}
	return user, true
}

// rawSessionToken mirrors middleware.extractToken: Authorization header
// first, then the auth cookie.
func rawSessionToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie(auth.AuthCookieName); err == nil {
		return c.Value
	}
	return ""
}

func (h *Handler) ListMyPasskeys(w http.ResponseWriter, r *http.Request) {
	user, ok := h.sessionUser(w, r, false)
	if !ok {
		return
	}
	rows, err := h.Queries.ListWebauthnCredentialsByUser(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list passkeys")
		return
	}
	out := make([]PasskeyResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, passkeyToResponse(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) BeginAddPasskey(w http.ResponseWriter, r *http.Request) {
	user, ok := h.sessionUser(w, r, true)
	if !ok {
		return
	}
	h.beginRegistration(w, r, user, pgtype.UUID{})
}

func (h *Handler) FinishAddPasskey(w http.ResponseWriter, r *http.Request) {
	user, ok := h.sessionUser(w, r, true)
	if !ok {
		return
	}
	req, ok := decodePasskeyFinish(w, r)
	if !ok {
		return
	}
	challenge, session, ok := h.consumeChallenge(w, r, req.ChallengeID, challengePurposeRegister)
	if !ok {
		return
	}
	// A challenge begun for another account, or from an enrollment link, can
	// not be finished from this session.
	if challenge.EnrollmentTokenID.Valid || challenge.UserID != user.ID {
		writeError(w, http.StatusBadRequest, errPasskeyChallenge)
		return
	}
	_, cred, ok := h.verifyRegistration(w, r, challenge, session, req.Credential)
	if !ok {
		return
	}
	row, ok := h.storeCredential(w, r, h.Queries, user, cred, req.Name)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, passkeyToResponse(row))
}

// DeleteMyPasskey removes one of the caller's passkeys. The last one can not
// be removed. Sessions already signed in with it stay valid until they expire
// or sign out; the key just can not start new ones.
func (h *Handler) DeleteMyPasskey(w http.ResponseWriter, r *http.Request) {
	user, ok := h.sessionUser(w, r, true)
	if !ok {
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "passkeyId"), "passkey id")
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove passkey")
		return
	}
	defer tx.Rollback(r.Context())
	// Serialize removals before taking the count snapshot. Two concurrent
	// removals must not both observe two credentials and delete the last two.
	if _, err := tx.Exec(r.Context(), `SELECT id FROM "user" WHERE id = $1 FOR UPDATE`, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove passkey")
		return
	}
	qtx := h.Queries.WithTx(tx)
	n, err := qtx.DeleteWebauthnCredentialIfNotLast(r.Context(), db.DeleteWebauthnCredentialIfNotLastParams{ID: id, UserID: user.ID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove passkey")
		return
	}
	if n == 0 {
		rows, err := qtx.ListWebauthnCredentialsByUser(r.Context(), user.ID)
		if err == nil {
			for _, row := range rows {
				if row.ID == id {
					writeError(w, http.StatusConflict, "add another passkey before removing your only one")
					return
				}
			}
		}
		writeError(w, http.StatusNotFound, "passkey not found")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove passkey")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Invitation enrollment links (workspace owner/admin)
// ---------------------------------------------------------------------------

// CreateInvitationPasskeyLink mints a one-time enrollment link for the email
// on a pending invitation, creating the account if it does not exist yet.
// The admin hands the link over out of band; the invitee registers a passkey
// with it and then accepts the invitation as usual.
//
// An account that already has a passkey or belongs to any workspace is
// refused: a workspace admin must never be able to attach a passkey to
// someone else's existing account. Those people sign in with their own
// passkey, and lost-key recovery stays with the host operator.
func (h *Handler) CreateInvitationPasskeyLink(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "id")
	requester, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	invID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "invitationId"), "invitation id")
	if !ok {
		return
	}
	inv, err := h.Queries.GetInvitation(r.Context(), invID)
	if err != nil || inv.WorkspaceID != requester.WorkspaceID {
		writeError(w, http.StatusNotFound, "invitation not found")
		return
	}
	if inv.Status != "pending" || !inv.ExpiresAt.Valid || !inv.ExpiresAt.Time.After(time.Now()) {
		writeError(w, http.StatusConflict, "invitation is no longer pending")
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create enrollment link")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	user, err := qtx.GetUserByEmail(r.Context(), inv.InviteeEmail)
	switch {
	case isNotFound(err):
		name := inv.InviteeEmail
		if at := strings.Index(name, "@"); at > 0 {
			name = name[:at]
		}
		user, err = qtx.CreateUser(r.Context(), db.CreateUserParams{Name: name, Email: inv.InviteeEmail})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create account")
			return
		}
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to load account")
		return
	}
	// Enrollment and link issuance use the same account lock, so an admin
	// cannot issue another usable first-key link as the account is claimed.
	if _, err := tx.Exec(r.Context(), `SELECT id FROM "user" WHERE id = $1 FOR UPDATE`, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load account")
		return
	}
	creds, err1 := qtx.CountWebauthnCredentialsByUser(r.Context(), user.ID)
	members, err2 := qtx.CountMembershipsByUser(r.Context(), user.ID)
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusInternalServerError, "failed to load account")
		return
	}
	if creds > 0 || members > 0 {
		writeError(w, http.StatusConflict, passkey.ErrUserNotEnrollable.Error())
		return
	}
	token, expiresAt, err := passkey.IssueEnrollmentToken(r.Context(), qtx, user.ID, requester.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create enrollment link")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create enrollment link")
		return
	}
	slog.Info("passkey enrollment link created", append(logger.RequestAttrs(r), "invitation_id", uuidToString(inv.ID), "workspace_id", workspaceID)...)
	writeJSON(w, http.StatusCreated, PasskeyLinkResponse{
		URL:       passkey.EnrollmentURL(h.cfg.AppURL, token),
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
}
