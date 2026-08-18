"use client";

import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { ContextMenuItem } from "@/components/ui/context-menu";
import { cn } from "@/lib/utils";

/**
 * How long an armed control waits before going back to sleep.
 *
 * Short enough that a menu left open on a second monitor is not still holding a
 * loaded delete when somebody comes back to it, and long enough to survive
 * moving the pointer across a menu and reading the word "Confirm".
 */
const DISARM_MS = 4000;

/**
 * Two clicks, in one place.
 *
 * The alternative — a click that opens a dialog that has a button in it — is
 * three surfaces and a focus trap to ask a question the button can ask itself.
 * This is the same safety with none of that: the first click changes what the
 * control says and how it looks, and until it does the second click is not the
 * one that deletes anything.
 *
 * The dialog is still the right answer for the keyboard, where the gesture is a
 * single keystroke with nothing to click twice. See SelectionPill.
 */
export function useArmed(fire: () => void): {
  armed: boolean;
  press: () => void;
  disarm: () => void;
} {
  const [armed, setArmed] = useState(false);
  const timer = useRef(0);

  const disarm = useCallback(() => {
    window.clearTimeout(timer.current);
    timer.current = 0;
    setArmed(false);
  }, []);

  useEffect(() => disarm, [disarm]);

  const press = useCallback(() => {
    if (armed) {
      disarm();
      fire();
      return;
    }
    setArmed(true);
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setArmed(false), DISARM_MS);
  }, [armed, disarm, fire]);

  return { armed, press, disarm };
}

interface ArmedProps {
  /** What the control says at rest. */
  label: string;
  /** What it says once it is loaded. */
  confirm?: string;
  icon?: ReactNode;
  onConfirm: () => void;
  /**
   * Set false to put the control back to sleep from outside — when the sheet it
   * lives in closes, or the selection it was about changes.
   */
  open?: boolean;
  disabled?: boolean;
  /**
   * What the control is asking about. Destructive by default, because that is
   * what two clicks are usually buying.
   *
   * The other tone exists for one item: taking a selection out of an album,
   * which is armed because "remove these forty" is a thing to have meant, and
   * is not destructive because nothing is destroyed — the photographs stay in
   * the library, in their other albums, and in the timeline. Painting it the
   * same red as Delete would be a lie in the direction that makes people stop
   * reading the word.
   */
  tone?: "destructive" | "neutral";
  className?: string;
}

/**
 * The armed control as a button, for the sheet above the selection pill.
 */
export function ArmedButton({
  label,
  confirm = "Confirm",
  icon,
  onConfirm,
  open = true,
  disabled,
  tone = "destructive",
  className,
}: ArmedProps) {
  const { armed, press, disarm } = useArmed(onConfirm);

  // A control nobody can see must not still be holding a loaded delete for
  // whenever it comes back.
  useEffect(() => {
    if (!open) disarm();
  }, [open, disarm]);

  return (
    <Button
      type="button"
      size="sm"
      variant={armed ? "default" : "outline"}
      disabled={disabled}
      onClick={press}
      // The label changes under the pointer, so the accessible name has to say
      // what pressing it now would do rather than what the control is for.
      aria-label={armed ? `${confirm}: ${label.toLowerCase()}` : label}
      className={cn(
        "w-full justify-start gap-2 transition-colors",
        tone === "neutral"
          ? armed && "bg-foreground text-background hover:bg-foreground/90"
          : armed
            ? "bg-destructive text-background hover:bg-destructive/90 focus-visible:ring-destructive/40"
            : "text-destructive hover:text-destructive dark:hover:bg-destructive/15",
        className,
      )}
    >
      {icon}
      {armed ? confirm : label}
    </Button>
  );
}

/**
 * The armed control as a context-menu item.
 *
 * The first click has to leave the menu open or there would be nothing to click
 * again, which is what `closeOnClick` is for: false while the item is asking,
 * true on the press that answers.
 */
export function ArmedMenuItem({
  label,
  confirm = "Confirm",
  icon,
  onConfirm,
  open = true,
  disabled,
  tone = "destructive",
  className,
}: ArmedProps) {
  const { armed, press, disarm } = useArmed(onConfirm);

  useEffect(() => {
    if (!open) disarm();
  }, [open, disarm]);

  const loud = tone === "destructive";

  return (
    <ContextMenuItem
      // Not the destructive variant once armed: that one paints the text with
      // --destructive, which is the fill underneath it by then.
      variant={armed || !loud ? "default" : "destructive"}
      disabled={disabled}
      closeOnClick={armed}
      onClick={press}
      className={cn(
        armed &&
          (loud
            ? "bg-destructive text-background focus:bg-destructive focus:text-background"
            : "bg-foreground text-background focus:bg-foreground focus:text-background"),
        className,
      )}
    >
      {icon}
      {armed ? confirm : label}
    </ContextMenuItem>
  );
}
