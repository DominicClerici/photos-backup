// The continuous zoom position and its persistence.
//
// The module itself lives in packages/core, where both clients read it and
// where its node --test file sits beside it. This is a re-export so that the
// components importing "@/lib/zoom" did not all have to change on the day it
// moved; there is nothing here but names.

export {
  ZOOM_MS,
  Zoom,
  savedZoom,
} from "@photobackup/core";
