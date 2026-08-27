/**
 * The Archive and Hidden buckets, and the lock in front of them.
 *
 * Three screens and one prompt. The prompt is mounted once by the root layout
 * and opened from anywhere — a hold on a tile in the library, a bucket that has
 * drawn nothing, a write that came back 423 — which is the only shape that
 * serves all three. See `Gate`.
 *
 * The rule this directory exists to keep in one place: nothing in the vault is
 * written to disk on the phone. Not a day table, not a page of filenames, not a
 * thumbnail. `src/gallery/cache.ts` refuses a vault key and `BucketTimeline`
 * asks for no store, and the tiles and the viewer keep decrypted bytes in
 * memory only. A cache of what somebody hid, sitting in the app's sandbox after
 * the server has re-locked, would be the feature undone.
 */
export { BucketTimeline } from './BucketTimeline';
export { BucketView } from './BucketView';
export { VaultGate } from './Gate';
