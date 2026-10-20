// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

afterEach(() => vi.unstubAllGlobals());

function respond(body: unknown) {
  const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status: 200 }));
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

const api = () => new ApiClient("https://api.example.test");

describe("passkey API", () => {
  it("returns ceremony options and posts the credential back with its challenge", async () => {
    respond({ challenge_id: "c1", options: { publicKey: { challenge: "abc", rpId: "m.example" } } });
    await expect(api().passkeyLoginBegin()).resolves.toEqual({
      challenge_id: "c1",
      options: { publicKey: { challenge: "abc", rpId: "m.example" } },
    });

    const fetch = respond({ token: "jwt", user: { id: "u1", email: "a@b.c" } });
    const res = await api().passkeyLoginFinish("c1", { id: "cred" });
    expect(res.token).toBe("jwt");
    expect(res.user.id).toBe("u1");
    expect(JSON.parse(fetch.mock.calls[0]![1].body)).toEqual({ challenge_id: "c1", credential: { id: "cred" } });
  });

  it.each([{}, { challenge_id: "" , options: { publicKey: {} } }, { challenge_id: "c1" }, { challenge_id: "c1", options: "x" }])(
    "rejects a malformed ceremony instead of prompting with nothing: %j",
    async (body) => {
      respond(body);
      await expect(api().passkeyLoginBegin()).rejects.toThrow("Malformed response");
    },
  );

  it.each([{}, { token: "jwt" }, { token: 1, user: { id: "u1" } }, { token: "jwt", user: {} }])(
    "rejects a malformed sign-in result instead of half signing in: %j",
    async (body) => {
      respond(body);
      await expect(api().passkeyEnrollFinish("c1", {}, "")).rejects.toThrow("Malformed response");
    },
  );

  it("falls back to an empty passkey list and rejects malformed writes", async () => {
    respond({ nope: true });
    await expect(api().listMyPasskeys()).resolves.toEqual([]);
    respond({ name: "missing id" });
    await expect(api().finishAddPasskey("c1", {}, "x")).rejects.toThrow("Malformed response");
    respond({ expires_at: "2026-10-21T00:00:00Z" });
    await expect(api().createInvitationPasskeyLink("w", "i")).rejects.toThrow("Malformed response");
  });
});
