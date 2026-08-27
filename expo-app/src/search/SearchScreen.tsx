import {
  askFor,
  asks,
  chipsOf,
  forgetSearches,
  parsing,
  recentSearches,
  rememberSearch,
  requestOf,
  withoutChip,
  type Chip as QueryChip,
  type ChipKind,
} from '@photobackup/core';
import { useSearch, useSearchActions } from '@photobackup/core/react';
import { Feather } from '@expo/vector-icons';
import { router, useLocalSearchParams } from 'expo-router';
import { useCallback, useMemo, useState } from 'react';
import {
  ActivityIndicator,
  Pressable,
  ScrollView,
  StyleSheet,
  TextInput,
  View,
} from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { Grid } from '../grid';
import { useBrowsing } from '../state/browsing';
import { color, radius, space, text } from '../theme';
import { Text } from '../ui';

type Glyph = React.ComponentProps<typeof Feather>['name'];

const CHIP_ICON: Record<ChipKind, Glyph> = {
  person: 'user',
  place: 'map-pin',
  dates: 'calendar',
  tag: 'tag',
  kind: 'image',
  category: 'layers',
  favorites: 'star',
  visual: 'zap',
};

/**
 * The search screen: a question at the top, how it was read underneath, and the
 * answer in a grid.
 *
 * The grid is the gallery's own with the day headings turned off — ML_IMAGES.md
 * § 8 — because relevance is the answer to the question that was asked and
 * chronology is not. Everything else about it is unchanged: the same tiles, the
 * same zoom, the same peek, the same selection, the same viewer. It does not
 * claim the sort-and-filter control, and that is the one difference: the order
 * here *is* the ranking, and the filter is the query.
 *
 * The route's parameters are the request. `?q=` alone asks the server to read
 * the sentence; taking a chip off pushes the explicit spelling — `parse=0`
 * beside the fields that survived — which is the only way to say "and *not* the
 * date it found". A push rather than a replace, so the Back gesture undoes a
 * correction: a chip removed by mistake is the case the whole mechanism exists
 * for, and it should not cost retyping the sentence.
 */
export function SearchScreen() {
  const insets = useSafeAreaInsets();
  const params = useLocalSearchParams();

  // A fresh object every render, which is why the hook keys on its spelling
  // rather than its identity. See core's useSearch.
  const request = useMemo(() => requestOf(paramsToQuery(params)), [params]);
  const search = useSearch(request);
  const actions = useSearchActions(search);
  useBrowsing(search);

  const { query, degraded, total, ready, loading } = search;
  const typed = request.get('q') ?? '';
  const edited = !parsing(request);
  const asked = asks(request);

  const chips = useMemo(() => (query ? chipsOf(query) : []), [query]);

  const run = useCallback((sentence: string) => {
    const trimmed = sentence.trim();
    if (!trimmed) {
      router.push('/search');
      return;
    }
    rememberSearch(trimmed);
    router.push({ pathname: '/search', params: queryToParams(askFor(trimmed)) });
  }, []);

  const drop = useCallback(
    (chip: QueryChip) => {
      if (!query) return;
      router.push({ pathname: '/search', params: queryToParams(withoutChip(query, chip)) });
    },
    [query],
  );

  return (
    <View style={styles.root}>
      <View style={[styles.header, { paddingTop: insets.top + space.sm }]}>
        <View style={styles.bar}>
          <Pressable
            accessibilityRole="button"
            accessibilityLabel="Close search"
            onPress={() => router.back()}
            hitSlop={10}
            style={({ pressed }) => pressed && styles.pressed}
          >
            <Feather name="chevron-left" size={22} color={color.mutedForeground} />
          </Pressable>

          <QueryBox value={typed} onSubmit={run} />

          {!asked ? null : loading && !ready ? (
            <ActivityIndicator size="small" color={color.mutedForeground} />
          ) : (
            <Text variant="small" tone="faint">
              {total.toLocaleString()}
            </Text>
          )}
        </View>

        {chips.length > 0 || edited ? (
          <ScrollView
            horizontal
            showsHorizontalScrollIndicator={false}
            contentContainerStyle={styles.chips}
          >
            {chips.map((chip) => (
              <Chip key={chip.id} chip={chip} onRemove={() => drop(chip)} />
            ))}
            {/* Only once something has been taken off. Getting back to what the
                server made of the sentence is otherwise unreachable: the
                explicit spelling has replaced the reading it came from, and
                retyping is not the same thing as undoing. */}
            {edited ? (
              <Pressable
                accessibilityRole="button"
                onPress={() => run(typed)}
                style={({ pressed }) => [styles.reset, pressed && styles.pressed]}
              >
                <Feather name="rotate-ccw" size={13} color={color.mutedForeground} />
                <Text variant="small" tone="muted">
                  Reset
                </Text>
              </Pressable>
            ) : null}
          </ScrollView>
        ) : null}

        {/* What this search could not do. A note rather than an error: the
            answer below it is real, it was just ranked by words alone. */}
        {degraded ? (
          <Text variant="small" tone="warning" style={styles.degraded}>
            {degraded}
          </Text>
        ) : null}
      </View>

      {asked ? (
        <Grid
          timeline={search}
          actions={actions}
          sortable={false}
          immersive={false}
          empty={
            chips.length > 0
              ? 'Nothing matches all of that. Try taking a chip off.'
              : `Nothing in the archive matches “${typed}”.`
          }
        />
      ) : (
        <Blank onPick={run} />
      )}
    </View>
  );
}

/**
 * The question, editable in place.
 *
 * Held locally and pushed on submit rather than on every keystroke, because
 * committing here is a navigation: it replaces the ranking, drops the
 * selection, and lands in the stack. The browser has a command palette for
 * asking one *live*; a phone has a keyboard that covers half the screen, so
 * this is the only box there is and it commits when the return key says so.
 */
function QueryBox({ value, onSubmit }: { value: string; onSubmit: (text: string) => void }) {
  const [held, setHeld] = useState(value);
  // The route can change without this box being what changed it — a chip
  // removed, the Back gesture — and when it does, what is in it is out of date.
  const [shown, setShown] = useState(value);
  if (shown !== value) {
    setShown(value);
    setHeld(value);
  }

  return (
    <View style={styles.box}>
      <Feather name="search" size={15} color={color.mutedForeground} />
      <TextInput
        value={held}
        onChangeText={setHeld}
        onSubmitEditing={() => onSubmit(held)}
        placeholder="Search your photos…"
        placeholderTextColor={color.faint}
        autoFocus={value === ''}
        autoCorrect={false}
        autoCapitalize="none"
        returnKeyType="search"
        style={styles.input}
      />
      {held ? (
        <Pressable
          accessibilityRole="button"
          accessibilityLabel="Clear the search"
          hitSlop={8}
          onPress={() => {
            setHeld('');
            onSubmit('');
          }}
        >
          <Feather name="x" size={15} color={color.mutedForeground} />
        </Pressable>
      ) : null}
    </View>
  );
}

/**
 * One thing the server understood, and the × that says it understood wrong.
 *
 * The phrase is drawn apart from the rest because it is not a filter: every
 * other chip narrows the answer and this one *is* the question — the words that
 * went to the encoder, which are not the words that were typed. Seeing that
 * "photos of my dog at the beach" became "dog at the beach" is how somebody
 * finds out why the ocean came back.
 */
function Chip({ chip, onRemove }: { chip: QueryChip; onRemove: () => void }) {
  const icon: Glyph =
    chip.kind === 'kind' && chip.label === 'Videos' ? 'video' : CHIP_ICON[chip.kind];

  return (
    <View style={[styles.chip, chip.fuzzy && styles.fuzzy]}>
      <Feather name={icon} size={13} color={chip.fuzzy ? color.primary : color.mutedForeground} />
      <Text variant="small" numberOfLines={1} style={styles.chipLabel}>
        {chip.label}
      </Text>
      <Pressable
        accessibilityRole="button"
        accessibilityLabel={`Search without ${chip.label}`}
        onPress={onRemove}
        hitSlop={8}
      >
        <Feather name="x" size={13} color={color.mutedForeground} />
      </Pressable>
    </View>
  );
}

/** The screen before anything has been asked of it. */
function Blank({ onPick }: { onPick: (text: string) => void }) {
  // Read once, on the way in. The list only changes when this screen pushes a
  // new search, and pushing one replaces what is on top of it anyway.
  const [recent, setRecent] = useState(() => recentSearches());

  return (
    <ScrollView contentContainerStyle={styles.blank} keyboardShouldPersistTaps="handled">
      <Text variant="small" tone="muted" style={styles.centred}>
        Search by what is in a photograph, not what it was called.
      </Text>
      <Text variant="caption" tone="faint" style={styles.centred}>
        A place, a person, a year, or just what it looked like — “snow”,
        “Phoenix at the beach last summer”, “that blue error screenshot”.
      </Text>

      {recent.length > 0 ? (
        <View style={styles.recents}>
          <View style={styles.recentsHead}>
            <Text variant="caption" tone="faint" style={styles.grow}>
              RECENT
            </Text>
            <Pressable
              accessibilityRole="button"
              onPress={() => {
                forgetSearches();
                setRecent([]);
              }}
              hitSlop={8}
            >
              <Text variant="caption" tone="muted">
                Clear
              </Text>
            </Pressable>
          </View>

          {recent.map((sentence) => (
            <Pressable
              key={sentence}
              accessibilityRole="button"
              onPress={() => onPick(sentence)}
              style={({ pressed }) => [styles.recent, pressed && styles.pressed]}
            >
              <Feather name="clock" size={15} color={color.faint} />
              <Text variant="body" numberOfLines={1} style={styles.grow}>
                {sentence}
              </Text>
            </Pressable>
          ))}
        </View>
      ) : null}
    </ScrollView>
  );
}

/**
 * The route's parameters as a query string, and back.
 *
 * The browser has one spelling for both — the page's query string *is* the
 * API's — and expo-router has its own object shape, so the two conversions live
 * here and core's `requestOf` picks the request out of the result exactly as it
 * does in the browser. Repeated keys survive both ways: a query can name two
 * people, and each × has to be able to take one of them off.
 */
function paramsToQuery(params: Record<string, string | string[] | undefined>): URLSearchParams {
  const out = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined) continue;
    if (Array.isArray(value)) for (const one of value) out.append(key, one);
    else out.append(key, value);
  }
  return out;
}

function queryToParams(query: URLSearchParams): Record<string, string | string[]> {
  const out: Record<string, string | string[]> = {};
  for (const [key, value] of query.entries()) {
    const held = out[key];
    if (held === undefined) out[key] = value;
    else if (Array.isArray(held)) held.push(value);
    else out[key] = [held, value];
  }
  return out;
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: color.background },
  header: {
    gap: space.sm,
    paddingHorizontal: space.md,
    paddingBottom: space.sm,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: color.border,
  },
  bar: { flexDirection: 'row', alignItems: 'center', gap: space.sm },
  box: {
    flex: 1,
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    height: 38,
    paddingHorizontal: space.md,
    borderRadius: radius.md,
    borderWidth: 1,
    borderColor: color.input,
    backgroundColor: color.secondary,
  },
  input: { flex: 1, color: color.foreground, padding: 0, ...text.body },

  chips: { gap: space.sm, alignItems: 'center', paddingRight: space.md },
  chip: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    maxWidth: 240,
    paddingLeft: space.md,
    paddingRight: space.sm,
    paddingVertical: 5,
    borderRadius: radius.pill,
    borderWidth: 1,
    borderColor: color.border,
    backgroundColor: color.secondary,
  },
  fuzzy: { borderColor: color.primary, backgroundColor: color.accent },
  chipLabel: { flexShrink: 1 },
  reset: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.xs,
    paddingHorizontal: space.md,
    paddingVertical: 5,
  },
  degraded: { paddingTop: space.xs },

  blank: { padding: space.xl, gap: space.md },
  centred: { textAlign: 'center' },
  recents: { marginTop: space.xl, gap: space.sm },
  recentsHead: { flexDirection: 'row', alignItems: 'center', paddingHorizontal: space.sm },
  recent: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.md,
    minHeight: 44,
    paddingHorizontal: space.md,
    borderRadius: radius.lg,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: color.border,
    backgroundColor: color.card,
  },
  grow: { flex: 1 },
  pressed: { opacity: 0.7 },
});
