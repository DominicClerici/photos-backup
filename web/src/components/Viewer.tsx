"use client";

import { useEffect, useRef, useState } from "react";

import {
  fetchAsset,
  livePreviewUrl,
  originalUrl,
  plainPlaybackUrl,
  plainPreviewUrl,
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
import { useLiveFade } from "@/hooks/useLiveFade";
import { cn } from "@/lib/utils";

/** The bar's controls are 34px, between shadcn's icon (32px) and icon-lg (36px). */
const BAR_BUTTON = "size-[34px] text-muted-foreground";

const NAV_BUTTON =
  "absolute top-1/2 grid size-[46px] -translate-y-1/2 place-items-center rounded-full bg-card/70 text-muted-foreground hover:bg-card hover:text-foreground max-[700px]:hidden";

const MEDIA =
  "max-h-full max-w-full object-contain transition-opacity duration-[120ms] ease-out";

/**
 * How long a press has to be held before a Live Photo starts.
 *
 * Short enough to feel like a direct response, long enough that a click meant
 * to close the viewer or step to the next photo never trips it.
 */
const HOLD_MS = 150;

/**
 * The badge over a photo that has something to reveal. Same place and same
 * shape as the Live Photo's, because it is the same gesture and there is no
 * reason for someone to learn it twice.
 */
const HINT_BADGE =
  "pointer-events-none absolute top-3 left-3 flex items-center gap-1.5 rounded-full bg-card/70 px-2.5 py-1 text-[11px] font-medium tracking-[0.06em] text-muted-foreground uppercase backdrop-blur-[6px]";

/** Sidebar on a wide screen, bottom sheet under 700px. */
const PANEL =
  "absolute top-13 right-0 bottom-0 w-80 overflow-y-auto border-l bg-card px-5 pt-[18px] pb-7 max-[700px]:top-auto max-[700px]:left-0 max-[700px]:max-h-[55%] max-[700px]:w-full max-[700px]:border-t max-[700px]:border-l-0";

const PANEL_HINT = "mt-[2px] block text-[11px] text-faint";

interface Props {
  /**
   * The item at a position in the timeline, or undefined while it is still
   * being fetched. An accessor rather than a list because the timeline is
   * addressed by position now and holds only the stretch of it that has been
   * asked for — the photo two along may genuinely not be here yet.
   */
  at: (index: number) => TimelineItem | undefined;
  /** How many items the collection holds, loaded or not. */
  total: number;
  index: number;
  onClose: () => void;
  onNavigate: (index: number) => void;
}

export function Viewer({ at, total, index, onClose, onNavigate }: Props) {
  const item = at(index);
  const [detail, setDetail] = useState<AssetDetail | null>(null);
  const [panelOpen, setPanelOpen] = useState(false);
  const [loaded, setLoaded] = useState(false);
  // Kept across navigation rather than reset per photo: Snapchat memories
  // arrive in runs, and someone who turned the captions off to look at one
  // photograph almost always wants the next one the same way.
  const [overlayOn, setOverlayOn] = useState(true);
  const hasOverlay = detail?.has_overlay ?? false;

  const hasPrev = index > 0;
  const hasNext = index < total - 1;

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
  //
  // Computed on every render rather than memoised: `at` reads a sparse store,
  // and a neighbour that was not loaded when the viewer opened is exactly the
  // one worth reacting to when it arrives.
  const neighbourKey = [at(index - 1), at(index + 1)]
    .filter((it): it is TimelineItem => it?.kind === "image")
    .map((it) => it.id)
    .join(",");
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
        case "o":
        case "O":
          setOverlayOn((on) => !on);
          break;
        default:
          return;
      }
      e.preventDefault();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [index, hasPrev, hasNext, onClose, onNavigate]);

  // The page behind must not scroll while the overlay owns the screen, and the
  // tab bar it covers must not still be reachable by Tab — see globals.css.
  useEffect(() => {
    const previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    document.body.dataset.overlay = "open";
    return () => {
      document.body.style.overflow = previous;
      delete document.body.dataset.overlay;
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
          {index + 1} of {total.toLocaleString()}
          {detail ? (
            <em className="truncate not-italic text-faint">{detail.filename}</em>
          ) : null}
        </span>
        <span className="ml-auto flex gap-1">
          {hasOverlay ? (
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className={cn(
                BAR_BUTTON,
                "aria-pressed:bg-muted aria-pressed:text-foreground",
              )}
              onClick={() => setOverlayOn((on) => !on)}
              aria-pressed={overlayOn}
              aria-label={
                overlayOn ? "Hide the overlay (O)" : "Show the overlay (O)"
              }
            >
              <OverlayGlyph off={!overlayOn} />
            </Button>
          ) : null}
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
            <VideoStage item={item} plain={hasOverlay && !overlayOn} />
          ) : (
            <>
              {!loaded ? (
                <span className="absolute size-[26px] animate-spin rounded-full border-2 border-border border-t-muted-foreground [animation-duration:700ms]" />
              ) : null}
              <PhotoStage
                key={item.id}
                item={item}
                alt={detail?.filename ?? ""}
                loaded={loaded}
                hasOverlay={hasOverlay}
                overlayOn={overlayOn}
                onLoad={() => setLoaded(true)}
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

/**
 * The photo, and whatever is behind it — the three seconds a Live Photo
 * carries, or the photograph under a Snapchat memory's caption layer.
 *
 * One gesture reveals both, because they are the same gesture: press and hold,
 * let go and it goes back. Which one a photo has is a property of the photo and
 * never of the press, and nothing in this archive has ever had both.
 *
 * The rendition being revealed is asked for the moment the viewer opens rather
 * than when the press begins. For a Live Photo that means the server's ffmpeg
 * has long finished by the time anyone decides to hold; for a memory it means
 * the photograph is already in the browser's cache and the hold is instant.
 */
function PhotoStage({
  item,
  alt,
  loaded,
  hasOverlay,
  overlayOn,
  onLoad,
}: {
  item: TimelineItem;
  alt: string;
  loaded: boolean;
  hasOverlay: boolean;
  overlayOn: boolean;
  onLoad: () => void;
}) {
  const fade = useLiveFade();
  const timer = useRef(0);
  const [playing, setPlaying] = useState(false);
  const [pressed, setPressed] = useState(false);
  const live = item.live === "ready";

  useEffect(() => () => window.clearTimeout(timer.current), []);

  /**
   * Ends whatever the press started, whether it ran out or the press did.
   *
   * Pausing and rewinding wait for the fade to finish: doing either to a video
   * that is still half on screen is the jump cut the fade is there to hide.
   */
  const stop = () => {
    window.clearTimeout(timer.current);
    setPressed(false);
    fade.end(() => {
      setPlaying(false);
      const el = fade.ref.current;
      if (el) {
        el.pause();
        el.currentTime = 0;
      }
    });
  };

  const press = (e: React.PointerEvent) => {
    if ((!live && !hasOverlay) || e.button !== 0) return;
    // Claims the pointer, so releasing anywhere on the page still ends the
    // press — including the drag that a hold on a photo naturally becomes.
    e.currentTarget.setPointerCapture(e.pointerId);
    // Suppresses the image drag and iOS's press-and-hold callout, both of which
    // fire on exactly this gesture.
    e.preventDefault();

    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(async () => {
      // A memory has nothing to start: the photograph is already mounted under
      // the composite, and the hold is only what makes it visible.
      setPressed(true);

      const el = fade.ref.current;
      if (!live || !el) return;
      el.currentTime = 0;
      setPlaying(true);
      try {
        await el.play();
      } catch {
        // A press is a user gesture, so sound is allowed — but a browser that
        // disagrees should cost the audio, not the animation.
        el.muted = true;
        await el.play().catch(stop);
      }
    }, HOLD_MS);
  };

  return (
    <span
      className="contents"
      onPointerDown={press}
      onPointerUp={stop}
      onPointerCancel={stop}
    >
      {/* The preview renders straight from the blob, so it works even while the
          thumbnail job is still queued. It stays lit underneath the clip rather
          than crossing over with it: two images dissolving into each other are
          both part-transparent halfway through, and the backdrop showing between
          them dims the photo just as the motion starts. */}
      <img
        className={MEDIA}
        src={previewUrl(item.id)}
        alt={alt}
        draggable={false}
        onLoad={onLoad}
        style={{ opacity: loaded ? 1 : 0 }}
      />

      {hasOverlay ? (
        <>
          {/* The photograph Snapchat shipped, over the composite rather than
              under it: an absolutely positioned sibling paints on top, and the
              two are the same picture at the same size, so fading this in is
              the caption disappearing. Mounted from the start, which is what
              makes the hold instant and what leaves the composite showing for
              the moment before these bytes arrive. */}
          <img
            className={cn(MEDIA, "absolute")}
            src={plainPreviewUrl(item.id)}
            alt=""
            draggable={false}
            style={{ opacity: !overlayOn || pressed ? 1 : 0 }}
          />
          {overlayOn && !pressed ? (
            <span className={HINT_BADGE}>
              <OverlayGlyph />
              Hold to hide
            </span>
          ) : null}
        </>
      ) : null}

      {live ? (
        <>
          <video
            // Dissolves in once the clip is really playing, and back out into
            // the photo before its last frame. It brings its own transition, so
            // MEDIA's 120ms is left to the still underneath.
            {...fade.props}
            className={cn(MEDIA, "absolute")}
            src={livePreviewUrl(item.id)}
            preload="auto"
            playsInline
            onEnded={stop}
            onError={stop}
          />
          {!playing ? (
            <span className={HINT_BADGE}>
              <LiveGlyph />
              Hold to play
            </span>
          ) : null}
        </>
      ) : null}
    </span>
  );
}

function LiveGlyph() {
  return (
    <svg viewBox="0 0 24 24" width="14" height="14" aria-hidden="true">
      <circle cx="12" cy="12" r="3.1" fill="currentColor" />
      <circle cx="12" cy="12" r="6.2" fill="none" stroke="currentColor" strokeWidth="1.5" />
      <circle
        cx="12"
        cy="12"
        r="9.3"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeDasharray="2.4 2.6"
      />
    </svg>
  );
}

/**
 * A video, and for a Snapchat memory the choice between the two renditions of
 * it.
 *
 * There is no press-and-hold here and there cannot be: the caption is in the
 * pixels, because nothing in a browser will composite a PNG over a playing
 * <video>, so revealing the photograph underneath means fetching a different
 * file. The toggle swaps the source and the playhead is carried across, which
 * is as close to the still's gesture as a second download gets.
 */
/**
 * Two stacked sheets: the photograph and the layer somebody drew over it. The
 * slash is the layer taken away, which is what the button does.
 */
function OverlayGlyph({ off = false }: { off?: boolean }) {
  return (
    <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true">
      <path
        d="M12 3.6 3.4 8 12 12.4 20.6 8 12 3.6Z"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinejoin="round"
      />
      <path
        d="M4.6 12.6 12 16.4l7.4-3.8"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      {off ? (
        <path
          d="M4 20 20 4"
          stroke="currentColor"
          strokeWidth="1.7"
          strokeLinecap="round"
        />
      ) : null}
    </svg>
  );
}

function VideoStage({ item, plain }: { item: TimelineItem; plain: boolean }) {
  // Sampled from the element as it plays rather than read at the moment of the
  // swap: by the time React has re-rendered, the element has already been
  // reset by its new source and its playhead is gone.
  const at = useRef(0);
  const [plainBroken, setPlainBroken] = useState(false);

  if (item.playback_state === "ready") {
    // A rendition the transcode has not caught up with yet is a 404, and the
    // composite is a better answer than a black rectangle.
    const showPlain = plain && !plainBroken;
    return (
      <video
        // Keyed by the asset and not by the source: changing the src on the
        // same element is what lets the playhead be restored, where a remount
        // would start the clip over.
        key={item.id}
        className={MEDIA}
        src={showPlain ? plainPlaybackUrl(item.id) : playbackUrl(item.id)}
        poster={item.state === "ready" ? thumbUrl(item.id) : undefined}
        controls
        autoPlay
        playsInline
        onTimeUpdate={(e) => {
          at.current = e.currentTarget.currentTime;
        }}
        onLoadedMetadata={(e) => {
          if (at.current > 0 && at.current < e.currentTarget.duration) {
            e.currentTarget.currentTime = at.current;
          }
        }}
        onError={() => {
          if (showPlain) setPlainBroken(true);
        }}
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
