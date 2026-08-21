/**
 * What a file fetched out of iCloud should be called on the way to the archive.
 *
 * A shared album's copy is not the photograph that was taken. Apple re-encodes
 * every still it accepts down to 2048 pixels and, in this library, to JPEG
 * without exception — while `PHAssetResource.originalFilename` goes on naming
 * the HEIC the photographer shot. Thirty-nine of the first forty-five shared
 * stills arrived as `IMG_6822.HEIC` carrying JPEG bytes.
 *
 * That name is not cosmetic. The server classifies an upload by its extension
 * where nothing contradicts it, so the archive stored those thirty-nine as
 * `image/heic`: a type no browser will open, on files that are ordinary JPEGs.
 *
 * The resource's uniform type identifier is the one field that describes what
 * Apple actually handed over, so it is the one that names the file. The stem is
 * kept, because `IMG_6822` is how the photograph is known to everybody in the
 * album and is worth more than the three letters after it.
 *
 * Pure, and importing nothing, so it is tested in Node rather than only on a
 * phone.
 */

/** The extension each uniform type identifier is stored under. */
const EXTENSIONS: Record<string, string> = {
  'public.jpeg': '.jpg',
  'public.heic': '.heic',
  'public.heif': '.heic',
  'public.heics': '.heic',
  'public.png': '.png',
  'com.compuserve.gif': '.gif',
  'public.tiff': '.tiff',
  'com.adobe.raw-image': '.dng',
  'com.adobe.dng': '.dng',
  'org.webmproject.webp': '.webp',
  'public.webp': '.webp',
  'com.apple.quicktime-movie': '.mov',
  'public.mpeg-4': '.mp4',
  'public.avi': '.avi',
};

/**
 * Extensions that already name the same format as the one they map to.
 *
 * A file called `.jpeg` is not renamed to `.jpg`: the point is to correct names
 * that are wrong, and renaming ones that are merely spelled differently would
 * churn filenames in the archive to say nothing new.
 */
const ALIASES: Record<string, string> = {
  '.jpeg': '.jpg',
  '.jpe': '.jpg',
  '.heif': '.heic',
  '.tif': '.tiff',
  '.m4v': '.mp4',
  '.qt': '.mov',
};

/**
 * `filename` renamed to match what the bytes actually are.
 *
 * Left exactly as it came for a type identifier this does not recognise, which
 * is the honest answer: an unknown identifier means this file cannot say what
 * the file is, and overwriting a name on that basis would be worse than the
 * name being wrong.
 */
export function sharedResourceName(filename: string, uniformTypeIdentifier: string | null): string {
  const wanted = uniformTypeIdentifier
    ? EXTENSIONS[uniformTypeIdentifier.trim().toLowerCase()]
    : undefined;
  if (wanted === undefined) return filename;

  const dot = filename.lastIndexOf('.');
  const stem = dot > 0 ? filename.slice(0, dot) : filename;
  const had = dot > 0 ? filename.slice(dot).toLowerCase() : '';

  if (had === wanted || ALIASES[had] === wanted) return filename;
  return stem + wanted;
}
