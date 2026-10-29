import { cloneElement, useId, type ReactElement } from "react";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";

// Field labels one input: the id ties <label for> to it, so clicking the label
// focuses the input and screen readers (and getByLabel) can find it.
export function Field({
  label,
  className,
  children,
}: {
  label: string;
  className?: string;
  children: ReactElement<{ id?: string }>;
}) {
  const id = useId();
  return (
    <div className={cn("grid gap-1.5", className)}>
      <Label htmlFor={id}>{label}</Label>
      {cloneElement(children, { id })}
    </div>
  );
}
