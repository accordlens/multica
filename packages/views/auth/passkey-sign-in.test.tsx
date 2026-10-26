import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api";
import enCommon from "../locales/en/common.json";
import enAuth from "../locales/en/auth.json";

const mockLoginWithPasskey = vi.hoisted(() => vi.fn());
const mockEnrollWithPasskey = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/auth", () => ({
  useAuthStore: {
    getState: () => ({
      loginWithPasskey: mockLoginWithPasskey,
      enrollWithPasskey: mockEnrollWithPasskey,
    }),
  },
}));

import { PasskeySignIn } from "./passkey-sign-in";
import { registerCredential, signInCredential } from "./passkey-ceremony";

function Wrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={{ en: { common: enCommon, auth: enAuth } }}>
      {children}
    </I18nProvider>
  );
}

const signedIn = { token: "jwt", user: { id: "u1" } };

describe("PasskeySignIn", () => {
  beforeEach(() => {
    // jsdom has no WebAuthn; pretend the browser supports it.
    vi.stubGlobal("PublicKeyCredential", function PublicKeyCredential() {});
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  it("signs in with a passkey and hands the session to the caller", async () => {
    mockLoginWithPasskey.mockResolvedValue(signedIn);
    const onSignedIn = vi.fn().mockResolvedValue(undefined);
    render(<PasskeySignIn onSignedIn={onSignedIn} />, { wrapper: Wrapper });

    await userEvent.click(screen.getByRole("button", { name: "Sign in with passkey" }));

    expect(mockLoginWithPasskey).toHaveBeenCalledWith(signInCredential);
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledWith(signedIn));
  });

  it("explains a closed prompt and lets the person retry", async () => {
    mockLoginWithPasskey.mockRejectedValue(Object.assign(new Error("closed"), { name: "NotAllowedError" }));
    render(<PasskeySignIn onSignedIn={vi.fn()} />, { wrapper: Wrapper });

    await userEvent.click(screen.getByRole("button", { name: "Sign in with passkey" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(enAuth.passkey.errors.cancelled);
    expect(screen.getByRole("button", { name: "Sign in with passkey" })).toBeEnabled();
  });

  it("says when the browser can not use passkeys at all", async () => {
    vi.unstubAllGlobals();
    mockLoginWithPasskey.mockRejectedValue(new Error("no webauthn"));
    render(<PasskeySignIn onSignedIn={vi.fn()} />, { wrapper: Wrapper });

    await userEvent.click(screen.getByRole("button", { name: "Sign in with passkey" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(enAuth.passkey.errors.unsupported);
  });

  it("rejected passkeys get a generic message, not the server's", async () => {
    mockLoginWithPasskey.mockRejectedValue(new ApiError("passkey sign-in failed", 401, "Unauthorized", {}));
    render(<PasskeySignIn onSignedIn={vi.fn()} />, { wrapper: Wrapper });

    await userEvent.click(screen.getByRole("button", { name: "Sign in with passkey" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(enAuth.passkey.errors.failed);
  });

  it("registers with an enrollment link and reports a dead link", async () => {
    mockEnrollWithPasskey.mockRejectedValue(new ApiError("enrollment link is invalid or expired", 400, "Bad Request", {}));
    render(<PasskeySignIn enrollToken="tok" onSignedIn={vi.fn()} />, { wrapper: Wrapper });

    expect(screen.queryByText(enAuth.passkey.no_passkey_hint)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Create passkey" }));

    expect(mockEnrollWithPasskey).toHaveBeenCalledWith("tok", registerCredential);
    expect(await screen.findByRole("alert")).toHaveTextContent(enAuth.passkey.errors.link_invalid);
  });
});
