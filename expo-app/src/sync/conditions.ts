/**
 * Whether now is a reasonable moment to push a large backfill.
 *
 * Risk 6 in PROJECT.md: 100GB heats the phone significantly and will happily
 * drain the battery and a cellular plan along with it. So bulk work is gated on
 * charging and Wi-Fi by default, with a manual override for "I know, do it
 * anyway".
 *
 * Everything here is deliberately soft. Both checks are native modules that a
 * dev client built before they were added will not have, and the failure a
 * missing battery API should cause is "we could not tell", never "your photos
 * did not back up". Unknown always reads as allowed.
 */

export type Conditions = {
  charging: boolean | null;
  onWifi: boolean | null;
  /** Why a backup is being held, or null when nothing is in the way. */
  blockedBy: string | null;
};

export type GatePolicy = {
  requireCharging: boolean;
  requireWifi: boolean;
};

export const DEFAULT_GATE: GatePolicy = {
  requireCharging: true,
  requireWifi: true,
};

/** Nothing is required: the manual override behind "back up now". */
export const NO_GATE: GatePolicy = {
  requireCharging: false,
  requireWifi: false,
};

export async function readConditions(policy: GatePolicy): Promise<Conditions> {
  const [charging, onWifi] = await Promise.all([isCharging(), isOnWifi()]);

  let blockedBy: string | null = null;
  if (policy.requireCharging && charging === false) {
    blockedBy = 'waiting to be plugged in';
  } else if (policy.requireWifi && onWifi === false) {
    blockedBy = 'waiting for Wi-Fi';
  }

  return { charging, onWifi, blockedBy };
}

/**
 * Reports charging state, or null when it cannot be determined.
 *
 * The import is dynamic because a dev client built before expo-battery was
 * added throws on require, and the honest response to that is to stop gating
 * rather than to stop backing up.
 */
async function isCharging(): Promise<boolean | null> {
  try {
    const battery = await import('expo-battery');
    const state = await battery.getBatteryStateAsync();
    return state === battery.BatteryState.CHARGING || state === battery.BatteryState.FULL;
  } catch {
    return null;
  }
}

async function isOnWifi(): Promise<boolean | null> {
  try {
    const network = await import('expo-network');
    const state = await network.getNetworkStateAsync();
    if (!state.isConnected) return false;
    // An unrecognised transport is not cellular as far as anyone can tell, and
    // refusing to back up over it would be worse than the data it might cost.
    return state.type !== network.NetworkStateType.CELLULAR;
  } catch {
    return null;
  }
}
