"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { Trash2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { myPasskeysOptions, useAddPasskey, useDeletePasskey } from "@multica/core/auth";
import type { Passkey } from "@multica/core/api";
import { passkeyFailure, registerCredential } from "../../auth/passkey-ceremony";
import { useLocale, useT } from "../../i18n";
import { SettingsCard, SettingsRow, SettingsSection } from "./settings-layout";

/**
 * The signed-in person's passkeys on a passkey-only deployment (ACCO-55):
 * add a backup, remove a lost one. The server refuses to remove the last
 * passkey, and the UI says so up front.
 */
export function PasskeysSection() {
  const { t } = useT("settings");
  const locale = useLocale();
  const { data: passkeys = [] } = useQuery(myPasskeysOptions());
  const addPasskey = useAddPasskey(registerCredential);
  const deletePasskey = useDeletePasskey();
  const [removing, setRemoving] = useState<Passkey | null>(null);

  const failureMessage = (err: unknown) => {
    switch (passkeyFailure(err)) {
      case "reauth_required":
        return t(($) => $.passkeys.reauth_required);
      case "cancelled":
        return null; // closing the prompt is a choice, not an error
      default:
        return err instanceof Error ? err.message : t(($) => $.passkeys.toast_failed);
    }
  };

  const handleAdd = () => {
    addPasskey.mutate(`Passkey ${passkeys.length + 1}`, {
      onSuccess: () => toast.success(t(($) => $.passkeys.toast_added)),
      onError: (err) => {
        const message = failureMessage(err);
        if (message) toast.error(message);
      },
    });
  };

  const handleRemove = (passkey: Passkey) => {
    deletePasskey.mutate(passkey.id, {
      onSuccess: () => toast.success(t(($) => $.passkeys.toast_removed)),
      onError: (err) => toast.error(failureMessage(err) ?? t(($) => $.passkeys.toast_failed)),
      onSettled: () => setRemoving(null),
    });
  };

  const formatDate = (iso: string) => new Date(iso).toLocaleDateString(locale);
  const onlyOne = passkeys.length <= 1;

  return (
    <SettingsSection
      title={t(($) => $.passkeys.title)}
      description={t(($) => $.passkeys.description)}
      action={
        <Button size="sm" variant="outline" onClick={handleAdd} disabled={addPasskey.isPending}>
          {addPasskey.isPending ? t(($) => $.passkeys.adding) : t(($) => $.passkeys.add)}
        </Button>
      }
    >
      <SettingsCard>
        {passkeys.map((passkey) => (
          <SettingsRow
            key={passkey.id}
            label={passkey.name}
            description={[
              t(($) => $.passkeys.created, { date: formatDate(passkey.created_at) }),
              passkey.last_used_at
                ? t(($) => $.passkeys.last_used, { date: formatDate(passkey.last_used_at) })
                : t(($) => $.passkeys.never_used),
            ].join(" · ")}
          >
            <Button
              size="icon-sm"
              variant="ghost"
              disabled={onlyOne || deletePasskey.isPending}
              title={onlyOne ? t(($) => $.passkeys.only_one_hint) : undefined}
              aria-label={t(($) => $.passkeys.remove_aria, { name: passkey.name })}
              onClick={() => setRemoving(passkey)}
            >
              <Trash2 />
            </Button>
          </SettingsRow>
        ))}
      </SettingsCard>

      <AlertDialog open={removing !== null} onOpenChange={(open) => { if (!open) setRemoving(null); }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.passkeys.remove_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.passkeys.remove_description, { name: removing?.name ?? "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.members.confirm_cancel)}</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={deletePasskey.isPending}
              onClick={(event) => {
                event.preventDefault();
                if (removing) handleRemove(removing);
              }}
            >
              {t(($) => $.members.remove_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </SettingsSection>
  );
}
