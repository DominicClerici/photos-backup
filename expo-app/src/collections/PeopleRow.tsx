import { BASE_THUMB_SIZE, type Person } from '@photobackup/core';
import { Pressable, ScrollView, StyleSheet } from 'react-native';

import { space } from '../theme';
import { Text } from '../ui';
import { Cover } from './Cover';

/** The circles, and the width a name is allowed to be under one. */
const SIZE = 74;

/**
 * The tagged names, most photographed first.
 *
 * The circles are photographs someone appears in, not faces: nothing in this
 * archive knows where in a frame a person is, so cropping to one would be
 * guessing. It reads as a face row anyway at this size, and it stops being a
 * lie the day the v2 face work gives it something better to draw.
 *
 * There is no menu on a circle here, where the browser has one. A person's menu
 * is Archive and Hide and nothing else — a person is a label an import carried,
 * not a thing this archive owns, so "delete Brody" has no coherent meaning
 * short of deleting every photograph of them — and both of those are behind the
 * vault gate that arrives in Phase 6. A menu whose every item was inert would
 * be worse than the absence of one, so the absence is what this has until there
 * is something live to put in it.
 */
export function PeopleRow({
  people,
  onOpen,
}: {
  people: Person[];
  onOpen: (person: Person) => void;
}) {
  return (
    <ScrollView
      horizontal
      showsHorizontalScrollIndicator={false}
      contentContainerStyle={styles.row}
    >
      {people.map((person) => (
        <Pressable
          key={person.name}
          accessibilityRole="button"
          accessibilityLabel={person.name}
          onPress={() => onOpen(person)}
          style={({ pressed }) => [styles.person, pressed && styles.pressed]}
        >
          <Cover
            id={person.cover_id}
            size={BASE_THUMB_SIZE}
            round
            style={styles.circle}
          />
          <Text variant="small" tone="muted" numberOfLines={1} style={styles.name}>
            {person.name}
          </Text>
        </Pressable>
      ))}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  row: { gap: space.md, paddingRight: space.lg },
  person: { width: SIZE, alignItems: 'center', gap: space.sm },
  circle: { width: SIZE, height: SIZE, borderRadius: 999 },
  name: { textAlign: 'center' },
  pressed: { opacity: 0.75 },
});
