import { closeGate, onGate, useVault } from '@photobackup/core/react';
import { useCallback, useEffect, useState } from 'react';
import { StyleSheet, View } from 'react-native';

import { space } from '../theme';
import { Button, Field, Sheet, Text } from '../ui';

/** See the note on `useVault` below: this prompt has no lock state to keep up with. */
const POLL_MS = 15 * 60 * 1000;

/**
 * The password prompt, mounted once by the root layout.
 *
 * It is here rather than on the Archive screen for the reason the browser's
 * dialog is in its root layout: archiving a photograph on an archive that has
 * never had a vault has to be able to ask for a password from the middle of the
 * library's grid, and opening a locked bucket has to be able to ask from a
 * screen that has drawn nothing. One prompt, opened from anywhere, is the only
 * shape that serves both without a callback threaded through every component in
 * between. Core's `onGate` is the subscription — the same module-level store
 * the browser reads, unchanged.
 *
 * Two modes, because they are two different conversations. Unlocking is "prove
 * you are you"; creating is "choose the thing that will be the difference
 * between these photographs existing and not", and it says so.
 *
 * A sheet rather than a dialog, which is what every other "answer this before
 * continuing" in this app already is. It also puts the field directly above the
 * keyboard, which for a password typed one character at a time under a thumb is
 * the whole difference between the two shapes.
 */
export function VaultGate() {
  const [reason, setReason] = useState<'unlock' | 'setup' | null>(null);
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState<string | null>(null);
  /**
   * Deliberately barely polled.
   *
   * `useVault`'s poll exists so that a screen showing the lock state notices the
   * key being dropped after fifteen idle minutes. This component shows no lock
   * state — it spends a password and closes — and it is mounted for the whole
   * life of the app, so the default half-minute poll would be a network wakeup
   * every thirty seconds forever, on a phone, to answer a question nothing here
   * asks. The screens that *do* draw the lock keep the default; see BucketView.
   */
  const vault = useVault(POLL_MS);

  useEffect(() => onGate(setReason), []);

  // Nothing typed survives the prompt closing. It is a password, and the one
  // place it is allowed to live is the request that spends it.
  useEffect(() => {
    if (reason === null) {
      setPassword('');
      setConfirm('');
      setFailed(null);
    }
  }, [reason]);

  const setup = reason === 'setup';

  const submit = useCallback(async () => {
    setFailed(null);
    if (setup && password !== confirm) {
      setFailed('Those do not match.');
      return;
    }
    setBusy(true);
    try {
      if (setup) await vault.create(password);
      else await vault.unlock(password);
    } catch (err) {
      setFailed(err instanceof Error ? err.message : 'That did not work.');
    } finally {
      setBusy(false);
    }
  }, [setup, password, confirm, vault]);

  const ready = setup ? password.length >= 8 && confirm.length > 0 : password.length > 0;

  return (
    <Sheet
      open={reason !== null}
      onClose={closeGate}
      title={setup ? 'Choose a vault password' : 'Unlock the vault'}
    >
      <Text variant="small" tone="muted">
        {setup
          ? 'Archive and Hidden are encrypted with this password. There is no recovery: if you forget it, those photos are gone for good.'
          : 'Archive and Hidden are encrypted. The password decrypts them for the next fifteen minutes.'}
      </Text>

      <View style={styles.form}>
        <Field
          // Two different fields as far as a password manager is concerned, so
          // it offers to save on the first and to fill on the second rather
          // than doing the wrong one of those.
          textContentType={setup ? 'newPassword' : 'password'}
          autoComplete={setup ? 'new-password' : 'current-password'}
          secureTextEntry
          autoCapitalize="none"
          autoCorrect={false}
          placeholder={setup ? 'New password, at least 8 characters' : 'Password'}
          value={password}
          autoFocus
          onChangeText={setPassword}
          returnKeyType={setup ? 'next' : 'go'}
          onSubmitEditing={() => {
            if (!setup && ready && !busy) void submit();
          }}
        />
        {setup ? (
          <Field
            textContentType="newPassword"
            autoComplete="new-password"
            secureTextEntry
            autoCapitalize="none"
            autoCorrect={false}
            placeholder="Type it again"
            value={confirm}
            onChangeText={setConfirm}
            returnKeyType="go"
            onSubmitEditing={() => {
              if (ready && !busy) void submit();
            }}
          />
        ) : null}

        {failed ? (
          <Text variant="small" tone="destructive">
            {failed}
          </Text>
        ) : null}

        {/* The asymmetry the whole feature rests on, said at the moment
            somebody is deciding whether to have it at all. */}
        {setup ? (
          <Text variant="caption" tone="faint">
            Hiding a photo works whether or not the vault is unlocked. Opening it — browsing,
            restoring, or seeing a single thumbnail — always needs this password.
          </Text>
        ) : null}
      </View>

      <View style={styles.footer}>
        <Button label="Cancel" onPress={closeGate} />
        <Button
          label={setup ? 'Create vault' : 'Unlock'}
          variant="primary"
          disabled={!ready}
          busy={busy}
          onPress={() => void submit()}
          grow
        />
      </View>
    </Sheet>
  );
}

const styles = StyleSheet.create({
  form: { gap: space.md },
  footer: { flexDirection: 'row', gap: space.sm },
});
