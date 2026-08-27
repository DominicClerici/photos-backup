/**
 * The half of the gallery that has no opinion about how it is drawn.
 *
 * Zero runtime dependencies and no React, which is what makes the React version
 * skew between the two apps a non-issue rather than a resolution problem: this
 * is the half both of them import unconditionally.
 *
 * Install `configure` before anything else reads. `installNotifier` can wait
 * until there is somewhere to show a notice, but not past the first write.
 */

export {
  configure,
  rearmUnauthorized,
  baseUrl as apiBaseUrl,
  headers as apiHeaders,
  type Transport,
} from "./wire/transport.ts";
export * from "./wire/api.ts";
export * from "./notify.ts";

// The pure modules: the grid's geometry, the sort-and-filter rules, selections
// as index ranges, the continuous zoom, query parsing, and the formatters.
// Every one of them has a node --test file beside it and none of them has ever
// needed a DOM.
export * from "./lib/layout.ts";
export * from "./lib/view.ts";
export * from "./lib/ranges.ts";
export * from "./lib/zoom.ts";
export * from "./lib/search.ts";
export * from "./lib/format.ts";
