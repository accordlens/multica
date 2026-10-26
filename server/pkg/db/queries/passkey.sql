-- name: CreateWebauthnCredential :one
INSERT INTO webauthn_credential (user_id, credential_id, credential, name)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListWebauthnCredentialsByUser :many
SELECT * FROM webauthn_credential
WHERE user_id = $1
ORDER BY created_at ASC;

-- name: GetWebauthnCredentialByCredentialID :one
SELECT * FROM webauthn_credential
WHERE credential_id = $1;

-- name: CountWebauthnCredentialsByUser :one
SELECT count(*) FROM webauthn_credential
WHERE user_id = $1;

-- name: UpdateWebauthnCredentialAfterLogin :exec
UPDATE webauthn_credential
SET credential = $2, last_used_at = now()
WHERE id = $1;

-- name: DeleteWebauthnCredentialIfNotLast :execrows
-- Deletes one of the user's passkeys unless it is their only one; a person
-- without any passkey could no longer sign in.
DELETE FROM webauthn_credential
WHERE webauthn_credential.id = $1
  AND webauthn_credential.user_id = $2
  AND (SELECT count(*) FROM webauthn_credential c WHERE c.user_id = $2) > 1;

-- name: CreateWebauthnChallenge :one
INSERT INTO webauthn_challenge (purpose, user_id, enrollment_token_id, session_data, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ConsumeWebauthnChallenge :one
-- Single use: the finish call deletes the row it answers, so a replayed
-- response finds nothing.
DELETE FROM webauthn_challenge
WHERE id = $1 AND purpose = $2
RETURNING *;

-- name: DeleteExpiredWebauthnChallenges :exec
DELETE FROM webauthn_challenge
WHERE expires_at < now();

-- name: CreatePasskeyEnrollmentToken :one
INSERT INTO passkey_enrollment_token (user_id, token_hash, created_by, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUsablePasskeyEnrollmentToken :one
SELECT * FROM passkey_enrollment_token
WHERE token_hash = $1
  AND used_at IS NULL
  AND expires_at > now();

-- name: UsePasskeyEnrollmentToken :execrows
UPDATE passkey_enrollment_token
SET used_at = now()
WHERE id = $1
  AND used_at IS NULL
  AND expires_at > now();

-- name: CountMembershipsByUser :one
SELECT count(*) FROM member
WHERE user_id = $1;
