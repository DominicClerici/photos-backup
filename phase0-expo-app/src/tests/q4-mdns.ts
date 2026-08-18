import Zeroconf from 'react-native-zeroconf';

import { errText, type TestContext, type TestResult } from './types';

const SERVICE_TYPE = 'photobackup';
const SCAN_MS = 12000;

type Service = {
  name: string;
  fullName?: string;
  host?: string;
  port?: number;
  addresses?: string[];
  txt?: Record<string, string>;
};

export async function runMdns(ctx: TestContext): Promise<TestResult> {
  const { log } = ctx;

  let zeroconf: Zeroconf;
  try {
    zeroconf = new Zeroconf();
  } catch (e) {
    return {
      // A legacy bridge module under RN 0.86 bridgeless — if the interop layer
      // does not carry it, this is where it shows up.
      ok: false,
      verdict: `react-native-zeroconf failed to construct: ${errText(e)}`,
      details: [],
    };
  }

  const resolved = new Map<string, Service>();
  const events: string[] = [];
  const errors: string[] = [];

  const record = (msg: string) => {
    events.push(msg);
    log(msg);
  };

  zeroconf.on('start', () => record('scan started'));
  zeroconf.on('stop', () => record('scan stopped'));
  zeroconf.on('found', (name: string) => record(`found: ${name}`));
  zeroconf.on('remove', (name: string) => record(`removed: ${name}`));
  zeroconf.on('resolved', (service: Service) => {
    resolved.set(service.name, service);
    record(
      `resolved: ${service.name} -> ${service.host}:${service.port} ` +
        `addresses=${(service.addresses ?? []).join(',')} txt=${JSON.stringify(service.txt ?? {})}`
    );
  });
  zeroconf.on('error', (err: Error) => {
    errors.push(String(err?.message ?? err));
    record(`error: ${String(err?.message ?? err)}`);
  });

  log(`scanning _${SERVICE_TYPE}._tcp.local. for ${SCAN_MS / 1000}s`);
  try {
    zeroconf.scan(SERVICE_TYPE, 'tcp', 'local.');
  } catch (e) {
    return { ok: false, verdict: `scan() threw: ${errText(e)}`, details: [['events', events.join('\n')]] };
  }

  await new Promise((r) => setTimeout(r, SCAN_MS));
  try {
    zeroconf.stop();
  } catch {}
  zeroconf.removeDeviceListeners();

  const services = [...resolved.values()];
  const details: [string, string][] = [
    ['service type', `_${SERVICE_TYPE}._tcp.local.`],
    ['scan duration', `${SCAN_MS / 1000} s`],
    ['services resolved', String(services.length)],
  ];
  if (errors.length) details.push(['errors', errors.join('; ')]);

  if (!services.length) {
    return {
      ok: false,
      verdict:
        'nothing resolved — check the Local Network permission prompt was accepted, ' +
        'and that the server is advertising',
      details: [...details, ['events', events.join('\n') || 'none']],
    };
  }

  for (const s of services) {
    details.push([
      s.name,
      `${s.host}:${s.port} addresses=${(s.addresses ?? []).join(', ')} txt=${JSON.stringify(s.txt ?? {})}`,
    ]);
  }

  // Discovery is only useful if the address it hands back is actually dialable.
  const target = services[0];
  const ipv4 = (target.addresses ?? []).find((a) => a.includes('.'));
  const candidates = [ipv4, target.host?.replace(/\.$/, '')].filter(Boolean) as string[];

  let reached: string | null = null;
  for (const hostish of candidates) {
    const url = `http://${hostish}:${target.port}/health`;
    try {
      const t0 = Date.now();
      const res = await fetch(url);
      const body = await res.json();
      log(`health via ${hostish}: ${res.status} in ${Date.now() - t0}ms ${JSON.stringify(body)}`);
      details.push([`GET ${url}`, `${res.status} in ${Date.now() - t0} ms`]);
      if (res.ok && !reached) reached = hostish;
    } catch (e) {
      log(`health via ${hostish} failed: ${errText(e)}`);
      details.push([`GET ${url}`, `failed: ${errText(e)}`]);
    }
  }

  return {
    ok: reached !== null,
    verdict: reached
      ? `discovered and reached the server at ${reached}:${target.port}`
      : 'resolved the service but could not reach it over HTTP',
    details,
  };
}
