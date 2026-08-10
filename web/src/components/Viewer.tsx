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
      className={`viewer${panelOpen ? " hasPanel" : ""}`}
      role="dialog"
      aria-modal="true"
      aria-label="Photo viewer"
    >
      <div className="viewerBar">
        <button type="button" onClick={onClose} aria-label="Close viewer">
          <CloseGlyph />
        </button>
        <span className="viewerCount">
          {index + 1} of {items.length}
          {detail ? <em>{detail.filename}</em> : null}
        </span>
        <span className="viewerActions">
          <button
            type="button"
            onClick={() => setPanelOpen((open) => !open)}
            aria-pressed={panelOpen}
            aria-label="Toggle details"
          >
            <InfoGlyph />
          </button>
          <a href={originalUrl(item.id)} download aria-label="Download original">
            <DownloadGlyph />
          </a>
        </span>
      </div>

      <div className="viewerStage" onClick={onClose}>
        {hasPrev ? (
          <button
            type="button"
            className="viewerNav isPrev"
            aria-label="Previous"
            onClick={(e) => {
              e.stopPropagation();
              onNavigate(index - 1);
            }}
          >
            <ChevronGlyph />
          </button>
        ) : null}

        <div className="viewerMedia" onClick={(e) => e.stopPropagation()}>
          {item.kind === "video" ? (
            <VideoStage item={item} />
          ) : (
            <>
              {!loaded ? <span className="viewerSpinner" /> : null}
              {/* The preview renders straight from the blob, so it works even
                  while the thumbnail job is still queued. */}
              <img
                key={item.id}
                className="viewerImage"
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
            className="viewerNav isNext"
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
        className="viewerVideo"
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
    <div className="viewerFallback">
      <p>
        {failed
          ? "This video could not be converted for playback."
          : "Preparing a version this browser can play…"}
      </p>
      <p className="viewerFallbackWhy">
        The original is stored untouched and can always be downloaded.
      </p>
      <a href={originalUrl(item.id)} download>
        Download original
      </a>
    </div>
  );
}

function MetadataPanel({ detail }: { detail: AssetDetail | null }) {
  if (!detail) {
    return (
      <aside className="panel">
        <p className="panelEmpty">Loading details…</p>
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
    <aside className="panel">
      <h3>{detail.filename}</h3>

      <dl>
        <Row label="Taken">
          {capture.text}
          <span className="panelHint">{capture.zone}</span>
        </Row>

        {disagrees && reported ? (
          <Row label="Phone reported">
            {reported.toLocaleString()}
            <span className="panelHint">differs from the file&rsquo;s own time</span>
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
          <code title={detail.sha256}>{detail.sha256.slice(0, 16)}…</code>
        </Row>
      </dl>
    </aside>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="panelRow">
      <dt>{label}</dt>
      <dd>{children}</dd>
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
