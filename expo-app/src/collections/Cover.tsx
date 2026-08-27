import { BASE_THUMB_SIZE, media, thumbVariant, type ThumbSize } from '@photobackup/core';
import { Feather } from '@expo/vector-icons';
import { Image } from 'expo-image';
import { useState } from 'react';
import { StyleSheet, View, type StyleProp, type ViewStyle } from 'react-native';

import { color } from '../theme';

/**
 * The single thumbnail that stands for a collection.
 *
 * The browser's `Cover`, unchanged in what it decides and different only in
 * what draws it: `expo-image` takes the `{ uri, headers }` that `media()`
 * returns whole, where an `<img src>` takes the uri and ignores the rest. That
 * one line is the entire difference between the two clients — WEB_TO_MOBILE
 * § 3.2 — and it is the same one the tiles already make.
 *
 * Unlike a tile this has no state to poll and no motion to play: it is one
 * image, chosen by the server, and the worst it does is not exist yet. The
 * larger sizes are the ones a library ingested before they existed does not
 * have until a backfill runs, so a miss falls back to the base size — the only
 * one every asset is guaranteed to have — before giving up on a placeholder.
 */
export function Cover({
  id,
  size = 512,
  style,
  round = false,
}: {
  /** The asset to draw, or nothing — an empty collection has no cover. */
  id?: string;
  /** Which stored rendition to ask for. Falls back to the base size on a 404. */
  size?: ThumbSize;
  style?: StyleProp<ViewStyle>;
  /** A person is a circle; everything else is a rounded square. */
  round?: boolean;
}) {
  const [fallback, setFallback] = useState(false);
  const [broken, setBroken] = useState(false);

  if (!id || broken) {
    return (
      <View style={[styles.box, style, styles.blank]}>
        <Feather name="image" size={18} color={color.faint} />
      </View>
    );
  }

  return (
    <View style={[styles.box, style]}>
      <Image
        style={StyleSheet.absoluteFill}
        source={media(id, thumbVariant(fallback ? BASE_THUMB_SIZE : size))}
        contentFit="cover"
        cachePolicy="memory-disk"
        transition={120}
        recyclingKey={`${id}#${fallback ? BASE_THUMB_SIZE : size}`}
        onError={() => (fallback ? setBroken(true) : setFallback(true))}
        accessibilityIgnoresInvertColors
      />
      {/* A circle clipped by `overflow` alone leaves a hard square corner on
          Android; the ring is drawn over the picture so both platforms agree. */}
      {round ? <View style={[StyleSheet.absoluteFill, styles.ring]} pointerEvents="none" /> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  box: { overflow: 'hidden', backgroundColor: color.tile },
  blank: { alignItems: 'center', justifyContent: 'center' },
  ring: { borderWidth: StyleSheet.hairlineWidth, borderColor: color.border, borderRadius: 999 },
});
