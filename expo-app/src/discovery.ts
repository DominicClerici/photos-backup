import Zeroconf, { type ZeroconfService } from 'react-native-zeroconf';

import { errorText } from './sync/types';

/** react-native-zeroconf takes the bare name; it assembles `_photobackup._tcp`. */
const SERVICE_NAME = 'photobackup';
const SCAN_MS = 5_000;
const HEALTH_TIMEOUT_MS = 2_500;

export type Discovered = {
  url: string;
  name: string;
  addresses: string[];
};

export type ScanOutcome =
  | { kind: 'found'; server: Discovered }
  /**
   * Nothing answered. Phase 0 established that this is indistinguishable from a
   * denied Local Network permission — the scan simply returns empty, with no
   * error — so callers must present both possibilities.
   */
  | { kind: 'empty' }
  | { kind: 'unavailable'; reason: string };

/**
 * Browses for photod on the LAN, resolving as soon as the first server answers
 * rather than always waiting out the timeout.
 */
export async function scanForServer(timeoutMs: number = SCAN_MS): Promise<ScanOutcome> {
  let zeroconf: Zeroconf;
  try {
    zeroconf = new Zeroconf();
  } catch (e) {
    return { kind: 'unavailable', reason: `zeroconf unavailable: ${errorText(e)}` };
  }

  return new Promise<ScanOutcome>((resolve) => {
    let settled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const finish = (outcome: ScanOutcome) => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      try {
        zeroconf.stop();
        zeroconf.removeAllListeners();
        zeroconf.removeDeviceListeners();
      } catch {
        // Tearing down a scan that never started is not worth reporting.
      }
      resolve(outcome);
    };

    zeroconf.on('resolved', (service: ZeroconfService) => {
      const server = toDiscovered(service);
      if (server) finish({ kind: 'found', server });
    });
    zeroconf.on('error', (err: Error) => {
      finish({ kind: 'unavailable', reason: errorText(err) });
    });

    timer = setTimeout(() => finish({ kind: 'empty' }), timeoutMs);

    try {
      zeroconf.scan(SERVICE_NAME, 'tcp', 'local.');
    } catch (e) {
      finish({ kind: 'unavailable', reason: `scan failed: ${errorText(e)}` });
    }
  });
}

/**
 * Prefers the IPv4 address over the advertised host name. The name resolves only
 * through mDNS, while the address is dialable immediately — and Phase 0 measured
 * the address as the faster of the two.
 *
 * https, because photod's upload path is TLS-only. Both the address and the
 * advertised `.local` name are in the certificate photod issues itself, so
 * either validates once the CA is installed.
 */
function toDiscovered(service: ZeroconfService): Discovered | null {
  if (!service.port) return null;
  const addresses = service.addresses ?? [];
  const host = addresses.find(isIPv4) ?? service.host;
  if (!host) return null;
  return {
    url: `https://${host}:${service.port}`,
    name: service.name,
    addresses,
  };
}

function isIPv4(address: string): boolean {
  return /^\d{1,3}(\.\d{1,3}){3}$/.test(address);
}

/**
 * True when /health answers. Used to decide whether a remembered address still
 * works.
 *
 * /health needs no token, on purpose — this has to be answerable before the
 * phone holds one. It does need TLS to validate, so a false here also covers "the
 * CA is not installed", which is why the pairing screen says so rather than
 * insisting the server is down.
 */
export async function isReachable(url: string, timeoutMs: number = HEALTH_TIMEOUT_MS): Promise<boolean> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const response = await fetch(`${url.replace(/\/+$/, '')}/health`, { signal: controller.signal });
    return response.ok;
  } catch {
    return false;
  } finally {
    clearTimeout(timer);
  }
}

export type ServerSource = 'discovered' | 'remembered' | 'manual';

export type ServerResolution = {
  url: string | null;
  source: ServerSource | null;
  /** Explains what happened, in words meant for the screen. */
  note: string;
  /** True when the scan came back empty, which has two very different causes. */
  emptyScan: boolean;
};

/**
 * Settles on an address: whatever mDNS finds, else the last address that
 * answered, else whatever was typed in by hand.
 */
export async function resolveServer(options: {
  rememberedUrl: string | null;
  manualUrl: string | null;
  timeoutMs?: number;
}): Promise<ServerResolution> {
  const { rememberedUrl, manualUrl, timeoutMs } = options;

  const scan = await scanForServer(timeoutMs);
  if (scan.kind === 'found') {
    return {
      url: scan.server.url,
      source: 'discovered',
      note: `found ${scan.server.name} at ${scan.server.url}`,
      emptyScan: false,
    };
  }

  const emptyScan = scan.kind === 'empty';
  const scanNote =
    scan.kind === 'unavailable'
      ? scan.reason
      : 'no server advertised on this network — either photod is not running, or Local Network access was denied in Settings';

  if (rememberedUrl && (await isReachable(rememberedUrl))) {
    return {
      url: rememberedUrl,
      source: 'remembered',
      note: `${scanNote}; using the last address that answered (${rememberedUrl})`,
      emptyScan,
    };
  }

  if (manualUrl) {
    return {
      url: manualUrl,
      source: 'manual',
      note: `${scanNote}; using the address entered by hand`,
      emptyScan,
    };
  }

  return { url: null, source: null, note: scanNote, emptyScan };
}
