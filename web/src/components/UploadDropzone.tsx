"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { CloudUpload } from "lucide-react";

import { carriesFiles, filesFrom } from "@/lib/dropped";
import { ACCEPT_ATTRIBUTE } from "@/lib/upload";
import { cn } from "@/lib/utils";

/**
 * Where files come in.
 *
 * Two sizes of the same control. Empty, it is the page — tall, centred, and the
 * only thing to look at. With a batch behind it, it collapses to a strip: still
 * the drop target, still the button, but no longer occupying the space the list
 * of files now needs. One component rather than two because it is one thing,
 * and because a batch that grows one file at a time should not have the target
 * jump around underneath the cursor.
 *
 * It stays live while a batch is uploading, which is deliberate: files dropped
 * then are read and vetted like any others and are waiting, already checked,
 * when the run finishes. A drop target that goes dead for the two minutes
 * somebody is most likely to remember the other four photographs would be
 * tidiness at the expense of the only thing this page does.
 *
 * The drag is watched on the window rather than on the box, for a reason that
 * has nothing to do with convenience: a file dropped on a page that is not
 * expecting it makes the browser navigate to that file, and this page would
 * take a batch of forty with it. Every drop anywhere in the window is caught
 * here, and the ones carrying files are accepted rather than merely blocked.
 */
export function UploadDropzone({
  onFiles,
  compact,
}: {
  onFiles: (files: File[]) => void;
  compact: boolean;
}) {
  const picker = useRef<HTMLInputElement>(null);
  const [over, setOver] = useState(false);

  const take = useCallback(
    (files: File[]) => {
      if (files.length > 0) onFiles(files);
    },
    [onFiles],
  );

  useEffect(() => {
    // dragenter and dragleave both fire as the pointer crosses every element
    // under it, so "is the file still over the window" is a depth count rather
    // than a flag. Without it the highlight strobes across the gaps between
    // rows.
    let depth = 0;

    const enter = (ev: DragEvent) => {
      if (!carriesFiles(ev.dataTransfer)) return;
      depth++;
      setOver(true);
    };
    const move = (ev: DragEvent) => {
      if (!carriesFiles(ev.dataTransfer)) return;
      // Both of these are required for a drop to happen at all: without them
      // the browser keeps its own default, which is to open the file.
      ev.preventDefault();
      if (ev.dataTransfer) ev.dataTransfer.dropEffect = "copy";
    };
    const leave = (ev: DragEvent) => {
      if (!carriesFiles(ev.dataTransfer)) return;
      depth = Math.max(0, depth - 1);
      if (depth === 0) setOver(false);
    };
    const drop = (ev: DragEvent) => {
      depth = 0;
      setOver(false);
      if (!carriesFiles(ev.dataTransfer)) return;
      ev.preventDefault();
      if (!ev.dataTransfer) return;
      void filesFrom(ev.dataTransfer).then(take);
    };

    window.addEventListener("dragenter", enter);
    window.addEventListener("dragover", move);
    window.addEventListener("dragleave", leave);
    window.addEventListener("drop", drop);
    return () => {
      window.removeEventListener("dragenter", enter);
      window.removeEventListener("dragover", move);
      window.removeEventListener("dragleave", leave);
      window.removeEventListener("drop", drop);
    };
  }, [take]);

  return (
    <>
      <input
        ref={picker}
        type="file"
        multiple
        accept={ACCEPT_ATTRIBUTE}
        className="sr-only"
        // Cleared on every selection so that choosing the same file twice in a
        // row still fires a change event.
        onChange={(ev) => {
          take(Array.from(ev.target.files ?? []));
          ev.target.value = "";
        }}
      />

      <button
        type="button"
        onClick={() => picker.current?.click()}
        aria-label="Choose photos and videos to upload"
        className={cn(
          "group relative flex w-full items-center justify-center overflow-hidden rounded-2xl border border-dashed text-center transition-[background-color,border-color,padding] duration-300 ease-out",
          "focus-visible:ring-2 focus-visible:ring-ring/70 focus-visible:outline-none",
          compact ? "gap-3 px-5 py-4" : "flex-col gap-4 px-6 py-14",
          over
            ? "border-primary/70 bg-primary/[0.07]"
            : "border-border bg-card/40 hover:border-foreground/25 hover:bg-card/70",
        )}
      >
        {/* A single sheen that sweeps once when a drag arrives. It is the only
            motion on the page that says "let go now", and it costs one element. */}
        {over ? (
          <span
            aria-hidden="true"
            className="pointer-events-none absolute inset-0 bg-[linear-gradient(110deg,transparent_25%,var(--color-primary)/0.08_50%,transparent_75%)] bg-[length:200%_100%] animate-shimmer"
          />
        ) : null}

        <span
          className={cn(
            "flex shrink-0 items-center justify-center rounded-full border bg-tile transition-transform duration-300 ease-out",
            compact ? "size-9" : "size-14",
            over && "scale-110 border-primary/40",
          )}
        >
          <CloudUpload
            className={cn(
              "transition-colors duration-200",
              compact ? "size-4.5" : "size-6",
              over ? "text-primary" : "text-muted-foreground group-hover:text-foreground",
            )}
            aria-hidden="true"
          />
        </span>

        <span className={cn(compact && "text-left")}>
          <span className="block text-[15px] font-medium tracking-[0.01em]">
            {over ? "Drop to add them" : compact ? "Add more" : "Drop photos and videos here"}
          </span>
          <span className="mt-0.5 block text-[13px] text-muted-foreground">
            or <span className="text-primary">browse this computer</span>
            {compact ? null : " — folders are read all the way down"}
          </span>
        </span>

        {compact ? null : (
          <span className="text-[12px] text-faint">
            HEIC · JPEG · PNG · DNG · GIF · WebP · MOV · MP4 · AVI · WebM — up to 2 GB each
          </span>
        )}
      </button>
    </>
  );
}
