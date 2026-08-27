// The grid's geometry.
//
// The module itself lives in packages/core, where both clients read it and
// where its node --test file sits beside it. This is a re-export so that the
// components importing "@/lib/layout" did not all have to change on the day it
// moved; there is nothing here but names.

export {
  BASE_THUMB_SIZE,
  DEFAULT_GAP,
  DEFAULT_HEADER_HEIGHT,
  DEFAULT_ZOOM,
  MAX_ZOOM,
  THUMB_SIZES,
  ZOOM_LEVELS,
  countOf,
  dayAt,
  dayIndexOf,
  dayKeyOf,
  dayLabelOf,
  daysFrom,
  frameAt,
  headerY,
  headless,
  itemAtPoint,
  itemTop,
  layoutLevel,
  metricsFor,
  thumbSizeFallbacks,
  thumbSizeFor,
  tileRect,
  visibleItems,
} from "@photobackup/core";

export {
  type Day,
  type Frame,
  type GridMetrics,
  type ItemRange,
  type LevelLayout,
  type Rect,
  type ThumbSize,
} from "@photobackup/core";
