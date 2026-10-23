-- Passkey (WebAuthn) sign-in for people (ACCO-55). Machine credentials
-- (PATs, daemon tokens, task tokens) are unaffected.

-- One row per registered passkey. `credential` is the go-webauthn
-- Credential serialized as JSON; `credential_id` is duplicated out of it so
-- discoverable sign-in can look the row up by the authenticator's raw id.
CREATE TABLE IF NOT EXISTS webauthn_credential (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    credential_id BYTEA NOT NULL,
    credential JSONB NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

-- Server-side ceremony state. A row is consumed (deleted) by the finish
-- call, so a challenge can be answered at most once.
CREATE TABLE IF NOT EXISTS webauthn_challenge (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    purpose TEXT NOT NULL CHECK (purpose IN ('login', 'register')),
    user_id UUID,
    enrollment_token_id UUID,
    session_data JSONB NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One-time links that let a person register their first passkey. Issued by
-- the host operator (owner bootstrap, lost-key recovery) or by a workspace
-- admin for a pending invitation. Only the SHA-256 hash is stored.
CREATE TABLE IF NOT EXISTS passkey_enrollment_token (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    token_hash TEXT NOT NULL,
    created_by UUID,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
