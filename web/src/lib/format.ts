// Bytes, durations, capture times, coordinates.
//
// The module itself lives in packages/core, where both clients read it and
// where its node --test file sits beside it. This is a re-export so that the
// components importing "@/lib/format" did not all have to change on the day it
// moved; there is nothing here but names.

export {
  ITEMS,
  counted,
  describeAction,
  formatBytes,
  formatCaptureTime,
  formatCoords,
  formatDuration,
  formatOffset,
  formatSince,
  mapLink,
  nounFor,
} from "@photobackup/core";

export {
  type Noun,
  type Subject,
} from "@photobackup/core";
