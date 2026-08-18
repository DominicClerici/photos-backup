import { Asset, AssetField, MediaType, Query, type AssetMetadata } from 'expo-media-library';

export type PickerItem = {
  id: string;
  filename: string;
  createdAt: number | null;
};

/**
 * Lists recent photos using exeForMetadata, which reads the media store without
 * resolving file paths. Resolving is the expensive step, so it is deferred to
 * thumbnailUri and to the moment an asset is actually chosen.
 */
export async function recentPhotos(limit: number): Promise<PickerItem[]> {
  const metadata: AssetMetadata[] = await new Query()
    .eq(AssetField.MEDIA_TYPE, MediaType.IMAGE)
    .orderBy({ key: AssetField.CREATION_TIME, ascending: false })
    .limit(limit)
    .exeForMetadata();

  return metadata.map((m) => ({
    id: m.id,
    filename: m.filename ?? m.id,
    createdAt: m.creationTime,
  }));
}

/**
 * React Native's image loader has no handler for ph:// URIs, so thumbnails need
 * the resolved on-disk path. PhotoKit appends a `#<base64 plist>` fragment that
 * the loader treats as part of the filename, so it is stripped here; Phase 0
 * confirmed the stripped path still reads.
 */
export async function thumbnailUri(id: string): Promise<string> {
  const uri = await new Asset(id).getUri();
  return uri.split('#')[0];
}
