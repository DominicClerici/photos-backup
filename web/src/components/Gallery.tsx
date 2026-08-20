"use client";

import { useTimeline } from "@/hooks/useTimeline";
import { useView } from "@/hooks/useView";
import { useTrashActions } from "@/hooks/useTrash";
import { useViewer } from "@/hooks/useViewer";
import { Timeline } from "./Timeline";
import { Viewer } from "./Viewer";
import { JobStatus } from "./JobStatus";

export function Gallery() {
  // The order and the filters somebody chose in the floating pill, which is
  // mounted by the root layout and cannot reach this timeline itself. Changing
  // one reloads the day table, exactly as opening a different collection does.
  const { view } = useView();
  const timeline = useTimeline(undefined, view);
  const { index, open, close, navigate } = useViewer(timeline);
  // No filter: a selection here is positions in the whole archive. `retry` is
  // what a delete reloads with — it refetches the day table, and every index
  // after a deleted photograph has moved.
  const actions = useTrashActions(undefined, timeline.retry, undefined, view);

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex h-13 flex-none items-center gap-3 border-b bg-card px-4">
        <h1 className="text-[15px] font-semibold tracking-[0.01em]">Photos</h1>
        <span className="text-[13px] text-faint">
          {timeline.total.toLocaleString()} items
        </span>
        <JobStatus />
      </header>

      <Timeline timeline={timeline} actions={actions} onOpen={open} />

      {index >= 0 ? (
        <Viewer
          at={timeline.at}
          total={timeline.total}
          index={index}
          onClose={close}
          onNavigate={navigate}
        />
      ) : null}
    </div>
  );
}
