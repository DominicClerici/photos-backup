import { File } from 'expo-file-system';
import { Asset, AssetField, MediaType, Query } from 'expo-media-library';

export type ResolvedAsset = {
  asset: Asset;
  filename: string;
  uri: string;
  size: number;
  resolveMs: number;
};

/**
 * Resolving a URI is not free on iOS — for video it goes through
 * requestAVAsset, and a slow-motion clip is re-exported rather than handed
 * over. So this reports the cost per asset instead of hiding it.
 */
export async function resolve(asset: Asset): Promise<ResolvedAsset> {
  const t0 = Date.now();
  const uri = await asset.getUri();
  const resolveMs = Date.now() - t0;
  const filename = await asset.getFilename();
  const file = new File(uri);
  return { asset, filename, uri, size: file.exists ? file.size : 0, resolveMs };
}

export async function largestVideo(
  candidates: number,
  onProgress?: (msg: string) => void
): Promise<ResolvedAsset | null> {
  let assets: Asset[] = [];
  try {
    assets = await new Query()
      .eq(AssetField.MEDIA_TYPE, MediaType.VIDEO)
      .orderBy({ key: AssetField.DURATION, ascending: false })
      .limit(candidates)
      .exe();
  } catch {
    onProgress?.('orderBy(duration) rejected, retrying unsorted');
    assets = await new Query().eq(AssetField.MEDIA_TYPE, MediaType.VIDEO).limit(candidates).exe();
  }

  onProgress?.(`${assets.length} video candidates`);
  let best: ResolvedAsset | null = null;
  for (const asset of assets) {
    try {
      const r = await resolve(asset);
      onProgress?.(`  ${r.filename} ${(r.size / 1024 / 1024).toFixed(1)}MB (resolve ${r.resolveMs}ms)`);
      if (!best || r.size > best.size) best = r;
    } catch (e) {
      onProgress?.(`  resolve failed: ${String(e)}`);
    }
  }
  return best;
}

export async function countLibrary() {
  const [photos, videos] = await Promise.all([
    new Query().eq(AssetField.MEDIA_TYPE, MediaType.IMAGE).exeForMetadata(),
    new Query().eq(AssetField.MEDIA_TYPE, MediaType.VIDEO).exeForMetadata(),
  ]);
  return { photos: photos.length, videos: videos.length };
}
