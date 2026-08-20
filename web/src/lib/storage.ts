import type { StorageStatus } from "./api";

/** One line of the storage card's breakdown. */
export interface StorageRow {
  key: string;
  label: string;
  bytes: number;
  /** Why this line exists, for the rows whose name does not say. */
  hint?: string;
}

/**
 * The storage card's arithmetic, kept out of the drawing.
 *
 * `used` and `free` come from the kernel and close on `total`, so the pie has
 * no gap in it. The rows do not close on `used` — the database, the vault and
 * the blocks the filesystem reserves for root are bytes nobody asked this
 * archive about — so whatever is left over is a row of its own rather than
 * quietly distributed among the others.
 */
export interface StorageBreakdown {
  total: number;
  used: number;
  free: number;
  /** Slices of `used`, on the volume the pie is of. */
  rows: StorageRow[];
  /**
   * The renditions, when they live on a different disk from the originals —
   * which is the deployed arrangement: blobs on the external drive, thumbnails
   * on the SSD. Listed apart because adding them into the pie would draw space
   * that is not on the drive the pie is of.
   */
  elsewhere: StorageRow[];
  elsewherePath: string;
}

function row(key: string, label: string, bytes: number, hint?: string): StorageRow {
  return { key, label, bytes, hint };
}

export function breakdown(storage: StorageStatus): StorageBreakdown {
  const { archive, derivatives, same_volume: same } = storage;

  const rows: StorageRow[] = [
    row("photos", "Photos", storage.photos),
    row("videos", "Videos", storage.videos),
  ];
  const derivativeRows = [
    row("photo_derivatives", "Photo derivatives", storage.photo_derivatives),
    row("video_derivatives", "Video derivatives", storage.video_derivatives),
  ];
  if (same) rows.push(...derivativeRows);

  const accounted = rows.reduce((sum, r) => sum + r.bytes, 0);
  rows.push(
    row(
      "other",
      "Everything else",
      Math.max(0, archive.used - accounted),
      "The database, the vault, and the space the filesystem holds back",
    ),
  );

  return {
    total: archive.total,
    used: archive.used,
    free: archive.free,
    rows,
    elsewhere: same ? [] : derivativeRows,
    elsewherePath: derivatives.path,
  };
}

/** What fraction of the drive is gone, as a percentage worth printing. */
export function percentUsed(total: number, used: number): number {
  if (total <= 0) return 0;
  return Math.round((used / total) * 100);
}
