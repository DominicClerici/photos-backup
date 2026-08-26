"use client";

import { useEffect, useState } from "react";
import { FileWarning } from "lucide-react";

import { formatBytes } from "@/lib/format";
import { kindOf, labelFor } from "@/lib/upload";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

/**
 * One file from the batch, at a size somebody can actually judge.
 *
 * The whole point of the eye on a row is answering "which photograph is this?"
 * before it goes into the archive, so this shows the file itself rather than
 * anything derived from it — there is nothing derived from it yet.
 *
 * Which is also its limit, and the limit is stated rather than hidden. A
 * browser can decode a JPEG and an MP4; it cannot decode a HEIC, a DNG or most
 * TIFFs, and those are a large part of what an iPhone produces. Once the file
 * is in the archive photod renders every one of them — so the honest thing here
 * is to say that the preview is missing rather than to imply the file is bad.
 */
export function UploadPreview({ file, onClose }: { file: File | null; onClose: () => void }) {
  const url = useObjectUrl(file);
  const [failed, setFailed] = useState(false);

  useEffect(() => setFailed(false), [file]);

  const kind = file ? kindOf(file.name) : null;

  return (
    <Dialog open={file !== null} onOpenChange={(open) => (open ? undefined : onClose())}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle className="truncate pr-8">{file?.name ?? ""}</DialogTitle>
          <DialogDescription>
            {file
              ? [labelFor(file.name), formatBytes(file.size), whenModified(file)]
                  .filter(Boolean)
                  .join(" · ")
              : ""}
          </DialogDescription>
        </DialogHeader>

        <div className="flex max-h-[70vh] min-h-56 items-center justify-center overflow-hidden rounded-lg bg-viewer">
          {url && kind === "video" && !failed ? (
            <video
              src={url}
              controls
              playsInline
              className="max-h-[70vh] w-auto"
              onError={() => setFailed(true)}
            />
          ) : url && kind === "image" && !failed ? (
            // eslint-disable-next-line @next/next/no-img-element
            <img
              src={url}
              alt={file?.name ?? ""}
              className="max-h-[70vh] w-auto object-contain"
              onError={() => setFailed(true)}
            />
          ) : (
            <div className="flex flex-col items-center gap-2 px-6 py-12 text-center">
              <FileWarning className="size-6 text-faint" aria-hidden="true" />
              <p className="text-[13px] text-muted-foreground">
                This browser cannot open {labelFor(file?.name ?? "") || "this file"} on its own.
              </p>
              <p className="text-[12px] text-faint">
                The archive can. It will have a thumbnail a moment after it is uploaded.
              </p>
            </div>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}

/**
 * A URL for a file that lives only in this tab, released the moment it is no
 * longer on screen.
 *
 * Revoking matters more here than in most places that use this API: a batch can
 * be two hundred files and several gigabytes, and every URL left behind pins
 * the whole of its file in the tab for as long as the page is open.
 */
export function useObjectUrl(file: File | null): string | null {
  const [url, setUrl] = useState<string | null>(null);

  useEffect(() => {
    if (!file) {
      setUrl(null);
      return;
    }
    const made = URL.createObjectURL(file);
    setUrl(made);
    return () => {
      URL.revokeObjectURL(made);
      setUrl(null);
    };
  }, [file]);

  return url;
}

function whenModified(file: File): string {
  if (!file.lastModified) return "";
  return new Date(file.lastModified).toLocaleDateString(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}
