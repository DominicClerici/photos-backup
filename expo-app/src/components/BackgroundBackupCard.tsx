import { useCallback, useEffect, useState } from 'react';
import { StyleSheet, View } from 'react-native';

import {
  backgroundTaskRegistered,
  backgroundTasksAvailable,
  loadOutcome,
  shouldRunInBackground,
  syncBackgroundRegistration,
  triggerBackgroundWindow,
  type BackgroundOutcome,
} from '../background';
import { useArchive } from '../state/archive';
import { formatAge } from '../stats/format';
import { color, space } from '../theme';
import { Button, Card, Row, Text, Toggle } from '../ui';

/**
 * Backing up without the app open, and an honest account of what that means.
 *
 * The switch is the small part. The rest of this card exists because iOS decides
 * when a background window happens and the app cannot make one occur — so a
 * switch on its own would be a promise the app is in no position to keep, which
 * is precisely what PROJECT.md § 8 risk 2 says the UI must not imply. What is
 * shown instead is the three things that are actually knowable: whether iOS will
 * run background tasks for this app, whether it is really holding a registration
 * for one, and what the last window did.
 *
 * The registration is asked about rather than assumed. `registerTaskAsync`
 * throws on a restricted device and on a build without the processing background
 * mode, and both failures look exactly like success from the switch's side — a
 * preference saved to a file that nothing in iOS ever read.
 */
export function BackgroundBackupCard() {
  const { config, credential, setBackgroundBackup } = useArchive();
  const [available, setAvailable] = useState<boolean | null>(null);
  const [registered, setRegistered] = useState<boolean | null>(null);
  const [last, setLast] = useState<BackgroundOutcome | null>(loadOutcome);
  const [triggering, setTriggering] = useState(false);

  const wanted = shouldRunInBackground(config, credential !== null);

  /**
   * Reconciles before reporting, rather than racing the provider that also
   * reconciles. Effects run child-first, so a card that only read the
   * registration would read it before the provider above had finished writing
   * it and accuse a perfectly healthy install of not being registered.
   * `syncBackgroundRegistration` is idempotent, so doing it twice costs a
   * lookup.
   */
  useEffect(() => {
    let alive = true;
    void (async () => {
      await syncBackgroundRegistration(wanted);
      const [isAvailable, isRegistered] = await Promise.all([
        backgroundTasksAvailable(),
        backgroundTaskRegistered(),
      ]);
      if (!alive) return;
      setAvailable(isAvailable);
      setRegistered(isRegistered);
    })();
    return () => {
      alive = false;
    };
  }, [wanted]);

  const toggle = useCallback(
    (next: boolean) => {
      setBackgroundBackup(next);
      // The record describes windows that ran under the old answer. Switching
      // off clears it on disk; clearing it here is what takes the sentence off
      // the screen in the same frame as the switch moves.
      if (!next) setLast(null);
    },
    [setBackgroundBackup]
  );

  /**
   * Debug builds only. iOS runs the window on its own schedule of one, so this
   * returns having asked rather than having finished — the outcome is re-read a
   * few seconds later, and re-read again by pressing it a second time if the run
   * was a long one.
   */
  const trigger = useCallback(async () => {
    setTriggering(true);
    try {
      await triggerBackgroundWindow();
      await new Promise((resolve) => setTimeout(resolve, 4000));
      setLast(loadOutcome());
    } finally {
      setTriggering(false);
    }
  }, []);

  const on = config.backgroundBackup;

  return (
    <Card title="Background backup">
      <Row style={styles.header}>
        <View style={styles.grow}>
          <Text variant="body">Back up when the app is closed</Text>
          <Text variant="small" tone="muted">
            While charging and on Wi-Fi
          </Text>
        </View>
        <Toggle
          value={on}
          onValueChange={toggle}
          disabled={!credential}
          label="Back up when the app is closed"
        />
      </Row>

      <Text variant="small" tone="muted">
        iOS chooses when to give the app time, usually overnight on a charger, and
        it is not a schedule anything here can set. A window lasts minutes rather
        than hours, so this keeps the queue moving rather than finishing it — the
        Backup tab is still where a full backfill happens.
      </Text>

      {!credential && (
        <Text variant="small" tone="warning">
          This phone is not paired, so there would be nowhere to back up to.
        </Text>
      )}

      {on && available === false && (
        <Text variant="small" tone="warning">
          iOS is not allowing background tasks for this app, so no window will ever
          come. Check Settings › General › Background App Refresh — Low Power Mode
          switches it off too.
        </Text>
      )}

      {on && available === true && registered === false && (
        <Text variant="small" tone="warning">
          This is switched on but iOS is not holding a registration for it, so no
          window will come. A build without the processing background mode does
          this; so does a restore from another phone.
        </Text>
      )}

      {on && (
        <View style={styles.last}>
          <Text variant="small" tone="muted">
            {last ? 'Last window' : 'No window has run yet.'}
          </Text>
          {last && (
            <Text variant="small" tone={toneFor(last)}>
              {formatAge(last.at, Date.now())} — {last.detail}
            </Text>
          )}
        </View>
      )}

      {/* Development builds only, and stripped from a release bundle along with
          the branch. There is otherwise no way to see this feature work that
          does not involve plugging the phone in and waiting until morning. */}
      {__DEV__ && on && (
        <Button
          label={triggering ? 'Waiting for the window…' : 'Trigger a window (debug)'}
          icon="play"
          onPress={() => void trigger()}
          busy={triggering}
        />
      )}
    </Card>
  );
}

/**
 * `held` reads as muted rather than as a warning on purpose: a window that
 * declined to run because the phone was on battery is the gate working, not a
 * problem to be looked into.
 */
function toneFor(outcome: BackgroundOutcome) {
  if (outcome.result === 'failed') return 'destructive' as const;
  if (outcome.result === 'worked') return 'success' as const;
  return 'muted' as const;
}

const styles = StyleSheet.create({
  header: { alignItems: 'center' },
  grow: { flex: 1 },
  last: {
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: color.border,
    paddingTop: space.sm,
    gap: 2,
  },
});
