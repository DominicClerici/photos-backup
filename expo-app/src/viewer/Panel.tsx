import {
  formatBytes,
  formatCaptureTime,
  formatCoords,
  formatDuration,
  mapLink,
  type AnalysisTag,
  type AssetAnalysis,
  type AssetDetail,
} from '@photobackup/core';
import { Linking, Pressable, StyleSheet, View } from 'react-native';

import { color, radius, space } from '../theme';
import { Text } from '../ui';

/**
 * What the archive knows about one photograph.
 *
 * A port of the browser's `MetadataPanel`, field for field and note for note.
 * Everything it decides — when a reported time is worth showing, how a place
 * name and its coordinates sit together, why a caption is missing — is the same
 * decision, because it is the same archive answering and there is no reason for
 * the phone to have a second opinion about it. What changed is the drawing:
 * a `<dl>` became rows, `<Badge>` became a chip, and the sidebar became the
 * body of a `Sheet`.
 */
export function Panel({
  detail,
  analysis,
}: {
  detail: AssetDetail | null;
  /** Null while it is still being fetched, and after a fetch that failed. */
  analysis: AssetAnalysis | null;
}) {
  if (!detail) {
    return (
      <Text variant="small" tone="faint">
        Loading details…
      </Text>
    );
  }

  const capture = formatCaptureTime(detail.taken_at, detail.offset_minutes);
  const camera = [detail.camera_make, detail.camera_model].filter(Boolean).join(' ');
  // City, state, country, dropping whatever the geocoder had no answer for — an
  // island nation has no first-order division, and a photograph over open water
  // has none of the three.
  const place = [detail.place_city, detail.place_admin1, detail.place_country]
    .filter(Boolean)
    .join(', ');
  const reported = detail.reported_at ? new Date(detail.reported_at) : null;
  const taken = new Date(detail.taken_at);
  // Worth showing only when the two disagree: if the phone and the file tell the
  // same story, repeating it is noise.
  const disagrees =
    reported != null && Math.abs(reported.getTime() - taken.getTime()) > 60_000;

  return (
    <>
      <Analysis
        analysis={analysis}
        people={detail.people}
        description={detail.description}
      />

      <View style={styles.rows}>
        <Row label="Taken" hint={capture.zone}>
          {capture.text}
        </Row>

        {disagrees && reported ? (
          <Row label="Phone reported" hint="differs from the file’s own time">
            {reported.toLocaleString()}
          </Row>
        ) : null}

        {detail.width && detail.height ? (
          <Row label="Dimensions">{`${detail.width} × ${detail.height}`}</Row>
        ) : null}

        {detail.duration ? (
          <Row label="Duration">{formatDuration(detail.duration)}</Row>
        ) : null}

        {detail.contributor ? (
          <Row label="Added by" hint="from a shared album">
            {detail.contributor}
          </Row>
        ) : null}

        {camera ? <Row label="Camera">{camera}</Row> : null}
        {detail.lens ? <Row label="Lens">{detail.lens}</Row> : null}

        {detail.gps_lat != null && detail.gps_lon != null ? (
          <Row
            label="Location"
            // The coordinates stay, underneath, when there is a name for them.
            // The name is what the photograph is of; the numbers are what the
            // camera recorded, and dropping them would hide a bad fix behind a
            // plausible-looking city.
            hint={place ? formatCoords(detail.gps_lat, detail.gps_lon) : undefined}
          >
            <Pressable
              accessibilityRole="link"
              onPress={() => {
                void Linking.openURL(mapLink(detail.gps_lat!, detail.gps_lon!));
              }}
              hitSlop={6}
            >
              <Text variant="small" tone="primary">
                {place || formatCoords(detail.gps_lat, detail.gps_lon)}
              </Text>
            </Pressable>
          </Row>
        ) : null}

        <Row label="Size">{formatBytes(detail.byte_size)}</Row>
        <Row label="Uploaded">{new Date(detail.uploaded_at).toLocaleString()}</Row>
        <Row label="SHA-256">
          <Text variant="caption" tone="muted" mono>
            {`${detail.sha256.slice(0, 16)}…`}
          </Text>
        </Row>
      </View>
    </>
  );
}

/**
 * The four passes, in the order a photograph goes through them, and what to
 * call each one to somebody who is not holding ML_IMAGES.md.
 *
 * `mlprep` is in here even though it is not ML at all, because it is the one
 * whose failure explains all three of the others: no rendition means nothing
 * ever looked at the photograph, and three separate "not captioned yet" lines
 * would be three symptoms of one cause.
 */
const PASSES = ['mlprep', 'vision', 'ocr', 'describe'] as const;

const PASS_LABEL: Record<string, string> = {
  mlprep: 'the rendition',
  vision: 'the encoder',
  ocr: 'the text recogniser',
  describe: 'the captioner',
};

/**
 * What the models have said about this photograph — and, where they have said
 * nothing, which of the two reasons that is.
 *
 * Nothing here is drawn as blank. A photograph with no caption is one the
 * captioner has not reached, or one it failed on, or one nothing has queued, and
 * a panel that drew all three the same way would be ML_IMAGES.md § 11's silent
 * exclusion with a nicer font. See `notes`.
 *
 * The tags and the names are labels here rather than the browser's buttons.
 * There they lead to `/search?q=…`, which is the question a tag actually asks —
 * "what else did it call a beach" — and there is no search route on the phone
 * until Phase 5. They become links there, where there is somewhere to land.
 */
function Analysis({
  analysis,
  people,
  description,
}: {
  analysis: AssetAnalysis | null;
  /** Names an import carried, which are not the model's and never become tags. */
  people?: string[];
  /** What a person typed under the photograph, before this archive existed. */
  description?: string;
}) {
  const tags = analysis?.tags ?? [];
  const said = Boolean(analysis?.caption || tags.length || analysis?.text);
  const pending = notes(analysis);

  // A Live Photo's video half, a Snapchat overlay layer, anything in the vault:
  // the ML pass skips these by construction rather than by a queue that has not
  // got to them. There is no story to tell about a photograph nothing was ever
  // going to look at.
  if (analysis && !said && !description && !people?.length && pending.length === 0) {
    return null;
  }

  return (
    <View style={styles.analysis}>
      {/* Only where there is a sentence to head, or one still coming. A heading
          over nothing is the blank this exists not to draw — the note at the
          foot says why there is no caption, and says it in words. */}
      {analysis == null || analysis.caption ? (
        <View>
          <Label>Analysis</Label>
          {analysis == null ? (
            <Text variant="small" tone="faint">
              Reading what the models said…
            </Text>
          ) : (
            <Text variant="small">{analysis.caption}</Text>
          )}
          {analysis?.caption_model ? <Hint>{analysis.caption_model}</Hint> : null}
        </View>
      ) : null}

      {tags.length ? (
        <View>
          <Label>Tags</Label>
          <View style={styles.chips}>
            {tags.map((tag) => (
              <Chip key={tag.name + tag.raw} tag={tag} />
            ))}
          </View>
        </View>
      ) : null}

      {people?.length ? (
        <View>
          <Label>People</Label>
          <View style={styles.chips}>
            {people.map((name) => (
              <View key={name} style={[styles.chip, styles.outlined]}>
                <Text variant="caption">{name}</Text>
              </View>
            ))}
          </View>
          {/* The seam ML_IMAGES.md § 11 asks to keep visible: a name somebody
              confirmed is a different kind of claim from a word a model wrote,
              and the panel should not be the place the two quietly become one
              list of things "in" the photograph. */}
          <Hint>named when this was imported</Hint>
        </View>
      ) : null}

      {description ? (
        <View>
          <Label>Description</Label>
          <Text variant="small">{description}</Text>
          <Hint>written by a person, not a model</Hint>
        </View>
      ) : null}

      {analysis?.text ? (
        <View>
          <Label>Recognised text</Label>
          {/* The whole of it. A search shows the matching line; somebody who
              opened this panel on a receipt wants the receipt — and the sheet
              this sits in scrolls, so there is nowhere for it to overflow to. */}
          <View style={styles.recognised}>
            <Text variant="caption" tone="muted" mono>
              {analysis.text}
            </Text>
          </View>
          {analysis.text_model ? <Hint>{analysis.text_model}</Hint> : null}
        </View>
      ) : null}

      {pending.length ? (
        <View style={styles.pending}>
          {pending.map((note) => (
            <Text key={note} variant="caption" tone="faint">
              {note}
            </Text>
          ))}
        </View>
      ) : null}

      {analysis?.frames ? (
        <Text variant="caption" tone="faint">
          {`Findable by what it looks like · ${
            analysis.frames === 1 ? 'one frame' : `${analysis.frames} frames`
          }${analysis.vision_model ? ` · ${analysis.vision_model}` : ''}`}
        </Text>
      ) : null}
    </View>
  );
}

/**
 * One word a search resolves this photograph to, and what the model actually
 * wrote where a merge has folded the two together.
 *
 * The browser hides that behind an asterisk and a tooltip. There are no
 * tooltips on a phone, so it is spelled out — seeing that a photograph was
 * called "puppy" and is findable as "dog" is the whole reason the field exists,
 * and a mark nothing can explain is worse than the extra word.
 */
function Chip({ tag }: { tag: AnalysisTag }) {
  return (
    <View style={styles.chip}>
      <Text variant="caption">{tag.name}</Text>
      {tag.raw ? (
        <Text variant="caption" tone="faint">
          {` ← ${tag.raw}`}
        </Text>
      ) : null}
    </View>
  );
}

/**
 * Why a pass has said nothing, for each pass that has said nothing.
 *
 * Empty on a photograph that has been all the way through, which is the
 * majority and should cost no lines in the panel. Everything else here is the
 * difference between "there is nothing in this photograph" and "nothing has
 * looked at this photograph", which are the two readings of an empty panel and
 * are not remotely the same news.
 */
function notes(analysis: AssetAnalysis | null): string[] {
  if (!analysis) return [];
  const jobs = analysis.jobs ?? {};

  // A missing rendition is the cause of everything downstream, so it is said
  // once and the three symptoms are left unsaid.
  if (jobs.mlprep === 'failed') {
    return ['No ML rendition could be made, so nothing has looked at this.'];
  }

  const out: string[] = [];
  for (const pass of PASSES) {
    if (pass === 'mlprep') continue;
    const produced =
      pass === 'vision'
        ? Boolean(analysis.frames)
        : pass === 'ocr'
          ? analysis.text != null
          : analysis.caption != null;
    const state = jobs[pass];

    if (state === 'done') {
      // Ran, and found nothing. Worth saying for the recogniser — a photograph
      // with no text in it is the ordinary case and looks identical to one
      // nobody has read — and worth saying for the captioner, where it means
      // the model returned an empty sentence.
      if (!produced) {
        out.push(
          pass === 'ocr'
            ? 'No text was found in this photograph.'
            : `Nothing came back from ${PASS_LABEL[pass]}.`,
        );
      }
      continue;
    }
    if (produced) continue;

    switch (state) {
      case 'failed':
        out.push(`${sentence(PASS_LABEL[pass])} failed on this photograph.`);
        break;
      case 'running':
        out.push(`${sentence(PASS_LABEL[pass])} is looking at this now.`);
        break;
      case 'pending':
        out.push(`Queued for ${PASS_LABEL[pass]}.`);
        break;
      default:
        // No row at all. The ordinary state of a library the captioning
        // backfill has not been run over — it is a typed command rather than
        // something a restart begins, so this is a fact about the archive
        // rather than a fault in it.
        out.push(`${sentence(PASS_LABEL[pass])} has not been run over this.`);
    }
  }
  return out;
}

function sentence(text: string): string {
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function Label({ children }: { children: string }) {
  return <Text style={styles.label}>{children}</Text>;
}

function Hint({ children }: { children: string }) {
  return (
    <Text variant="caption" tone="faint" style={styles.hint}>
      {children}
    </Text>
  );
}

function Row({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <View>
      <Label>{label}</Label>
      {typeof children === 'string' ? <Text variant="small">{children}</Text> : children}
      {hint ? <Hint>{hint}</Hint> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  analysis: {
    gap: space.md,
    paddingBottom: space.lg,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: color.border,
  },
  rows: { gap: space.md },
  label: {
    marginBottom: 3,
    color: color.faint,
    fontSize: 11,
    letterSpacing: 1,
    textTransform: 'uppercase',
  },
  hint: { marginTop: 2 },
  chips: { flexDirection: 'row', flexWrap: 'wrap', gap: space.xs + 2 },
  chip: {
    flexDirection: 'row',
    alignItems: 'center',
    paddingHorizontal: space.sm,
    paddingVertical: 3,
    borderRadius: radius.sm,
    backgroundColor: color.secondary,
  },
  outlined: {
    backgroundColor: 'transparent',
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
  },
  recognised: {
    borderRadius: radius.md,
    backgroundColor: color.muted,
    paddingHorizontal: space.sm + 2,
    paddingVertical: space.sm,
  },
  pending: { gap: 3 },
});
