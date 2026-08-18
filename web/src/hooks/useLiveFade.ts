"use client";

import {
  useCallback,
  useEffect,
  useRef,
  type CSSProperties,
  type RefObject,
  type TransitionEvent,
} from "react";

/**
 * How long a Live Photo takes to dissolve in over its still, and back out.
 *
 * Short enough to read as the photo coming alive rather than as a transition in
 * its own right, long enough to cover the seam between a still frame and a video
 * frame of the same moment — different codec, different sharpening, and the swap
 * is plainly a cut when it happens in one frame.
 *
 * Leaving is the slower half. Coming in, the motion itself carries the change
 * and a longer fade only delays it; going out, the picture is settling back to
 * where it started and wants letting down gently.
 */
export const LIVE_FADE_IN_MS = 75;
export const LIVE_FADE_OUT_MS = 100;

/** The resting state, and the transition a clip that never played goes out on. */
const FADE_STYLE: CSSProperties = {
  opacity: 0,
  transition: `opacity ${LIVE_FADE_IN_MS}ms linear`,
};

/**
 * How long after a fade the settle is forced through anyway.
 *
 * `transitionend` is the honest signal and normally the one that fires; this is
 * for the times it cannot, chiefly a background tab, where the alternative is a
 * video left playing under a pointer that has gone.
 */
const SETTLE_TIMEOUT_MS = LIVE_FADE_OUT_MS + 50;

export interface LiveFade {
  /** The <video>, for the caller's own play/pause/seek. */
  ref: RefObject<HTMLVideoElement | null>;
  /** Spread onto the <video>: opacity, its transition, and both fade triggers. */
  props: {
    ref: RefObject<HTMLVideoElement | null>;
    style: CSSProperties;
    onPlaying: () => void;
    onTransitionEnd: (event: TransitionEvent<HTMLVideoElement>) => void;
  };
  /**
   * Fade out now, cutting the clip short, and run `after` once the still is back
   * on its own — the moment at which pausing, rewinding or unmounting the video
   * costs nothing visible. Called with nothing to fade, it settles immediately.
   */
  end: (after?: () => void) => void;
}

/**
 * The dissolve either side of a Live Photo, wherever one plays over its still.
 *
 * The fade at the end of the clip is why this needs the video's own clock: it
 * has to *finish* as the last frame does, so it starts early rather than on
 * `ended`. Fading out after the fact would mean dissolving away from a picture
 * that had already stopped moving, which is the freeze the effect exists to
 * hide.
 *
 * Opacity is written to the element rather than held in state, for the same
 * reason: a render between deciding to fade and the browser seeing it is a frame
 * or two, which is most of a fade this short and all of the precision at the end
 * of one. Nothing here re-renders the component. The style prop stays at the
 * resting value so React, which only writes the style keys it sees change, never
 * clobbers what has been set underneath it.
 */
export function useLiveFade(): LiveFade {
  const ref = useRef<HTMLVideoElement>(null);

  // Whether the clip is meant to be on screen, whether it is still on its way
  // off, and what is waiting for it to get there.
  const visible = useRef(false);
  const fading = useRef(false);
  const waiting = useRef<(() => void) | null>(null);

  const frame = useRef(0);
  const timer = useRef(0);

  useEffect(
    () => () => {
      cancelAnimationFrame(frame.current);
      window.clearTimeout(timer.current);
    },
    [],
  );

  const settle = useCallback(() => {
    window.clearTimeout(timer.current);
    fading.current = false;
    const done = waiting.current;
    waiting.current = null;
    done?.();
  }, []);

  const hide = useCallback(() => {
    cancelAnimationFrame(frame.current);
    if (!visible.current) return;
    visible.current = false;
    fading.current = true;
    if (ref.current) {
      ref.current.style.transitionDuration = `${LIVE_FADE_OUT_MS}ms`;
      ref.current.style.opacity = "0";
    }
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(settle, SETTLE_TIMEOUT_MS);
  }, [settle]);

  /**
   * Start the fade in. Wired to `playing` rather than sitting next to `play()`:
   * before the first frame is decoded there is nothing to fade up to, and on a
   * clip the browser has not cached that gap is long enough to see.
   */
  const begin = useCallback(() => {
    const el = ref.current;
    if (!el) return;

    // A clip that stalled and recovered lands here too, and it must not inherit
    // the settle a fade-out queued: that would pause a video that is playing.
    window.clearTimeout(timer.current);
    waiting.current = null;
    fading.current = false;

    visible.current = true;
    el.style.transitionDuration = `${LIVE_FADE_IN_MS}ms`;
    el.style.opacity = "1";

    // A frame callback, not `timeupdate`, which fires about four times a second
    // and would sail straight past a 75ms window. Reading the clock also keeps
    // the fade honest when playback stalls, where a timer armed at `play()`
    // would have started dissolving over a picture that was still going.
    cancelAnimationFrame(frame.current);
    const watch = () => {
      const playing = ref.current;
      if (!playing) return;
      if (
        Number.isFinite(playing.duration) &&
        playing.currentTime >= playing.duration - LIVE_FADE_OUT_MS / 1000
      ) {
        hide();
        return;
      }
      frame.current = requestAnimationFrame(watch);
    };
    frame.current = requestAnimationFrame(watch);
  }, [hide]);

  const end = useCallback(
    (after?: () => void) => {
      hide();
      if (!fading.current) {
        after?.();
        return;
      }
      // Both the clip running out and the press letting go during the closing
      // fade arrive here, in either order. Whichever brought work keeps it: an
      // `end` with nothing to do must not cancel the unmount the other one is
      // waiting on.
      if (after) waiting.current = after;
    },
    [hide],
  );

  const onTransitionEnd = useCallback(
    (event: TransitionEvent<HTMLVideoElement>) => {
      if (event.propertyName === "opacity" && !visible.current) settle();
    },
    [settle],
  );

  return {
    ref,
    props: { ref, style: FADE_STYLE, onPlaying: begin, onTransitionEnd },
    end,
  };
}
