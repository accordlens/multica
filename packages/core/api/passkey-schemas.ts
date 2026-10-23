import { z } from "zod";

// Passkey-only sign-in (ACCO-55). The ceremony options are produced by the
// server's WebAuthn library and handed to the browser untouched, so the
// schema only checks they are an object; the browser validates the rest.

export const PasskeyCeremonySchema = z
  .object({
    challenge_id: z.string(),
    options: z
      .object({ publicKey: z.record(z.string(), z.unknown()) })
      .loose(),
  })
  .loose();

export type PasskeyCeremony = z.infer<typeof PasskeyCeremonySchema>;

export const EMPTY_PASSKEY_CEREMONY: PasskeyCeremony = {
  challenge_id: "",
  options: { publicKey: {} },
};

export const PasskeySchema = z
  .object({
    id: z.string(),
    name: z.string().default(""),
    created_at: z.string().default(""),
    last_used_at: z.string().nullable().default(null),
  })
  .loose();

export type Passkey = z.infer<typeof PasskeySchema>;

export const PasskeyListSchema = z.array(PasskeySchema);

export const PasskeyLinkSchema = z
  .object({ url: z.string(), expires_at: z.string().default("") })
  .loose();

export type PasskeyLink = z.infer<typeof PasskeyLinkSchema>;
