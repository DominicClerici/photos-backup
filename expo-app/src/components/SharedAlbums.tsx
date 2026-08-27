import { useCallback, useMemo, useRef, useState } from 'react';
import { Pressable, StyleSheet, View } from 'react-native';

import {
  canDownloadShared,
  photoKitSharedProvenance,
  type SharedAsset,
  type SharedProvenance,
} from '../../modules/photo-facts';
import { fetchSharedAssets } from '../sharedalbums/fetch';
import type { FetchRun, SampleRead } from '../sharedalbums/run';
import {
  assetsOf,
  formatDuration,
  pickSample,
  SAMPLE_SIZE,
  SAMPLE_SIZES,
  STILL_LONG_EDGE_CAP,
  summarize,
  throughputMbPerSecond,
  VIDEO_SECONDS_CAP,
  type AlbumSummary,
  type SharedLibrary,
  type SharedSurvey,
} from '../sharedalbums/summary';
import { surveySharedAlbums } from '../sharedalbums/survey';
import { useBackup } from '../state/backup';
import { formatBytes, formatCount } from '../stats/format';
import { color, radius, space } from '../theme';
import { Button, Card, Count, Counts, Pill, Row, Subheading, Text } from '../ui';
import { errorText } from '../sync/types';

/**
 * The iCloud Shared Albums tools, whole.
 *
 * Moved out of `App.tsx` into settings, where they belong: this is a thing you
 * set up once and then look at when a shared photograph turns up wrong. Nothing
 * about what it does changed — the survey, the picker, the fetch diagnostic and
 * the two repairs are the same code with the theme's colours on them.
 *
 * The one piece of state that is not local is the tick list, which lives in
 * `BackupProvider`: the engine reads it at the moment a run starts, and a run
 * can start from the Backup tab while this screen is not mounted.
 */
export function SharedAlbums() {
  const { granted, limitedAccess, chosenIds, chooseAlbums, toggleAlbum, store } = useBackup();

  const [library, setLibrary] = useState<SharedLibrary | null>(null);
  const [surveying, setSurveying] = useState(false);
  const [surveyError, setSurveyError] = useState<string | null>(null);
  const [sampleSize, setSampleSize] = useState(SAMPLE_SIZE);
  const [fetchRun, setFetchRun] = useState<FetchRun | null>(null);
  const [fetching, setFetching] = useState(false);

  // A ref rather than state, because the run reads it from inside a closure
  // that was built when the button was pressed. State captured there is frozen
  // at the value it had then, which for a stop flag is permanently false.
  const cancel = useRef(false);

  /**
   * Looks at the iCloud Shared Albums on this phone without touching one.
   *
   * Nothing here feeds the backup — see `src/sharedalbums/survey.ts` for why it
   * exists at all. It is here rather than behind a developer flag because the
   * thing it measures is on this phone and nowhere else, and the answer decides
   * whether shared albums are worth teaching the queue about.
   */
  const runSurvey = useCallback(async () => {
    setSurveying(true);
    setSurveyError(null);
    // The old readings describe a library that has just been re-read. Leaving
    // them beside fresh counts would invite reading one against the other.
    setFetchRun(null);
    try {
      setLibrary(await surveySharedAlbums());
    } catch (e) {
      setSurveyError(errorText(e));
    } finally {
      setSurveying(false);
    }
  }, []);

  const chosenAlbums = useMemo(() => {
    if (!library) return [];
    const wanted = new Set(chosenIds);
    return library.albums.filter((album) => wanted.has(album.localId));
  }, [library, chosenIds]);

  // Two summaries of the same phone. The first is the picker's rows, which have
  // to list albums that are not selected in order for them to be selectable;
  // the second is everything else on screen, which is about what was chosen.
  const everyAlbum = useMemo(() => (library ? summarize(library.albums) : null), [library]);
  const chosen = useMemo(() => summarize(chosenAlbums), [chosenAlbums]);
  const sample = useMemo(
    () => pickSample(assetsOf(chosenAlbums), sampleSize),
    [chosenAlbums, sampleSize]
  );

  /**
   * Fetches the sample from iCloud, reporting itself all the way through.
   *
   * The run drives its own pacing and retries — see `src/sharedalbums/run.ts`.
   * This owns the two things a screen owns: where the progress goes, and the
   * flag that stops it.
   */
  const runSamples = useCallback(async () => {
    if (sample.length === 0 || fetching) return;
    cancel.current = false;
    setFetching(true);
    setFetchRun(null);
    try {
      await fetchSharedAssets(sample, setFetchRun, () => cancel.current);
    } catch (e) {
      setSurveyError(errorText(e));
    } finally {
      setFetching(false);
    }
  }, [sample, fetching]);

  return (
    <Card title="Shared albums">
      <Text variant="small" tone="muted">
        iCloud Shared Albums live in collections of their own, outside the library the backup
        enumerates. Tick the ones to archive: their photos join the ordinary backup, filed under
        the album&rsquo;s name, with whoever added each one recorded beside it.
      </Text>

      <Button
        label={surveying ? 'Surveying…' : 'Survey shared albums'}
        icon="refresh-cw"
        onPress={() => void runSurvey()}
        busy={surveying}
        disabled={!granted}
      />

      {surveyError && (
        <Text variant="small" tone="warning">
          {surveyError}
        </Text>
      )}

      {limitedAccess && (
        <Text variant="small" tone="warning">
          Limited access hides shared albums entirely, so a survey run now will find none whether
          or not there are any.
        </Text>
      )}

      {library?.supported && library.albums.length > 0 && !canDownloadShared && (
        <Text variant="small" tone="warning">
          This dev client can list shared albums but not fetch them, so ticking one would queue
          photos that every run then fails on. Rebuild it with{' '}
          <Text variant="small" tone="warning" mono>
            pnpm ios
          </Text>{' '}
          first.
        </Text>
      )}

      {library && !library.supported && (
        <Text variant="small" tone="warning">
          This dev client has no shared-album enumerator in it. Rebuild it with{' '}
          <Text variant="small" tone="warning" mono>
            pnpm ios
          </Text>{' '}
          and run the survey again.
        </Text>
      )}

      {library?.supported && library.albums.length === 0 && (
        <Text variant="small" tone="muted">
          No shared albums on this phone — so there is nothing missing from the backup, and
          nothing here to decide about. Shared Albums can also be switched off entirely under
          Settings › Photos, which looks exactly like this.
        </Text>
      )}

      {chosenIds.length > 0 && !library && (
        <Text variant="small" tone="muted">
          {formatCount(chosenIds.length)} album(s) are ticked from a previous session and will be
          backed up by the next run. Survey to see them.
        </Text>
      )}

      {library?.supported && everyAlbum && library.albums.length > 0 && (
        <>
          <AlbumPicker
            albums={everyAlbum.albums}
            chosen={chosenIds}
            onToggle={toggleAlbum}
            onChoose={chooseAlbums}
          />
          <SurveyReport survey={chosen} />
          <FetchPanel
            sample={sample}
            size={sampleSize}
            onSize={setSampleSize}
            run={fetchRun}
            fetching={fetching}
            onFetch={() => void runSamples()}
            onStop={() => {
              cancel.current = true;
            }}
          />
          <RepairPanel sample={sample} disabled={!store} />
        </>
      )}
    </Card>
  );
}

/**
 * Which shared albums are going to be imported.
 *
 * Every album is listed whether or not it is chosen, because an unlisted album
 * cannot be chosen and the point of the screen is choosing. The count beside
 * each is its own — an asset in two albums is shown under both — which is what
 * the Photos app shows and is not the number the totals below use; see
 * `summarize()`.
 */
function AlbumPicker({
  albums,
  chosen,
  onToggle,
  onChoose,
}: {
  albums: AlbumSummary[];
  chosen: string[];
  onToggle: (localId: string) => void;
  onChoose: (ids: string[]) => void;
}) {
  const isChosen = (localId: string) => chosen.includes(localId);
  const count = albums.filter((album) => isChosen(album.localId)).length;

  return (
    <>
      <Subheading
        trailing={
          <Row>
            <Pressable onPress={() => onChoose(albums.map((album) => album.localId))} hitSlop={8}>
              <Text variant="small" tone="primary">
                all
              </Text>
            </Pressable>
            <Pressable onPress={() => onChoose([])} hitSlop={8}>
              <Text variant="small" tone="primary">
                none
              </Text>
            </Pressable>
          </Row>
        }
      >
        albums to back up
      </Subheading>

      <Text variant="small" tone="muted">
        {count} of {albums.length} chosen
        {count === 0 && ' — nothing shared is backed up until one is ticked'}
      </Text>

      {albums.map((album) => (
        <Pressable
          key={album.localId}
          accessibilityRole="checkbox"
          accessibilityState={{ checked: isChosen(album.localId) }}
          style={({ pressed }) => [styles.albumRow, pressed && styles.pressed]}
          onPress={() => onToggle(album.localId)}
        >
          <Text variant="title" tone={isChosen(album.localId) ? 'primary' : 'faint'}>
            {isChosen(album.localId) ? '◉' : '○'}
          </Text>
          <View style={styles.grow}>
            <Text variant="small">{album.title ?? 'untitled album'}</Text>
            <Text variant="caption" tone="muted">
              {formatCount(album.assets)} assets · {formatCount(album.stills)} stills ·{' '}
              {formatCount(album.videos)} videos
              {album.live > 0 && ` · ${formatCount(album.live)} live`}
            </Text>
          </View>
        </Pressable>
      ))}
    </>
  );
}

/**
 * The shared-album survey, read out.
 *
 * Written to answer one question rather than to display a structure: is
 * Apple's copy of a shared photo worth archiving? Everything on screen is
 * arranged around that, and the two findings that would change the answer — a
 * still above the documented cap, a full-size resource sitting beside the
 * render — are called out in words rather than left to be spotted in a table.
 *
 * It describes the chosen albums rather than the phone. Unticking an album has
 * to change these numbers or the picker above is decoration: the question is
 * not what iCloud is holding, it is what a backup of the chosen albums would
 * fetch.
 */
function SurveyReport({ survey }: { survey: SharedSurvey }) {
  if (survey.albums.length === 0) {
    return (
      <Text variant="small" tone="muted">
        No albums chosen, so there is nothing to survey and nothing to fetch.
      </Text>
    );
  }

  const fullSize =
    (survey.resourceTypes.fullSizePhoto ?? 0) + (survey.resourceTypes.fullSizeVideo ?? 0);
  const original = survey.still.overCap > 0 || fullSize > 0;

  return (
    <>
      <Counts>
        <Count label="albums" value={formatCount(survey.albums.length)} />
        <Count label="assets" value={formatCount(survey.assets)} tone="success" />
        <Count label="videos" value={formatCount(survey.videos)} />
      </Counts>

      <Text variant="small" tone="muted">
        {formatCount(survey.stills)} stills · {formatCount(survey.videos)} videos ·{' '}
        {formatCount(survey.live)} Live Photos
        {survey.inMultipleAlbums > 0 &&
          ` · ${formatCount(survey.inMultipleAlbums)} in more than one album`}
      </Text>

      {survey.oldest !== null && survey.newest !== null && (
        <Text variant="small" tone="muted">
          taken between {new Date(survey.oldest).toISOString().slice(0, 10)} and{' '}
          {new Date(survey.newest).toISOString().slice(0, 10)}
        </Text>
      )}

      <Subheading>what sharing cost</Subheading>
      <Text variant="small" tone="muted">
        stills — longest edge {survey.still.maxLongEdge ?? '—'} px, {survey.still.atMax} of{' '}
        {survey.stills} sitting exactly there
      </Text>
      <Text variant="small" tone="muted">
        A shared still reports one pixel more than it downloads: PhotoKit says 2049 px and the
        resource arrives at 2048. The count above is the honest signal that a cap is being
        enforced — not the handful of pixels either side of it.
      </Text>
      <Text variant="small" tone="muted">
        videos — longest edge {survey.video.maxLongEdge ?? '—'} px, longest clip{' '}
        {formatDuration(survey.longestVideoSeconds)}
      </Text>

      {original ? (
        <Text variant="small" tone="success">
          {survey.still.overCap > 0
            ? `${formatCount(survey.still.overCap)} stills are above the ${STILL_LONG_EDGE_CAP}px cap`
            : `${formatCount(fullSize)} assets carry a full-size resource`}{' '}
          — Apple is not downscaling everything here, so what a backup fetched may be the
          original after all. Worth looking at one by hand before deciding.
        </Text>
      ) : (
        <Text variant="small" tone="muted">
          Nothing exceeds Apple&apos;s documented caps ({STILL_LONG_EDGE_CAP}px on a photo,{' '}
          {formatDuration(VIDEO_SECONDS_CAP)} on a video) and no full-size resource exists, so
          every one of these is Apple&apos;s re-encode — a JPEG, whatever the original was shot
          as. Backing them up archives the downscale, which is the only copy that exists for
          anything you did not share yourself.
        </Text>
      )}

      <Text variant="small" tone="muted">
        resources — {inventory(survey.resourceTypes)}
      </Text>
      <Text variant="small" tone="muted">
        sources — {inventory(survey.sourceTypes)}
      </Text>
    </>
  );
}

/**
 * The fetch: how much of it to do, doing it, and what came back.
 *
 * The size is a control rather than a constant because the two things worth
 * learning here need different amounts of it. Three assets show what a shared
 * photo weighs and how long one takes. Nothing under a few hundred shows what
 * iCloud does to a phone that asks for hundreds in a row, and that is the
 * question a backup depends on the answer to.
 */
function FetchPanel({
  sample,
  size,
  onSize,
  run,
  fetching,
  onFetch,
  onStop,
}: {
  sample: SharedAsset[];
  size: number;
  onSize: (size: number) => void;
  run: FetchRun | null;
  fetching: boolean;
  onFetch: () => void;
  onStop: () => void;
}) {
  return (
    <>
      <Subheading>fetching from iCloud</Subheading>
      <Text variant="small" tone="muted">
        A shared asset has no original on the disk, so reading one means asking iCloud for it.
        This fetches them one at a time and keeps none of the bytes — it reports the size, how
        long each took, and how it fails. It slows down when iCloud starts refusing and stops on
        its own if it keeps doing so.
      </Text>

      <Row>
        {SAMPLE_SIZES.map((option) => (
          <Pill
            key={option}
            label={String(option)}
            on={option === size}
            onPress={() => onSize(option)}
            disabled={fetching}
          />
        ))}
      </Row>

      <Row>
        <Button
          label={fetching ? 'Fetching…' : `Fetch ${sample.length} from iCloud`}
          icon="download-cloud"
          grow
          onPress={onFetch}
          busy={fetching}
          disabled={sample.length === 0}
        />
        {fetching && <Button label="Stop" variant="destructive" onPress={onStop} />}
      </Row>

      {run && <FetchProgress run={run} />}
      {run && run.results.length > 0 && <ResultRows results={run.results} />}
    </>
  );
}

/** Failures shown at most, before the list stops being a list. */
const SHOWN_FAILURES = 40;
/** Successes shown, counting back from the most recent. */
const SHOWN_SUCCESSES = 10;

/**
 * The results, thinned to what a person would actually read.
 *
 * Five hundred rows in a ScrollView that redraws several times a second is a
 * stutter, and four hundred and ninety of them say the same thing. Failures are
 * kept whatever their age, because they are the reason this panel exists; the
 * successes are kept only while they are recent, because their value is showing
 * that the run is still working and yesterday's does not.
 */
function ResultRows({ results }: { results: SampleRead[] }) {
  const recent = new Set(results.filter((r) => r.error === null).slice(-SHOWN_SUCCESSES));
  const failures = results.filter((r) => r.error !== null);
  const kept = new Set(failures.slice(-SHOWN_FAILURES));
  // Filtered in place rather than concatenated, so the rows stay in the order
  // they were fetched in and a failure keeps the successes around it.
  const shown = results.filter((r) => recent.has(r) || kept.has(r));

  return (
    <>
      {shown.length < results.length && (
        <Text variant="small" tone="muted">
          showing {shown.length} of {results.length} — every failure, and the last{' '}
          {SHOWN_SUCCESSES} that worked
        </Text>
      )}
      {shown.map((sampleRead) => (
        <SampleRow key={sampleRead.asset.localId} sample={sampleRead} />
      ))}
    </>
  );
}

/**
 * How far the run has got.
 *
 * The bar counts finished assets plus however far into the current one iCloud
 * says it is, which is the only way it moves at all during a video: one of
 * those is six seconds on its own, and a bar that advances once every six
 * seconds is a bar that looks broken. On a build with no progress event in it
 * the fraction is always zero and the bar advances per asset, which is the
 * honest version of the same picture.
 */
function FetchProgress({ run }: { run: FetchRun }) {
  const fraction = run.total === 0 ? 0 : (run.done + (run.current?.fraction ?? 0)) / run.total;

  return (
    <>
      <View style={styles.track}>
        <View style={[styles.fill, { width: `${Math.min(100, fraction * 100)}%` }]} />
      </View>

      <Text variant="small" tone="muted">
        {run.done} of {run.total} · {formatBytes(run.bytes)}
        {run.failed > 0 && ` · ${run.failed} failed`}
      </Text>

      {run.current && (
        <Text variant="caption" tone="muted" mono>
          {run.current.asset.filename ?? run.current.asset.localId}
          {run.current.attempt > 1 && ` · attempt ${run.current.attempt}`}
          {run.current.bytes > 0 && ` · ${formatBytes(run.current.bytes)}`}
          {run.current.fraction > 0 && ` · ${Math.round(run.current.fraction * 100)}%`}
        </Text>
      )}

      {/* The gap between two healthy fetches is a fraction of a second, and a
          line that appears and vanishes at that rate is worse than none. This
          shows only the waits that are a backing-off rather than a pause. */}
      {run.waitingMs >= 1_000 && (
        <Text variant="small" tone="muted">
          waiting {(run.waitingMs / 1000).toFixed(1)}s before the next attempt
        </Text>
      )}

      {run.stoppedBecause && (
        <Text variant="small" tone="warning">
          {run.stoppedBecause}
        </Text>
      )}
    </>
  );
}

function SampleRow({ sample }: { sample: SampleRead }) {
  const { asset, read, error, attempts } = sample;
  const rate = read ? throughputMbPerSecond(read.bytes, read.elapsedMs) : null;

  return (
    <View style={styles.resultRow}>
      <Text variant="small">
        {error ? '✗' : '✓'} {asset.filename ?? asset.localId}
        {attempts > 1 && (
          <Text variant="small" tone="muted">
            {' '}
            after {attempts} attempts
          </Text>
        )}
      </Text>

      {read ? (
        <Text variant="caption" tone="muted" mono>
          {read.resourceType} · {asset.pixelWidth}×{asset.pixelHeight}
          {asset.kind === 'video' && ` · ${formatDuration(asset.durationSeconds)}`} ·{' '}
          {formatBytes(read.bytes)} · {(read.elapsedMs / 1000).toFixed(1)}s
          {rate !== null && ` · ${rate.toFixed(1)} MB/s`}
        </Text>
      ) : (
        <Text variant="small" tone="destructive">
          {error}
        </Text>
      )}

      {read?.originalFilename && read.originalFilename !== asset.filename && (
        <Text variant="caption" tone="muted">
          the camera called it {read.originalFilename}
        </Text>
      )}
    </View>
  );
}

/**
 * The two things to do when a shared photograph reached the archive with
 * something missing from it.
 *
 * Both are about metadata rather than bytes, and neither re-fetches anything
 * from iCloud.
 */
function RepairPanel({ sample, disabled }: { sample: SharedAsset[]; disabled: boolean }) {
  const { forgetShared, forgotten } = useBackup();
  const [provenance, setProvenance] = useState<SharedProvenance | null>(null);
  const [provenanceError, setProvenanceError] = useState<string | null>(null);

  /**
   * Asks one photograph what this iOS will say about who shared it.
   *
   * Here rather than in a test because the answer is a property of the phone
   * this is running on. The contributor is read from keys Apple does not
   * document, and a photograph reporting none is ambiguous between having none
   * and this build not knowing what the field is called; only the phone can
   * settle that. See `sharedProvenance()` in the native module.
   */
  const probe = useCallback(async () => {
    setProvenanceError(null);
    const subject = sample[0];
    if (!subject) {
      setProvenanceError('Tick an album with something in it first.');
      return;
    }
    try {
      const found = await photoKitSharedProvenance(subject.localId);
      setProvenance(found);
      if (found === null) {
        setProvenanceError('This dev client has no provenance diagnostic in it.');
      }
    } catch (error) {
      setProvenanceError(errorText(error));
    }
  }, [sample]);

  return (
    <>
      <Subheading>fixing what was already archived</Subheading>
      <Text variant="small" tone="muted">
        A shared photograph records its album and who added it at the moment it is queued, so
        anything archived by an earlier build kept neither. This forgets what the queue remembers
        about shared items; the next run offers them again, the server answers &ldquo;already
        have it&rdquo;, and each one is described on the way past. No bytes are fetched and
        nothing is uploaded twice.
      </Text>

      <Button
        label="Re-send shared album details"
        icon="rotate-ccw"
        onPress={() => void forgetShared()}
        disabled={disabled}
      />

      {forgotten !== null && (
        <Text variant="small" tone="muted">
          {forgotten === 0
            ? 'Nothing shared was in the queue. Survey and tick an album, then run a backup.'
            : `${formatCount(forgotten)} shared item(s) forgotten. Run a backup to re-send them.`}
        </Text>
      )}

      <Text variant="small" tone="muted">
        The contributor is read from PhotoKit properties Apple does not document, so an empty
        &ldquo;added by&rdquo; can mean the photograph has no contributor or that this iOS calls
        that field something else. This asks one photograph directly.
      </Text>

      <Button label="Read provenance for one photo" icon="user" onPress={() => void probe()} />

      {provenanceError && (
        <Text variant="small" tone="warning">
          {provenanceError}
        </Text>
      )}

      {provenance && (
        <View style={styles.block}>
          <Text variant="caption" tone="muted" mono>
            {provenance.class} · {provenance.sourceTypes.names.join(', ') || 'no source type'}
          </Text>
          <Text variant="caption" tone="muted" mono>
            contributor: {provenance.contributor?.displayName ?? 'none'}
          </Text>
          <Text variant="caption" tone="muted" mono>
            looked under: {provenance.contributorKeys.join(', ')}
          </Text>
          {provenance.albums.map((album, index) => (
            <Text key={index} variant="caption" tone="muted" mono>
              album {album.title ?? 'untitled'} · owner{' '}
              {album.contributor?.displayName ?? 'none'}
            </Text>
          ))}
          {provenance.properties.map((line, index) => (
            <Text key={`p${index}`} variant="caption" tone="muted" mono>
              {line}
            </Text>
          ))}
        </View>
      )}
    </>
  );
}

/** "photo ×812, adjustmentData ×4", commonest first. */
function inventory(counts: Record<string, number>): string {
  const entries = Object.entries(counts).sort(([, a], [, b]) => b - a);
  if (entries.length === 0) return 'none reported';
  return entries.map(([name, count]) => `${name} ×${formatCount(count)}`).join(', ');
}

const styles = StyleSheet.create({
  grow: { flex: 1 },
  pressed: { opacity: 0.6 },
  albumRow: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: color.border,
    paddingVertical: space.xs + 2,
  },
  resultRow: {
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: color.border,
    paddingTop: space.xs + 2,
    gap: 2,
  },
  track: {
    height: 6,
    borderRadius: 3,
    backgroundColor: color.secondary,
    overflow: 'hidden',
    marginTop: space.sm,
  },
  fill: { height: 6, borderRadius: 3, backgroundColor: color.primary },
  block: {
    backgroundColor: color.background,
    borderRadius: radius.md,
    padding: space.sm + 2,
    marginTop: space.sm,
    gap: 2,
  },
});
