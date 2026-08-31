import * as BackgroundTask from 'expo-background-task';
import * as MediaLibrary from 'expo-media-library';

import { archiveAddress, archiveToken, setArchiveAddress, setArchiveToken } from '../archive';
import { loadConfig } from '../config';
import { isReachable } from '../discovery';
import { loadCredential } from '../pairing';
import { loadSelection } from '../sharedalbums/selection';
import { DEFAULT_GATE, readConditions } from '../sync/conditions';
import { DEFAULT_ENGINE_CONFIG, SyncEngine } from '../sync/engine';
import { acquireRun } from '../sync/exclusive';
import { PhotoKitMediaSource } from '../sync/media';
import { sharedQueueStore } from '../sync/sqliteStore';
import { HttpTransport } from '../sync/transport';
import { errorText, isUnauthorized, systemClock, type StateCounts } from '../sync/types';
import { saveOutcome, type BackgroundOutcome, type BackgroundResult } from './record';

/**
 * A backup run with nobody watching it.
 *
 * This is the Backup tab's `start()` with every part that needed a screen taken
 * out. It shares the queue, the engine, the transport and the conditions gate
 * with the foreground run — deliberately, because a background window that
 * backed up differently would be a second implementation of the one thing this
 * app does, diverging quietly on the runs nobody sees. What is different is only
 * what has to be: the inputs come off disk instead of out of React, and the run
 * is written down instead of rendered.
 *
 * The window itself is real execution — iOS launches or resumes the app and the
 * process is alive for it. That matters more than it sounds: PHASE0.md Q1b found
 * that a background *NSURLSession* cannot read a PhotoKit original, because
 * `nsurlsessiond` does not inherit the app's sandbox extension. Here the app is
 * doing its own uploading, so `HttpTransport` keeps `sessionType: 'foreground'`
 * and originals stream straight from the library exactly as they do on the
 * Backup tab. No staging, no second copy of every byte.
 *
 * What iOS does take away is time, without much warning. Everything below is
 * arranged around that: the engine stops at the next safe point when the
 * expiration handler fires, the queue is resumable by construction, and the
 * outcome is written in a `finally` so a window that ends badly still leaves an
 * account of itself.
 */

/**
 * A backstop for a run that stops making progress without erroring.
 *
 * Not the primary limit — iOS's expiration handler is, and it is the one that
 * decides how long a window really lasts. This exists because every timeout in
 * `HttpTransport` bounds a single request and none of them bounds the loop, so a
 * server that answers slowly forever would hold the lock until the process died
 * with nothing written down.
 */
const BACKSTOP_MS = 25 * 60 * 1000;

export async function runBackgroundBackup(): Promise<BackgroundOutcome> {
  const startedAt = Date.now();
  let expired = false;

  const finish = (
    result: BackgroundResult,
    detail: string,
    remaining: number | null = null
  ): BackgroundOutcome => {
    const outcome: BackgroundOutcome = {
      at: Date.now(),
      result,
      detail,
      durationMs: Date.now() - startedAt,
      remaining,
      expired,
    };
    saveOutcome(outcome);
    return outcome;
  };

  const config = loadConfig();
  // A registration that outlived the switch being turned off. Unregistering is
  // what normally stops this happening; refusing here is what makes it certain.
  if (!config.backgroundBackup) return finish('skipped', 'background backup is switched off');

  const credential = await loadCredential();
  if (!credential) return finish('skipped', 'this phone is not paired');

  // Nothing to enumerate without it, and asking is a screen's job — a run that
  // prompted from the background would prompt at nobody.
  const permission = await MediaLibrary.getPermissionsAsync();
  if (!permission.granted) return finish('skipped', 'photo library access is not granted');

  const conditions = await readConditions(DEFAULT_GATE);
  if (conditions.blockedBy) return finish('held', conditions.blockedBy);

  // Before any network work, so a foreground run that is already going keeps
  // the queue to itself rather than being raced for individual rows.
  const release = acquireRun('background');
  if (!release) return finish('skipped', 'a backup was already running');

  let expiration: { remove: () => void } | null = null;
  let backstop: ReturnType<typeof setTimeout> | null = null;

  try {
    const address = await reachableAddress(
      config.lastServerUrl,
      config.serverUrl,
      credential.serverUrl
    );
    if (!address) return finish('failed', 'the archive did not answer at any known address');

    setArchiveAddress(address);
    setArchiveToken(credential.token);

    const store = await sharedQueueStore();
    const engine = new SyncEngine(
      {
        store,
        // Read here rather than passed in, for the reason the Backup tab reads
        // it at the moment a run starts: the ticked albums are what the run is
        // for, and this run has no screen that could have changed them since.
        media: new PhotoKitMediaSource({ albumIds: loadSelection() }),
        transport: new HttpTransport(archiveAddress, archiveToken),
        clock: systemClock,
      },
      { ...DEFAULT_ENGINE_CONFIG, deviceId: credential.deviceId, maxItems: config.maxItems }
    );

    // `stop()` rather than anything abrupt: it asks the run loop to finish the
    // item it is on and unwind, which is the difference between a queue whose
    // last row is consistent and one that has to be repaired on the next run.
    expiration = BackgroundTask.addExpirationListener(() => {
      expired = true;
      engine.stop();
    });
    backstop = setTimeout(() => engine.stop(), BACKSTOP_MS);

    const counts = await engine.run();
    const remaining = owing(counts);

    if (expired) {
      const detail = `iOS ended the window with ${remaining} item(s) still owing`;
      return finish('worked', detail, remaining);
    }
    if (remaining === 0) {
      return finish('worked', 'the archive has everything this phone offered', 0);
    }
    return finish('worked', `${remaining} item(s) still owing`, remaining);
  } catch (e) {
    // Deliberately not `unpair()`, which is what the Backup tab does with the
    // same error. Dropping the keychain entry is how a phone gets sent back to
    // the pairing screen, and doing that from a window nobody is watching means
    // the next person to open the app finds themselves unpaired with no account
    // of why. The token is dead either way; the foreground run will meet the
    // same 401 and can say so on screen while it acts.
    if (isUnauthorized(e)) {
      return finish('failed', 'the archive refused this phone — pair it again');
    }
    return finish('failed', errorText(e));
  } finally {
    if (backstop) clearTimeout(backstop);
    expiration?.remove();
    release();
  }
}

/** How much of the queue has not settled. `failed` is parked, not owing. */
function owing(counts: StateCounts): number {
  return counts.pending + counts.unknown + counts.hashed + counts.want;
}

/**
 * The first address that answers, without a Bonjour scan.
 *
 * Discovery is skipped on purpose. Browsing for a service takes seconds this
 * window does not have to spare, and the addresses worth trying are already
 * written down: whatever a foreground session left installed, the last one that
 * answered, the one typed into settings, and the one this phone was paired
 * against. A phone that has moved off the home network fails all four quickly,
 * which is the right outcome — there is nothing to upload to from a café.
 */
async function reachableAddress(...candidates: (string | null)[]): Promise<string | null> {
  const tried = new Set<string>();
  for (const candidate of [archiveAddress(), ...candidates]) {
    if (!candidate || tried.has(candidate)) continue;
    tried.add(candidate);
    if (await isReachable(candidate)) return candidate;
  }
  return null;
}
