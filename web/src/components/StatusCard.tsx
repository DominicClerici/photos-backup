"use client";

import Link from "next/link";
import type { ComponentType, ReactNode } from "react";
import { ChevronRight } from "lucide-react";

import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";

/**
 * One tile in the row along the top of the status page.
 *
 * A shell rather than four near-identical cards, because the row only reads as
 * a row if the eye can find the same three things in the same three places
 * across it: what this is, the one number that answers it, and the smaller
 * sentence that qualifies the number.
 *
 * `href` turns the whole card into a link, which is why the chevron appears
 * with it and not otherwise — a card that goes nowhere should not look like one
 * that does.
 */
export function StatusCard({
  Icon,
  title,
  href,
  action,
  children,
  className,
}: {
  Icon: ComponentType<{ className?: string; "aria-hidden"?: boolean }>;
  title: string;
  href?: string;
  /** Drawn at the right of the header row — a badge, usually. */
  action?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  const body = (
    <>
      <div className="flex items-center gap-2.5">
        <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-tile">
          <Icon className="size-4 text-muted-foreground" aria-hidden={true} />
        </span>
        <span className="flex-1 truncate text-[13px] font-medium tracking-[0.01em]">{title}</span>
        {action}
        {href ? (
          <ChevronRight
            className="size-4 shrink-0 text-faint transition-transform group-hover/card:translate-x-0.5"
            aria-hidden="true"
          />
        ) : null}
      </div>
      {children}
    </>
  );

  const shell = cn(
    "gap-3 px-4",
    href && "relative transition-colors hover:bg-foreground/[0.04]",
    className,
  );

  if (!href) {
    return <Card className={shell}>{body}</Card>;
  }
  return (
    <Card
      className={cn(
        shell,
        "focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-ring",
      )}
    >
      {/* The link covers the card rather than wrapping it: a card is a grid of
          blocks and an <a> around them would make one long line of text of the
          whole thing for a screen reader. */}
      <Link href={href} className="absolute inset-0 z-10 focus-visible:outline-none">
        <span className="sr-only">{title}</span>
      </Link>
      {body}
    </Card>
  );
}

/**
 * The number the card is about, and the line under it that says what it means.
 *
 * Tabular figures throughout: these are numbers that change under a poll, and
 * proportional digits make a count that ticks from 1,109 to 1,110 shuffle
 * everything beside it.
 */
export function StatusFigure({
  value,
  unit,
  note,
}: {
  value: ReactNode;
  unit?: string;
  note?: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-baseline gap-1.5">
        <span className="text-[26px] leading-none font-semibold tabular-nums">{value}</span>
        {unit ? <span className="text-[13px] text-faint">{unit}</span> : null}
      </div>
      {note ? <p className="text-[13px] text-muted-foreground">{note}</p> : null}
    </div>
  );
}
