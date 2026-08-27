// Query parsing and the ask-for vocabulary.
//
// The module itself lives in packages/core, where both clients read it and
// where its node --test file sits beside it. This is a re-export so that the
// components importing "@/lib/search" did not all have to change on the day it
// moved; there is nothing here but names.

export {
  CATEGORY_LABEL,
  SEARCH_PARAMS,
  askFor,
  asks,
  chipsOf,
  dateLabel,
  explicitParams,
  forgetSearches,
  parsing,
  placeLabel,
  recentSearches,
  rememberSearch,
  requestOf,
  withoutChip,
} from "@photobackup/core";

export {
  type Chip,
  type ChipKind,
} from "@photobackup/core";
