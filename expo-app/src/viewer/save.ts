import { media, notify, notifyError } from '@photobackup/core';
import { Directory, File, Paths } from 'expo-file-system';
import { Asset, getPermissionsAsync, requestPermissionsAsync } from 'expo-media-library';

/** Where an original lands on its way to the camera roll. */
const STAGE = 'saves';

/**
 * The phone's answer to the browser's Download button.
 *
 * A browser hands the bytes to whatever the person has set up for files; a
 * phone has one place a photograph goes, and it is the camera roll. So this
 * fetches the original — the archived file, untouched, not a rendition — and
 * gives it to PhotoKit.
 *
 * It is a download rather than a link because there is no such thing as a link
 * here: every rendition this app fetches carries the device token in a header,
 * which is the whole reason `media()` returns headers beside a URL and the one
 * line WEB_TO_MOBILE § 3.2 says separates the two clients. `File.downloadFileAsync`
 * takes them, so the original authenticates exactly as the thumbnail did.
 *
 * The staged file is deleted whether or not the save worked. It is a copy of
 * something the archive already holds, and leaving originals in the cache
 * directory of a backup app is how a phone quietly fills up.
 */
export async function saveOriginal(id: string, filename: string): Promise<void> {
  let staged: File | null = null;
  try {
    // Write-only: this app already holds full library access for the backup
    // engine, but asking for the narrower one here is what makes the prompt say
    // "add to your photos" on a phone that has not granted anything yet.
    const held = await getPermissionsAsync(true);
    const permission = held.granted ? held : await requestPermissionsAsync(true);
    if (!permission.granted) {
      notify({
        type: 'error',
        title: 'Photos access is off',
        description: 'Allow photobackup to add photos in Settings, then try again.',
      });
      return;
    }

    const directory = new Directory(Paths.cache, STAGE);
    directory.create({ intermediates: true, idempotent: true });

    // Named for the file rather than the asset id, because the name is what
    // survives into the camera roll and what somebody looking for it later will
    // have in mind. Two photographs with the same filename are one overwrite of
    // a staging copy apart, which is why the previous one goes first.
    staged = new File(directory, safeName(filename, id));
    if (staged.exists) staged.delete();

    const source = media(id, 'original');
    await File.downloadFileAsync(source.uri, staged, {
      headers: source.headers,
      idempotent: true,
    });

    await Asset.create(staged.uri);
    notify({ type: 'success', title: 'Saved to your photos', description: filename });
  } catch (err) {
    notifyError(err, 'save this photograph');
  } finally {
    try {
      if (staged?.exists) staged.delete();
    } catch {
      // A staged copy that cannot be removed is a few megabytes in the cache
      // directory, which the system reclaims. It is not worth a second notice
      // on top of whatever already failed.
    }
  }
}

/**
 * A filename PhotoKit will take.
 *
 * Path separators are the only thing that could turn a name from the archive
 * into a write somewhere else, and an empty name would produce a file with no
 * name at all — the asset id is a better answer than either.
 */
function safeName(filename: string, id: string): string {
  const cleaned = filename.replace(/[/\\]/g, '_').trim();
  return cleaned === '' ? id : cleaned;
}
