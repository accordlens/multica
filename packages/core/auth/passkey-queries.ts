import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { PasskeyCeremonyRunner } from "./store";

// The signed-in person's own passkeys (ACCO-55). Account-scoped, so the key
// carries no workspace id.
export const passkeyKeys = {
  mine: () => ["me", "passkeys"] as const,
};

export function myPasskeysOptions() {
  return queryOptions({
    queryKey: passkeyKeys.mine(),
    queryFn: () => api.listMyPasskeys(),
  });
}

/** Register another passkey; `createCredential` runs the browser ceremony. */
export function useAddPasskey(createCredential: PasskeyCeremonyRunner) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (name: string) => {
      const ceremony = await api.beginAddPasskey();
      const credential = await createCredential(ceremony.options.publicKey);
      return api.finishAddPasskey(ceremony.challenge_id, credential, name);
    },
    onSettled: () => qc.invalidateQueries({ queryKey: passkeyKeys.mine() }),
  });
}

export function useDeletePasskey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (passkeyId: string) => api.deleteMyPasskey(passkeyId),
    onSettled: () => qc.invalidateQueries({ queryKey: passkeyKeys.mine() }),
  });
}
