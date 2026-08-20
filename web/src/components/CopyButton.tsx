"use client";

import { useEffect, useRef, useState } from "react";
import { Check, Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { toast } from "@/components/ui/toast";
import { cn } from "@/lib/utils";

/**
 * Puts a report on the clipboard, and says that it did.
 *
 * The tick is not decoration. A copy leaves no trace anywhere on screen — the
 * page is identical before and after — so without it the only way to find out
 * whether the click landed is to paste somewhere and look, which is exactly the
 * moment somebody is trying to leave the page.
 *
 * `text` is a function rather than a string so that a page polling every ten
 * seconds copies what is on screen at the click rather than at the render.
 */
export function CopyButton({
  text,
  label = "Copy",
  copied = "Copied",
  variant = "ghost",
  size = "sm",
  className,
  iconOnly = false,
}: {
  text: () => string;
  label?: string;
  copied?: string;
  variant?: "ghost" | "outline" | "secondary";
  size?: "xs" | "sm" | "default";
  className?: string;
  iconOnly?: boolean;
}) {
  const [done, setDone] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => () => void (timer.current && clearTimeout(timer.current)), []);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text());
    } catch {
      // Denied permission, or a page served over plain HTTP from somewhere
      // other than localhost. Nothing is broken and nothing was copied, and
      // the difference matters enough to say out loud.
      toast.add({
        type: "error",
        title: "Could not copy",
        description: "This browser refused clipboard access on this page.",
      });
      return;
    }
    setDone(true);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setDone(false), 2000);
  };

  return (
    <Button
      variant={variant}
      size={iconOnly ? (size === "xs" ? "icon-xs" : "icon-sm") : size}
      onClick={copy}
      aria-label={iconOnly ? label : undefined}
      className={cn("text-muted-foreground hover:text-foreground", className)}
    >
      {done ? (
        <Check className="text-primary" data-icon="inline-start" aria-hidden="true" />
      ) : (
        <Copy data-icon="inline-start" aria-hidden="true" />
      )}
      {iconOnly ? null : done ? copied : label}
    </Button>
  );
}
