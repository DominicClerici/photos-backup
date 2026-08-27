import { BASE_THUMB_SIZE, type Category } from '@photobackup/core';
import { Feather } from '@expo/vector-icons';
import { StyleSheet, View } from 'react-native';

import { color, radius } from '../theme';
import { ListRow, ROW_ICON_SIZE, RowList } from '../ui';
import { Cover } from './Cover';

type Glyph = React.ComponentProps<typeof Feather>['name'];

/**
 * What each category key is called and drawn as.
 *
 * The browser's `LOOK`, with lucide's names swapped for Feather's — the icon
 * set is a rendering decision and this is the file that makes it, which is why
 * the two apps can disagree about the glyph and agree about everything else.
 *
 * The server decides which categories exist and sends only their keys, because
 * what a category *is* is a predicate over the library and belongs beside the
 * query. What it is called is a UI decision and belongs here — and a key with
 * no entry falls back to something legible rather than rendering nothing, so
 * adding a category to the server does not require a matching release here.
 */
const LOOK: Record<string, { label: string; icon: Glyph }> = {
  videos: { label: 'Videos', icon: 'video' },
  favorites: { label: 'Favorites', icon: 'heart' },
  live: { label: 'Live Photos', icon: 'aperture' },
  screenshots: { label: 'Screenshots', icon: 'smartphone' },
  panoramas: { label: 'Panoramas', icon: 'crop' },
  timelapse: { label: 'Time-lapse', icon: 'clock' },
  cinematic: { label: 'Cinematic', icon: 'film' },
  hdr: { label: 'HDR', icon: 'sun' },
  // Drawn under Other rather than in this list — see OtherList — but its screen
  // is still /collections/categories/archived, so the label still lives here.
  archived: { label: 'Archive', icon: 'archive' },
};

function look(key: string) {
  return LOOK[key] ?? { label: key, icon: 'layers' as Glyph };
}

/** The label a category's own screen puts in its heading. */
export const categoryLabel = (key: string) => look(key).label;

/**
 * The named slices of a scope, as rows rather than as a grid.
 *
 * @param onOpen where a row leads. The library's categories go to
 * `/collections/categories/…` and a bucket's to `/archive/categories/…` or
 * `/hidden/categories/…`, over exactly the same keys — a hidden screenshot is a
 * screenshot. Which is why this takes a callback rather than having grown a
 * second component for the vault.
 */
export function CategoryList({
  categories,
  onOpen,
  sealed = false,
}: {
  categories: Category[];
  onOpen: (key: string) => void;
  /** Whether these are a bucket's categories, whose covers are decrypted. */
  sealed?: boolean;
}) {
  return (
    <RowList>
      {categories.map((category) => {
        const { label, icon } = look(category.key);
        return (
          <ListRow
            key={category.key}
            label={label}
            value={category.count.toLocaleString()}
            onPress={() => onOpen(category.key)}
            leading={
              <View style={styles.badge}>
                <Cover
                  id={category.cover_id}
                  size={BASE_THUMB_SIZE}
                  sealed={sealed}
                  style={styles.cover}
                />
                {/* The glyph is what makes the row scannable; the photograph
                    behind it is what makes the list look like the library
                    rather than a settings screen. Dimming it keeps both
                    readable. */}
                <View style={[StyleSheet.absoluteFill, styles.scrim]}>
                  <Feather name={icon} size={18} color={color.foreground} />
                </View>
              </View>
            }
          />
        );
      })}
    </RowList>
  );
}

const styles = StyleSheet.create({
  badge: { width: ROW_ICON_SIZE, height: ROW_ICON_SIZE },
  cover: { width: ROW_ICON_SIZE, height: ROW_ICON_SIZE, borderRadius: radius.lg },
  scrim: {
    alignItems: 'center',
    justifyContent: 'center',
    borderRadius: radius.lg,
    backgroundColor: 'rgba(11,11,13,0.55)',
  },
});
