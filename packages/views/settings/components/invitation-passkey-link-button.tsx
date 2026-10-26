"use client";

import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { Link2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { api } from "@multica/core/api";
import type { Invitation } from "@multica/core/types";
import { useT } from "../../i18n";

/**
 * Passkey-only deployments have no invitation email (ACCO-55). An admin
 * copies a one-time sign-up link here and hands it to the invitee, who
 * registers a passkey with it and then accepts the invitation.
 */
export function InvitationPasskeyLinkButton({ invitation }: { invitation: Invitation }) {
  const { t } = useT("settings");
  const createLink = useMutation({
    mutationFn: () => api.createInvitationPasskeyLink(invitation.workspace_id, invitation.id),
  });

  const handleCopy = () => {
    createLink.mutate(undefined, {
      onSuccess: ({ url }) =>
        navigator.clipboard.writeText(url).then(
          () => toast.success(t(($) => $.members.toast_passkey_link_copied)),
          () => toast.error(t(($) => $.members.toast_passkey_link_failed)),
        ),
      onError: (err) =>
        toast.error(err instanceof Error ? err.message : t(($) => $.members.toast_passkey_link_failed)),
    });
  };

  return (
    <Button
      variant="ghost"
      size="icon-sm"
      disabled={createLink.isPending}
      onClick={handleCopy}
      aria-label={t(($) => $.members.copy_passkey_link_aria, { email: invitation.invitee_email })}
      title={t(($) => $.members.copy_passkey_link_aria, { email: invitation.invitee_email })}
    >
      <Link2 className="size-4 text-muted-foreground" />
    </Button>
  );
}
