"use client";

import { useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";

import {
  fetchAnalysis,
  fetchAsset,
  livePreviewUrl,
  originalUrl,
  plainPlaybackUrl,
  plainPreviewUrl,
  playbackUrl,
  previewUrl,
  thumbUrl,
  type AnalysisTag,
  type AssetAnalysis,
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
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useLiveFade } from "@/hooks/useLiveFade";
import { askFor } from "@/lib/search";
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
  /**
   * This collection is the vault, so the panel may show what the sealed
   * document says and may not offer to look anything up.
   *
   * The names on a hidden photograph came out of the vault; typing one into
   * /v1/search would put it in the URL, in the browser's history, and in the
   * list of recent searches this app keeps — three places outside the vault,
   * for a name the vault is holding. Nothing else in the panel is affected,
   * because nothing else in it exists: the ML passes all refuse a sealed asset
   * and the analysis comes back empty.
   */
  sealed?: boolean;
}

export function Viewer({ at, total, index, onClose, onNavigate, sealed }: Props) {
  const router = useRouter();
  const item = at(index);
  const [detail, setDetail] = useState<AssetDetail | null>(null);
  const [analysis, setAnalysis] = useState<AssetAnalysis | null>(null);
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

  // What the models said, fetched only while the panel is open and asked again
  // for each photograph stepped to with it open.
  //
  // Separate from the detail above rather than folded into it, because the two
  // are wanted at different moments: the detail is on every arrow-key press and
  // this is on a toggle, and a photograph of a terminal carries kilobytes of
  // recognised text that nobody with the panel shut has asked to download.
  useEffect(() => {
    if (!item || !panelOpen) return;
    setAnalysis(null);
    const controller = new AbortController();
    fetchAnalysis(item.id, controller.signal)
      .then(setAnalysis)
      .catch(() => {
        // The rest of the panel still draws, and this half goes on saying it is
        // reading — the same thing the panel already does when the detail fetch
        // fails, and for the same reason: a photograph is not the place to
        // report that the server is unreachable, and the status page is.
      });
    return () => controller.abort();
  }, [item?.id, panelOpen]);

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

      {panelOpen ? (
        <MetadataPanel
          detail={detail}
          analysis={analysis}
          onSearch={
            sealed
              ? undefined
              : (text) => {
                  onClose();
                  router.push(`/search?${askFor(text)}`);
                }
          }
        />
      ) : null}
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

function MetadataPanel({
  detail,
  analysis,
  onSearch,
}: {
  detail: AssetDetail | null;
  /** Null while it is still being fetched, and after a fetch that failed. */
  analysis: AssetAnalysis | null;
  /**
   * Ask the archive for everything else that was called this, or undefined
   * where nothing may be looked up — see Props.sealed.
   */
  onSearch?: (text: string) => void;
}) {
  if (!detail) {
    return (
      <aside className={PANEL}>
        <p className="text-[13px] text-faint">Loading details…</p>
      </aside>
    );
  }

  const capture = formatCaptureTime(detail.taken_at, detail.offset_minutes);
  const camera = [detail.camera_make, detail.camera_model].filter(Boolean).join(" ");
  // City, state, country, dropping whatever the geocoder had no answer for —
  // an island nation has no first-order division, and a photograph over open
  // water has none of the three.
  const place = [detail.place_city, detail.place_admin1, detail.place_country]
    .filter(Boolean)
    .join(", ");
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

      <Analysis
        analysis={analysis}
        people={detail.people}
        description={detail.description}
        onSearch={onSearch}
      />

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
              {place || formatCoords(detail.gps_lat, detail.gps_lon)}
            </a>
            {/* The coordinates stay, underneath, when there is a name for them.
                The name is what the photograph is of; the numbers are what the
                camera recorded, and dropping them would hide a bad fix behind a
                plausible-looking city. */}
            {place ? (
              <span className={PANEL_HINT}>
                {formatCoords(detail.gps_lat, detail.gps_lon)}
              </span>
            ) : null}
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

/**
 * The four passes, in the order a photograph goes through them, and what to
 * call each one to somebody who is not holding ML_IMAGES.md.
 *
 * `mlprep` is in here even though it is not ML at all, because it is the one
 * whose failure explains all three of the others: no rendition means nothing
 * ever looked at the photograph, and three separate "not captioned yet" lines
 * would be three symptoms of one cause.
 */
const PASSES = ["mlprep", "vision", "ocr", "describe"] as const;

const PASS_LABEL: Record<string, string> = {
  mlprep: "the rendition",
  vision: "the encoder",
  ocr: "the text recogniser",
  describe: "the captioner",
};

/**
 * What the models have said about this photograph — and, where they have said
 * nothing, which of the two reasons that is.
 *
 * This is a search read backwards. /v1/search takes a sentence and ranks
 * photographs out of these four tables; this takes a photograph and shows the
 * words the ranking was built from. Which is the question somebody has the
 * moment a search returns something surprising, and it is also the only way to
 * see what the model actually called things — the thing ML_IMAGES.md §9's tag
 * cleanup has to be read through.
 *
 * Nothing here is drawn as blank. A photograph with no caption is one the
 * captioner has not reached, or one it failed on, or one nothing has queued —
 * and a panel that drew all three the same way would be §11's silent exclusion
 * with a nicer font. See `notes`.
 */
function Analysis({
  analysis,
  people,
  description,
  onSearch,
}: {
  analysis: AssetAnalysis | null;
  /** Names an import carried, which are not the model's and never become tags. */
  people?: string[];
  /** What a person typed under the photograph, before this archive existed. */
  description?: string;
  onSearch?: (text: string) => void;
}) {
  const tags = analysis?.tags ?? [];
  const said = Boolean(analysis?.caption || tags.length || analysis?.text);
  const pending = notes(analysis);

  // A Live Photo's video half, a Snapchat overlay layer, anything in the vault:
  // the ML pass is 23% smaller than the library and skips these by construction
  // rather than by a queue that has not got to them. There is no story to tell
  // about a photograph nothing was ever going to look at.
  if (analysis && !said && !description && !people?.length && pending.length === 0) {
    return null;
  }

  return (
    <section className="mb-5 flex flex-col gap-3.5 border-b pb-5">
      {/* Only where there is a sentence to head, or one still coming. A heading
          over nothing is the blank this component exists not to draw — the note
          at the foot says why there is no caption, and says it in words. */}
      {analysis == null || analysis.caption ? (
        <div>
          <h4 className="mb-[3px] text-[11px] tracking-[0.06em] text-faint uppercase">
            Analysis
          </h4>
          {analysis == null ? (
            <p className="text-[13px] text-faint">Reading what the models said…</p>
          ) : (
            <p className="text-[13px] leading-[1.45]">{analysis.caption}</p>
          )}
          {analysis?.caption_model ? (
            <span className={PANEL_HINT}>{analysis.caption_model}</span>
          ) : null}
        </div>
      ) : null}

      {tags.length ? (
        <div>
          <h4 className="mb-1.5 text-[11px] tracking-[0.06em] text-faint uppercase">
            Tags
          </h4>
          <span className="flex flex-wrap gap-1.5">
            {tags.map((tag) => (
              <TagChip key={tag.name + tag.raw} tag={tag} onSearch={onSearch} />
            ))}
          </span>
        </div>
      ) : null}

      {people?.length ? (
        <div>
          <h4 className="mb-1.5 text-[11px] tracking-[0.06em] text-faint uppercase">
            People
          </h4>
          <span className="flex flex-wrap gap-1.5">
            {people.map((name) =>
              onSearch ? (
                <Badge
                  key={name}
                  variant="outline"
                  className="cursor-pointer hover:bg-muted"
                  render={<button type="button" />}
                  onClick={() => onSearch(name)}
                  title={`Search for ${name}`}
                >
                  {name}
                </Badge>
              ) : (
                <Badge key={name} variant="outline">
                  {name}
                </Badge>
              ),
            )}
          </span>
          {/* The seam ML_IMAGES.md §11 asks to keep visible: a name somebody
              confirmed is a different kind of claim from a word a model wrote,
              and the panel should not be the place the two quietly become one
              list of things "in" the photograph. */}
          <span className={PANEL_HINT}>named when this was imported</span>
        </div>
      ) : null}

      {description ? (
        <div>
          <h4 className="mb-[3px] text-[11px] tracking-[0.06em] text-faint uppercase">
            Description
          </h4>
          <p className="text-[13px] leading-[1.45] [overflow-wrap:anywhere]">
            {description}
          </p>
          <span className={PANEL_HINT}>written by a person, not a model</span>
        </div>
      ) : null}

      {analysis?.text ? (
        <div>
          <h4 className="mb-1.5 text-[11px] tracking-[0.06em] text-faint uppercase">
            Recognised text
          </h4>
          {/* The whole of it, scrolled rather than truncated. A search shows the
              matching line; somebody who opened this panel on a receipt wants
              the receipt. Rendered as text and never as markup — a photograph of
              a screen is exactly where a <script> would come from. */}
          <p className="max-h-40 overflow-y-auto rounded-md bg-muted/40 px-2.5 py-2 font-mono text-[11px] leading-[1.5] whitespace-pre-wrap text-muted-foreground">
            {analysis.text}
          </p>
          {analysis.text_model ? (
            <span className={PANEL_HINT}>{analysis.text_model}</span>
          ) : null}
        </div>
      ) : null}

      {pending.length ? (
        <ul className="flex flex-col gap-1 text-[11px] leading-[1.4] text-faint">
          {pending.map((note) => (
            <li key={note}>{note}</li>
          ))}
        </ul>
      ) : null}

      {analysis?.frames ? (
        <span className="text-[11px] text-faint">
          Findable by what it looks like ·{" "}
          {analysis.frames === 1 ? "one frame" : `${analysis.frames} frames`}
          {analysis.vision_model ? ` · ${analysis.vision_model}` : ""}
        </span>
      ) : null}
    </section>
  );
}

/**
 * One word, and the way back to everything else that was called it.
 *
 * Clicking searches rather than filtering the current view, because a tag is a
 * question about the whole archive: "what else did it call a beach" is not a
 * narrowing of the album somebody happens to have open.
 */
function TagChip({
  tag,
  onSearch,
}: {
  tag: AnalysisTag;
  onSearch?: (text: string) => void;
}) {
  const confidence =
    tag.confidence != null && tag.confidence > 0
      ? `${Math.round(tag.confidence * 100)}% sure`
      : "";
  // Only ever set when a merge has folded one word into another, which is the
  // one case where what the model wrote and what a search resolves to are two
  // different facts worth seeing side by side.
  const merged = tag.raw ? `written as “${tag.raw}”` : "";

  const label = (
    <>
      {tag.name}
      {merged ? <span className="text-faint">*</span> : null}
    </>
  );
  const title = [merged, confidence].filter(Boolean).join(" · ");

  if (!onSearch) {
    return (
      <Badge variant="secondary" title={title}>
        {label}
      </Badge>
    );
  }
  return (
    <Badge
      variant="secondary"
      className="cursor-pointer hover:bg-secondary/80"
      render={<button type="button" />}
      onClick={() => onSearch(tag.name)}
      title={[`Search for ${tag.name}`, title].filter(Boolean).join(" · ")}
    >
      {label}
    </Badge>
  );
}

/**
 * Why a pass has said nothing, for each pass that has said nothing.
 *
 * Empty on a photograph that has been all the way through, which is the
 * majority and should cost no lines in the panel. Everything else here is the
 * difference between "there is nothing in this photograph" and "nothing has
 * looked at this photograph", which are the two readings of an empty panel and
 * are not remotely the same news.
 */
function notes(analysis: AssetAnalysis | null): string[] {
  if (!analysis) return [];
  const jobs = analysis.jobs ?? {};

  // A missing rendition is the cause of everything downstream, so it is said
  // once and the three symptoms are left unsaid.
  if (jobs.mlprep === "failed") {
    return ["No ML rendition could be made, so nothing has looked at this."];
  }

  const out: string[] = [];
  for (const pass of PASSES) {
    if (pass === "mlprep") continue;
    const produced =
      pass === "vision"
        ? Boolean(analysis.frames)
        : pass === "ocr"
          ? analysis.text != null
          : analysis.caption != null;
    const state = jobs[pass];

    if (state === "done") {
      // Ran, and found nothing. Worth saying for the recogniser — a photograph
      // with no text in it is the ordinary case and looks identical to one
      // nobody has read — and worth saying for the captioner, where it means
      // the model returned an empty sentence.
      if (!produced) {
        out.push(
          pass === "ocr"
            ? "No text was found in this photograph."
            : `Nothing came back from ${PASS_LABEL[pass]}.`,
        );
      }
      continue;
    }
    if (produced) continue;

    switch (state) {
      case "failed":
        out.push(`${sentence(PASS_LABEL[pass])} failed on this photograph.`);
        break;
      case "running":
        out.push(`${sentence(PASS_LABEL[pass])} is looking at this now.`);
        break;
      case "pending":
        out.push(`Queued for ${PASS_LABEL[pass]}.`);
        break;
      default:
        // No row at all. The ordinary state of a library the captioning
        // backfill has not been run over — it is a typed command rather than
        // something a restart begins, so this is a fact about the archive
        // rather than a fault in it.
        out.push(`${sentence(PASS_LABEL[pass])} has not been run over this.`);
    }
  }
  return out;
}

function sentence(text: string): string {
  return text.charAt(0).toUpperCase() + text.slice(1);
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
