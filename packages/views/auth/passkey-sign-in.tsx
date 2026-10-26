"use client";

import { useState } from "react";
import { Button } from "@multica/ui/components/ui/button";
import { ApiError, type LoginResponse } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { useT } from "../i18n";
import { passkeyFailure, registerCredential, signInCredential } from "./passkey-ceremony";

interface PasskeySignInProps {
  /** One-time enrollment token from `/login?enroll=…`; registers a passkey instead of signing in. */
  enrollToken?: string;
  /** Runs after the session exists; owns navigation or the CLI hand-off. */
  onSignedIn: (res: LoginResponse) => Promise<void>;
}

/** The only sign-in control on a passkey-only deployment (ACCO-55). */
export function PasskeySignIn({ enrollToken, onSignedIn }: PasskeySignInProps) {
  const { t } = useT("auth");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");

  const errorMessage = (err: unknown) => {
    switch (passkeyFailure(err)) {
      case "unsupported":
        return t(($) => $.passkey.errors.unsupported);
      case "cancelled":
        return t(($) => $.passkey.errors.cancelled);
      case "already_registered":
        return t(($) => $.passkey.errors.already_registered);
      default:
        if (enrollToken && err instanceof ApiError && err.status === 400) {
          return t(($) => $.passkey.errors.link_invalid);
        }
        return t(($) => $.passkey.errors.failed);
    }
  };

  const run = async () => {
    setPending(true);
    setError("");
    try {
      const store = useAuthStore.getState();
      const res = enrollToken
        ? await store.enrollWithPasskey(enrollToken, registerCredential)
        : await store.loginWithPasskey(signInCredential);
      await onSignedIn(res);
    } catch (err) {
      setError(errorMessage(err));
      setPending(false);
    }
  };

  return (
    <div className="flex w-full flex-col gap-3">
      <Button type="button" className="w-full" size="lg" onClick={run} disabled={pending}>
        {pending
          ? t(($) => $.passkey.waiting)
          : enrollToken
            ? t(($) => $.passkey.enroll)
            : t(($) => $.passkey.sign_in)}
      </Button>
      {error && (
        <p role="alert" className="text-body text-destructive">
          {error}
        </p>
      )}
      {!enrollToken && (
        <p className="text-caption text-muted-foreground text-center">
          {t(($) => $.passkey.no_passkey_hint)}
        </p>
      )}
    </div>
  );
}
