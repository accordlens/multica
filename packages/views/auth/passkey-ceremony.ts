import {
  browserSupportsWebAuthn,
  startAuthentication,
  startRegistration,
  WebAuthnError,
  type PublicKeyCredentialCreationOptionsJSON,
  type PublicKeyCredentialRequestOptionsJSON,
} from "@simplewebauthn/browser";
import { errorCode } from "@multica/core/api";

// Browser side of passkey-only sign-in (ACCO-55). The server's WebAuthn
// library emits options in the standard JSON form; the browser validates
// them, so they pass through without a local schema.

export function signInCredential(publicKey: Record<string, unknown>) {
  return startAuthentication({
    optionsJSON: publicKey as unknown as PublicKeyCredentialRequestOptionsJSON,
  });
}

export function registerCredential(publicKey: Record<string, unknown>) {
  return startRegistration({
    optionsJSON: publicKey as unknown as PublicKeyCredentialCreationOptionsJSON,
  });
}

export type PasskeyFailure =
  | "unsupported"
  | "cancelled"
  | "already_registered"
  | "reauth_required"
  | "other";

/** Classify a failed ceremony so the UI can say something a person can act on. */
export function passkeyFailure(err: unknown): PasskeyFailure {
  if (!browserSupportsWebAuthn()) return "unsupported";
  if (errorCode(err) === "passkey_reauth_required") return "reauth_required";
  if (err instanceof WebAuthnError) {
    if (err.code === "ERROR_AUTHENTICATOR_PREVIOUSLY_REGISTERED") return "already_registered";
    if (err.code === "ERROR_CEREMONY_ABORTED") return "cancelled";
  }
  const name = err instanceof Error ? err.name : "";
  if (name === "NotAllowedError" || name === "AbortError") return "cancelled";
  return "other";
}
