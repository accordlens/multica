CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_passkey_enrollment_token_hash ON passkey_enrollment_token (token_hash);
