import * as BackgroundTask from 'expo-background-task';
import * as TaskManager from 'expo-task-manager';

import type { Config } from '../config';
import { clearOutcome } from './record';
import { runBackgroundBackup } from './run';

/**
 * The registration side of background backup: what iOS is told, and when.
 *
 * `defineTask` has to run at module scope, before anything else, because a
 * background launch is iOS starting the app *for* the task — the JS bundle
 * loads, this file runs, and the native side then looks for a handler under
 * this name. A definition inside a component would be registered a frame too
 * late, on a launch where no component may ever be asked to render. This module
 * is imported by `app/_layout.tsx` for that reason and no other.
 *
 * Under the hood expo-background-task schedules one `BGProcessingTask` with
 * `requiresNetworkConnectivity` and, notably, `requiresExternalPower = false` —
 * so iOS will happily wake the app on battery. The charger requirement is the
 * app's own, checked in `runBackgroundBackup` against the same `DEFAULT_GATE`
 * the Backup tab uses. That split is intentional: iOS decides when there is a
 * window, and the app decides whether this is a window worth spending.
 */

export const BACKGROUND_BACKUP_TASK = 'photobackup.backup';

/**
 * A floor on the delay between windows, not a schedule.
 *
 * iOS treats it as "no sooner than", and in practice runs processing tasks in
 * its own windows — overnight, on a charger, which is when this app wants to run
 * anyway. An hour rather than the twelve-hour default because most windows are
 * refused: the gate wants charging and Wi-Fi, and a phone that is plugged in for
 * two hours in the evening should have more than one chance to be noticed. A
 * refused window costs a launch and a battery reading before it returns.
 */
const MINIMUM_INTERVAL_MINUTES = 60;

TaskManager.defineTask(BACKGROUND_BACKUP_TASK, async () => {
  try {
    const outcome = await runBackgroundBackup();
    return outcome.result === 'failed'
      ? BackgroundTask.BackgroundTaskResult.Failed
      : BackgroundTask.BackgroundTaskResult.Success;
  } catch {
    // runBackgroundBackup writes its own outcome and does not throw. If it ever
    // does, iOS still needs an answer, and reporting failure is the answer that
    // does not leave the task looking like it succeeded.
    return BackgroundTask.BackgroundTaskResult.Failed;
  }
});

/**
 * Whether iOS should be scheduling windows for this phone, as one rule.
 *
 * Two callers reconcile the registration — the provider at launch and the
 * settings card while it is open — and a rule written out twice is a rule that
 * eventually disagrees with itself. An unpaired phone is in it because a window
 * spent discovering there is nowhere to upload to is a window wasted.
 */
export function shouldRunInBackground(config: Config, paired: boolean): boolean {
  return config.backgroundBackup && paired;
}

/**
 * Makes the registration match the preference.
 *
 * Idempotent and safe to call on every launch, which is how it is called: the
 * registration lives in iOS rather than in the app, so a reinstall, a restore,
 * or a build that changed the task name leaves the preference saying yes and
 * nothing scheduled. Reconciling on mount is what repairs that without anybody
 * having to notice.
 */
export async function syncBackgroundRegistration(enabled: boolean): Promise<void> {
  try {
    const registered = await TaskManager.isTaskRegisteredAsync(BACKGROUND_BACKUP_TASK);
    if (enabled && !registered) {
      await BackgroundTask.registerTaskAsync(BACKGROUND_BACKUP_TASK, {
        minimumInterval: MINIMUM_INTERVAL_MINUTES,
      });
    } else if (!enabled && registered) {
      await BackgroundTask.unregisterTaskAsync(BACKGROUND_BACKUP_TASK);
      clearOutcome();
    }
  } catch {
    // Never worth breaking a launch over, and never worth hiding either: this
    // throws when iOS has background tasks restricted, and when the processing
    // background mode is missing from the built app. Both mean the switch is on
    // and nothing is scheduled, which is why the settings card asks
    // `backgroundTaskRegistered` rather than trusting that this call worked.
  }
}

/**
 * Whether iOS is actually holding a registration for the task.
 *
 * The question the switch cannot answer on its own. The preference is a line in
 * a file on this phone; the registration is state inside iOS, and every way the
 * two come apart — a restricted device, a build without the background mode, an
 * app restored from a backup — looks identical from the switch's side.
 */
export async function backgroundTaskRegistered(): Promise<boolean> {
  try {
    return await TaskManager.isTaskRegisteredAsync(BACKGROUND_BACKUP_TASK);
  } catch {
    return false;
  }
}

/**
 * Asks iOS to run a window now. Debug builds only, and it returns having asked
 * rather than having finished — the run happens on iOS's schedule of one.
 *
 * This exists because the feature is otherwise close to untestable: the honest
 * way to see a background window is to plug the phone in and look at the card
 * tomorrow. `BGTaskScheduler`'s debug simulation collapses that to a button.
 */
export async function triggerBackgroundWindow(): Promise<void> {
  if (!__DEV__) return;
  await BackgroundTask.triggerTaskWorkerForTestingAsync();
}

/**
 * Whether iOS will run background tasks for this app at all.
 *
 * `Restricted` is almost always Background App Refresh being off — either for
 * this app or system-wide, including whenever Low Power Mode is on. Worth
 * surfacing, because it is the difference between "this is switched on and
 * waiting for a window" and "this is switched on and will never run".
 */
export async function backgroundTasksAvailable(): Promise<boolean> {
  try {
    const status = await BackgroundTask.getStatusAsync();
    return status === BackgroundTask.BackgroundTaskStatus.Available;
  } catch {
    return false;
  }
}
