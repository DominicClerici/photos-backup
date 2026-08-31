/**
 * Backing up while the app is closed.
 *
 * Three files, one each for the three questions: `task.ts` is what iOS is told,
 * `run.ts` is what happens when iOS says yes, and `record.ts` is how anybody
 * finds out afterwards.
 */

export { loadOutcome, type BackgroundOutcome, type BackgroundResult } from './record';
export { runBackgroundBackup } from './run';
export {
  backgroundTaskRegistered,
  backgroundTasksAvailable,
  shouldRunInBackground,
  syncBackgroundRegistration,
  triggerBackgroundWindow,
  BACKGROUND_BACKUP_TASK,
} from './task';
