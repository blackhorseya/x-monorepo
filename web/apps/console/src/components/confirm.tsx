import { useState, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogTitle,
  DialogFooter,
  DialogHeader,
} from "@/components/ui/dialog";
import { AlertDialog as AlertDialogPrimitive } from "radix-ui";

// A button that asks before it acts: for the operations that are hard to take back (disable an
// account, revoke a key). onConfirm runs on the action button and the dialog closes once it
// settles; its error is the caller's to show (the mutation's own ErrorAlert).
export function ConfirmButton({
  label,
  title,
  description,
  action,
  onConfirm,
  disabled,
}: {
  label: string;
  title: string;
  description: ReactNode;
  action: string;
  onConfirm: () => Promise<unknown>;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  return (
    <AlertDialog open={open} onOpenChange={setOpen}>
      <AlertDialogPrimitive.Trigger asChild>
        <Button variant="outline" size="sm" disabled={disabled}>
          {label}
        </Button>
      </AlertDialogPrimitive.Trigger>
      <AlertDialogContent>
        <DialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </DialogHeader>
        <DialogFooter>
          <AlertDialogPrimitive.Cancel asChild>
            <Button variant="outline">Cancel</Button>
          </AlertDialogPrimitive.Cancel>
          <Button
            variant="destructive"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              // The mutation reports its own failure; either way the question is answered.
              void onConfirm()
                .catch(() => undefined)
                .finally(() => {
                  setBusy(false);
                  setOpen(false);
                });
            }}
          >
            {action}
          </Button>
        </DialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
