// The sort-and-filter rules.
//
// The module itself lives in packages/core, where both clients read it and
// where its node --test file sits beside it. This is a re-export so that the
// components importing "@/lib/view" did not all have to change on the day it
// moved; there is nothing here but names.

export {
  DEFAULT_VIEW,
  FACET_LABEL,
  SORT_LABEL,
  byDuration,
  describeView,
  facetsFor,
  facetsOn,
  isDefault,
  isFiltered,
  pickSort,
  toggleFacet,
  viewKey,
  withinFacets,
} from "@photobackup/core";

export {
  type Facet,
  type Facets,
} from "@photobackup/core";
