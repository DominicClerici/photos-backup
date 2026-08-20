"use client";

import { useState } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  ChevronDown,
  FileWarning,
  Image as ImageIcon,
  Video,
  XCircle,
} from "lucide-react";

import { thumbUrl, type Failure, type Problem, type Status } from "@/lib/api";
import { formatSince } from "@/lib/format";
import { reportOneFailure, reportOneProblem, reportStatus } from "@/lib/report";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { CopyButton } from "./CopyButton";

/**
 * Everything that is wrong, and the means to hand it to somebody who can fix
 * it.
 *
 * The two kinds of wrong are kept apart on purpose. A problem is true of the
 * server — ffmpeg is not installed, the workers are off — and one of them
 * explains every failure below it, so it is drawn first and drawn larger. A
 * failure is one job that gave up on one photograph, and forty of those are
 * usually one problem nobody has noticed yet.
 */
/**
 * How many failures are drawn before the list asks permission for the rest.
 *
 * A page that opens with sixty near-identical rows on it has buried the three
 * cards above them. Eight is enough to see whether the failures are all the
 * same thing, which is the question a long list is being read for.
 */
const FIRST_FEW = 8;

export function StatusIssues({ status }: { status: Status }) {
  const { problems, failures } = status;
  const total = problems.length + failures.length;
  const [all, setAll] = useState(false);
  const shown = all ? failures : failures.slice(0, FIRST_FEW);
  // Red is for the things that are actually broken. A server that is merely
  // missing a tool it has not needed yet should not put a red number on the
  // page, or the red stops meaning anything.
  const grave = failures.length > 0 || problems.some((p) => p.severity === "error");

  if (total === 0) {
    return (
      <Card className="items-center gap-2 py-10 text-center">
        <CheckCircle2 className="size-5 text-primary" aria-hidden="true" />
        <p className="text-sm font-medium">Nothing to report</p>
        <p className="max-w-sm text-[13px] text-muted-foreground">
          Every job the archive has been asked to do has finished, and the server has all the
          tools it needs.
        </p>
      </Card>
    );
  }

  return (
    <section aria-labelledby="issues-heading" className="flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <h2 id="issues-heading" className="text-[13px] font-medium tracking-[0.01em]">
          Needs attention
        </h2>
        <Badge
          variant={grave ? "destructive" : "outline"}
          className={cn("tabular-nums", !grave && "border-warning/40 text-warning")}
        >
          {total.toLocaleString()}
        </Badge>
        <CopyButton
          className="ml-auto"
          variant="outline"
          text={() => reportStatus(status, new Date())}
          label="Copy report"
          copied="Report copied"
        />
      </div>

      {problems.map((problem) => (
        <ProblemRow key={problem.id} problem={problem} />
      ))}

      {failures.length > 0 ? (
        <>
          <p className="mt-1 text-[13px] text-faint">
            {failures.length === 1
              ? "One job gave up after retrying."
              : `${failures.length.toLocaleString()} jobs gave up after retrying.`}{" "}
            The originals are safe; what is missing is what the job would have made.
          </p>
          <ul className="flex flex-col gap-2">
            {shown.map((failure) => (
              <li key={failure.id}>
                <FailureRow failure={failure} />
              </li>
            ))}
          </ul>
          {failures.length > shown.length ? (
            <Button variant="outline" className="self-center" onClick={() => setAll(true)}>
              Show the other {(failures.length - shown.length).toLocaleString()}
            </Button>
          ) : null}
        </>
      ) : null}
    </section>
  );
}

function ProblemRow({ problem }: { problem: Problem }) {
  const bad = problem.severity === "error";
  const Icon = bad ? XCircle : AlertTriangle;

  return (
    <Card
      className={cn(
        "flex-row items-start gap-3 px-4 ring-1",
        bad ? "ring-destructive/30" : "ring-warning/25",
      )}
    >
      <Icon
        className={cn("mt-0.5 size-4 shrink-0", bad ? "text-destructive" : "text-warning")}
        aria-hidden="true"
      />
      <div className="flex min-w-0 flex-1 flex-col gap-1">
        <p className="text-sm font-medium">{problem.title}</p>
        <p className="text-[13px] text-muted-foreground">{problem.detail}</p>
      </div>
      <CopyButton
        iconOnly
        size="xs"
        text={() => reportOneProblem(problem)}
        label={`Copy the details of "${problem.title}"`}
      />
    </Card>
  );
}

/**
 * One failed job.
 *
 * The thumbnail earns its place: a filename says which file and a picture says
 * which photograph, and when the failure is "this HEIC will not decode" the
 * second one is the question being asked. It is also the one thing here that
 * might not exist — a metadata job that failed never made a thumbnail — so the
 * fallback is part of the design rather than an error state.
 */
function FailureRow({ failure }: { failure: Failure }) {
  const [open, setOpen] = useState(false);
  const [drawn, setDrawn] = useState(failure.viewable);

  return (
    <Card size="sm" className="gap-2 px-3">
      <div className="flex items-start gap-3">
        <span className="flex size-10 shrink-0 items-center justify-center overflow-hidden rounded-md bg-tile">
          {drawn ? (
            // eslint-disable-next-line @next/next/no-img-element -- one 96px
            // thumbnail per failed job, straight from photod; next/image would
            // add a resizing hop for a picture that is already exactly the size
            // it is drawn at.
            <img
              src={thumbUrl(failure.asset_id, 96)}
              alt=""
              className="size-full object-cover"
              onError={() => setDrawn(false)}
            />
          ) : (
            <Fallback kind={failure.media_kind} />
          )}
        </span>

        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="truncate text-[13px] font-medium">
              {failure.filename || "Not in the library any more"}
            </span>
            <Badge variant="outline" className="font-normal text-muted-foreground">
              {failure.kind}
            </Badge>
          </div>
          <p className="text-[12px] text-faint">
            <time dateTime={failure.failed_at} title={failure.failed_at}>
              {formatSince(failure.failed_at)}
            </time>
            {" · "}
            {failure.attempts} {failure.attempts === 1 ? "attempt" : "attempts"}
            {" · "}
            <span className="font-mono">{failure.asset_id.slice(0, 8)}</span>
          </p>
        </div>

        <CopyButton
          iconOnly
          size="xs"
          text={() => reportOneFailure(failure)}
          label={`Copy the details of this ${failure.kind} failure`}
        />
      </div>

      <div className="flex items-start gap-1.5">
        <pre
          className={cn(
            "min-w-0 flex-1 rounded-md bg-tile/60 px-2.5 py-1.5 font-mono text-[11.5px] leading-relaxed text-muted-foreground",
            open ? "max-h-64 overflow-auto whitespace-pre-wrap" : "truncate",
          )}
        >
          {failure.error || "The job recorded no error text."}
        </pre>
        {failure.error.length > 80 ? (
          <Button
            variant="ghost"
            size="icon-xs"
            onClick={() => setOpen((v) => !v)}
            aria-expanded={open}
            aria-label={open ? "Collapse the error" : "Show the whole error"}
          >
            <ChevronDown
              className={cn("transition-transform", open && "rotate-180")}
              aria-hidden="true"
            />
          </Button>
        ) : null}
      </div>
    </Card>
  );
}

function Fallback({ kind }: { kind?: "image" | "video" }) {
  const Icon = kind === "video" ? Video : kind === "image" ? ImageIcon : FileWarning;
  return <Icon className="size-4 text-faint" aria-hidden="true" />;
}
