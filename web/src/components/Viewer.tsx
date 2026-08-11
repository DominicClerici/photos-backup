"use client";

import { useEffect, useMemo, useState } from "react";

import {
  fetchAsset,
  originalUrl,
  playbackUrl,
  previewUrl,
  thumbUrl,
  type AssetDetail,
  type TimelineItem,
} from "@/lib/api";
import {
  formatBytes,
  formatCaptureTime,
  formatCoords,
  formatDuration,
  mapLink,
} from "@/lib/format";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

/** The bar's controls are 34px, between shadcn's icon (32px) and icon-lg (36px). */
const BAR_BUTTON = "size-[34px] text-muted-foreground";

const NAV_BUTTON =
  "absolute top-1/2 grid size-[46px] -translate-y-1/2 place-items-center rounded-full bg-card/70 text-muted-foreground hover:bg-card hover:text-foreground max-[700px]:hidden";

const MEDIA =
  "max-h-full max-w-full object-contain transition-opacity duration-[120ms] ease-out";

/** Sidebar on a wide screen, bottom sheet under 700px. */
const PANEL =
  "absolute top-13 right-0 bottom-0 w-80 overflow-y-auto border-l bg-card px-5 pt-[18px] pb-7 max-[700px]:top-auto max-[700px]:left-0 max-[700px]:max-h-[55%] max-[700px]:w-full max-[700px]:border-t max-[700px]:border-l-0";

const PANEL_HINT = "mt-[2px] block text-[11px] text-faint";

interface Props {
  items: TimelineItem[];
  index: number;
  onClose: () => void;
  onNavigate: (index: number) => void;
}

export function Viewer({ items, index, onClose, onNavigate }: Props) {
  const item = items[index];
  const [detail, setDetail] = useState<AssetDetail | null>(null);
  const [panelOpen, setPanelOpen] = useState(false);
  const [loaded, setLoaded] = useState(false);

  const hasPrev = index > 0;
  const hasNext = index < items.length - 1;

  useEffect(() => {
    if (!item) return;
    setDetail(null);
    setLoaded(false);
    const controller = new AbortController();
    fetchAsset(item.id, controller.signal)
      .then(setDetail)
      .catch(() => {
        // The picture still shows; only the metadata panel goes empty.
      });
    return () => controller.abort();
  }, [item?.id]);

  // Preloading the neighbours is what makes arrow-keying feel instant. The
  // preview is rendered per request, so this also gets the conversion started
  // before it is needed rather than after.
  const neighbours = useMemo(
    () =>
      [items[index - 1], items[index + 1]]
        .filter((it): it is TimelineItem => it?.kind === "image")
        .map((it) => it.id),
    [items, index],
  );
  const neighbourKey = neighbours.join(",");
  useEffect(() => {
    for (const id of neighbourKey ? neighbourKey.split(",") : []) {
      const img = new Image();
      img.src = previewUrl(id);
    }
  }, [neighbourKey]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      switch (e.key) {
        case "Escape":
          onClose();
          break;
        case "ArrowLeft":
          if (hasPrev) onNavigate(index - 1);
          break;
        case "ArrowRight":
          if (hasNext) onNavigate(index + 1);
          break;
        case "i":
        case "I":
          setPanelOpen((open) => !open);
          break;
        default:
          return;
      }
      e.preventDefault();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [index, hasPrev, hasNext, onClose, onNavigate]);

  // The page behind must not scroll while the overlay owns the screen.
  useEffect(() => {
    const previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previous;
    };
  }, []);

  if (!item) return null;

  return (
    <div
      className="fixed inset-0 z-50 flex flex-col bg-viewer"
      role="dialog"
      aria-modal="true"
      aria-label="Photo viewer"
    >
      <div className="flex h-13 flex-none items-center gap-3.5 px-3.5 text-muted-foreground">
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className={BAR_BUTTON}
          onClick={onClose}
          aria-label="Close viewer"
        >
          <CloseGlyph />
        </Button>
        <span className="flex min-w-0 items-baseline gap-3 text-[13px] tabular-nums">
          {index + 1} of {items.length}
          {detail ? (
            <em className="truncate not-italic text-faint">{detail.filename}</em>
          ) : null}
        </span>
        <span className="ml-auto flex gap-1">
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className={cn(
              BAR_BUTTON,
              "aria-pressed:bg-muted aria-pressed:text-foreground",
            )}
            onClick={() => setPanelOpen((open) => !open)}
            aria-pressed={panelOpen}
            aria-label="Toggle details"
          >
            <InfoGlyph />
          </Button>
          <Button
            variant="ghost"
            size="icon"
            className={BAR_BUTTON}
            aria-label="Download original"
            // The download is a real link, so it must stay an <a>. Base UI warns
            // unless it is told the rendered element is not a native button —
            // same as shadcn's own PaginationLink.
            nativeButton={false}
            render={<a href={originalUrl(item.id)} download />}
          >
            <DownloadGlyph />
          </Button>
        </span>
      </div>

      <div
        // Stretch, not center: the media wrapper needs a definite height for the
        // image's own max-height to resolve against. Centering it here would leave
        // the wrapper auto-height, percentages would resolve to nothing, and a tall
        // photo would render at full size and overflow the screen.
        className={cn(
          "relative flex min-h-0 flex-1 items-stretch justify-center px-3 pb-4 transition-[margin] duration-[140ms] ease-out",
          // The panel sits over the photo, so give the photo back the space it takes.
          panelOpen && "mr-80 max-[700px]:mr-0 max-[700px]:mb-[55%]",
        )}
        onClick={onClose}
      >
        {hasPrev ? (
          <button
            type="button"
            className={cn(NAV_BUTTON, "left-3.5")}
            aria-label="Previous"
            onClick={(e) => {
              e.stopPropagation();
              onNavigate(index - 1);
            }}
          >
            <ChevronGlyph />
          </button>
        ) : null}

        <div
          className="relative flex min-h-0 min-w-0 flex-1 items-center justify-center"
          onClick={(e) => e.stopPropagation()}
        >
          {item.kind === "video" ? (
            <VideoStage item={item} />
          ) : (
            <>
              {!loaded ? (
                <span className="absolute size-[26px] animate-spin rounded-full border-2 border-border border-t-muted-foreground [animation-duration:700ms]" />
              ) : null}
              {/* The preview renders straight from the blob, so it works even
                  while the thumbnail job is still queued. */}
              <img
                key={item.id}
                className={MEDIA}
                src={previewUrl(item.id)}
                alt={detail?.filename ?? ""}
                onLoad={() => setLoaded(true)}
                style={{ opacity: loaded ? 1 : 0 }}
              />
            </>
          )}
        </div>

        {hasNext ? (
          <button
            type="button"
            className={cn(NAV_BUTTON, "right-3.5 rotate-180")}
            aria-label="Next"
            onClick={(e) => {
              e.stopPropagation();
              onNavigate(index + 1);
            }}
          >
            <ChevronGlyph />
          </button>
        ) : null}
      </div>

      {panelOpen ? <MetadataPanel detail={detail} /> : null}
    </div>
  );
}

function VideoStage({ item }: { item: TimelineItem }) {
  if (item.playback_state === "ready") {
    return (
      <video
        key={item.id}
        className={MEDIA}
        src={playbackUrl(item.id)}
        poster={item.state === "ready" ? thumbUrl(item.id) : undefined}
        controls
        autoPlay
        playsInline
      />
    );
  }

  const failed = item.playback_state === "failed";
  return (
    <div className="max-w-[420px] text-center text-sm leading-normal text-muted-foreground">
      <p>
        {failed
          ? "This video could not be converted for playback."
          : "Preparing a version this browser can play…"}
      </p>
      <p className="text-[13px] text-faint">
        The original is stored untouched and can always be downloaded.
      </p>
      <a className="text-primary" href={originalUrl(item.id)} download>
        Download original
      </a>
    </div>
  );
}

function MetadataPanel({ detail }: { detail: AssetDetail | null }) {
  if (!detail) {
    return (
      <aside className={PANEL}>
        <p className="text-[13px] text-faint">Loading details…</p>
      </aside>
    );
  }

  const capture = formatCaptureTime(detail.taken_at, detail.offset_minutes);
  const camera = [detail.camera_make, detail.camera_model].filter(Boolean).join(" ");
  const reported = detail.reported_at ? new Date(detail.reported_at) : null;
  const taken = new Date(detail.taken_at);
  // Worth showing only when the two disagree: if the phone and the file tell the
  // same story, repeating it is noise.
  const disagrees =
    reported != null && Math.abs(reported.getTime() - taken.getTime()) > 60_000;

  return (
    <aside className={PANEL}>
      <h3 className="mb-4 text-sm font-semibold [overflow-wrap:anywhere]">
        {detail.filename}
      </h3>

      <dl className="flex flex-col gap-3.5">
        <Row label="Taken">
          {capture.text}
          <span className={PANEL_HINT}>{capture.zone}</span>
        </Row>

        {disagrees && reported ? (
          <Row label="Phone reported">
            {reported.toLocaleString()}
            <span className={PANEL_HINT}>differs from the file&rsquo;s own time</span>
          </Row>
        ) : null}

        {detail.width && detail.height ? (
          <Row label="Dimensions">
            {detail.width} × {detail.height}
          </Row>
        ) : null}

        {detail.duration ? (
          <Row label="Duration">{formatDuration(detail.duration)}</Row>
        ) : null}

        {camera ? <Row label="Camera">{camera}</Row> : null}
        {detail.lens ? <Row label="Lens">{detail.lens}</Row> : null}

        {detail.gps_lat != null && detail.gps_lon != null ? (
          <Row label="Location">
            <a
              className="text-primary"
              href={mapLink(detail.gps_lat, detail.gps_lon)}
              target="_blank"
              rel="noreferrer"
            >
              {formatCoords(detail.gps_lat, detail.gps_lon)}
            </a>
          </Row>
        ) : null}

        <Row label="Size">{formatBytes(detail.byte_size)}</Row>
        <Row label="Uploaded">{new Date(detail.uploaded_at).toLocaleString()}</Row>
        <Row label="SHA-256">
          <code className="font-mono text-xs text-muted-foreground" title={detail.sha256}>
            {detail.sha256.slice(0, 16)}…
          </code>
        </Row>
      </dl>
    </aside>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="mb-[3px] text-[11px] tracking-[0.06em] text-faint uppercase">
        {label}
      </dt>
      <dd className="text-[13px] leading-[1.4] [overflow-wrap:anywhere]">{children}</dd>
    </div>
  );
}

function CloseGlyph() {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true">
      <path
        d="M6 6l12 12M18 6L6 18"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
      />
    </svg>
  );
}

function InfoGlyph() {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true">
      <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <path
        d="M12 11v5.5M12 7.6v.6"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
      />
    </svg>
  );
}

function DownloadGlyph() {
  return (
    <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true">
      <path
        d="M12 4v11m0 0 4-4m-4 4-4-4M5 19h14"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.7"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function ChevronGlyph() {
  return (
    <svg viewBox="0 0 24 24" width="26" height="26" aria-hidden="true">
      <path
        d="M15 5l-7 7 7 7"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}
