import {
  ApiError,
  createAlbum,
  type Bucket,
  type CreatedAlbum,
  type Target,
} from '@photobackup/core';
import { albumsChanged } from '@photobackup/core/react';
import { useCallback, useEffect, useState } from 'react';
import { StyleSheet, View } from 'react-native';

import { space } from '../theme';
import { Button, Field, Sheet, Text } from '../ui';

/** The one sheet every "Create album" in the app opens. */
export interface CreateAlbumRequest {
  /**
   * What the name box starts with. The album picker hands over whatever was
   * typed into its search box, so "Create 'Iceland'" opens on a form that
   * already says Iceland and needs one keystroke to finish.
   */
  name: string;
  /** Which half of the archive the album is made in. */
  bucket?: Bucket;
  /**
   * What to put in it, when this was opened from a selection. Captured at the
   * moment the row was tapped rather than read on submit: by the time somebody
   * has typed a name the selection may be gone, and every position in it would
   * mean a different photograph anyway.
   */
  target?: Target;
}

/**
 * Making an album, as the only surface in the app that asks for a name.
 *
 * Two fields and nothing else, because an album is two fields. The description
 * is optional and rarely used — an import fills it from a Takeout's per-folder
 * metadata and most people never type one — so it is here rather than hidden
 * behind a disclosure, and simply left empty.
 *
 * @param request What to make, or null while the sheet is shut. One object
 * rather than an `open` flag beside three props, so a sheet that is open cannot
 * be open about nothing.
 * @param onCreated Called with the album once it exists. The caller decides
 * what that means — the collections screen opens it, a selection stays put and
 * says what went in.
 */
export function CreateAlbumSheet({
  request,
  onClose,
  onCreated,
}: {
  request: CreateAlbumRequest | null;
  onClose: () => void;
  onCreated: (album: CreatedAlbum) => void;
}) {
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  // Reset on each open rather than on close, so the fields do not visibly empty
  // themselves while the sheet is sliding back down.
  useEffect(() => {
    if (!request) return;
    setTitle(request.name);
    setDescription('');
    setError(null);
    setSaving(false);
  }, [request]);

  const submit = useCallback(() => {
    if (!request || saving) return;

    const name = title.trim();
    if (!name) {
      setError('An album needs a name.');
      return;
    }

    setSaving(true);
    setError(null);
    createAlbum(
      { title: name, description: description.trim(), bucket: request.bucket },
      request.target,
    )
      .then((album) => {
        albumsChanged();
        onClose();
        onCreated(album);
      })
      .catch((err: unknown) => {
        setSaving(false);
        // 409 is the archive saying the name is taken, which belongs under the
        // field somebody typed it into rather than in a notice across the
        // screen from it.
        if (err instanceof ApiError && err.status === 409) {
          setError('An album with that name already exists.');
          return;
        }
        setError(err instanceof Error ? err.message : 'Could not create the album.');
      });
  }, [request, saving, title, description, onClose, onCreated]);

  // Whether anything is going in with it, which is the whole difference between
  // the two sentences under the heading.
  const filling = request?.target !== undefined;

  return (
    <Sheet open={request !== null} onClose={onClose} title="New album">
      <Text variant="small" tone="muted">
        {filling
          ? 'The photos you selected go in as soon as it exists.'
          : 'It starts empty. Add photos to it from any grid.'}
      </Text>

      <View style={styles.form}>
        <Field
          value={title}
          onChangeText={(next) => {
            setTitle(next);
            setError(null);
          }}
          placeholder="Iceland 2026"
          autoFocus
          autoCorrect={false}
          maxLength={200}
          returnKeyType="done"
          onSubmitEditing={submit}
        />
        {error ? (
          <Text variant="small" tone="destructive">
            {error}
          </Text>
        ) : null}

        <Field
          value={description}
          onChangeText={setDescription}
          placeholder="Description (optional)"
          maxLength={2000}
          multiline
        />
      </View>

      <View style={styles.buttons}>
        <Button label="Cancel" onPress={onClose} variant="ghost" disabled={saving} grow />
        <Button
          label="Create album"
          onPress={submit}
          variant="primary"
          busy={saving}
          disabled={title.trim() === ''}
          grow
        />
      </View>
    </Sheet>
  );
}

const styles = StyleSheet.create({
  form: { gap: space.sm },
  buttons: { flexDirection: 'row', gap: space.sm },
});
