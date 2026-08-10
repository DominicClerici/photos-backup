export type TestResult = {
  ok: boolean;
  verdict: string;
  details: [string, string][];
};

export type TestContext = {
  serverUrl: string;
  log: (msg: string) => void;
};

export function mb(bytes: number) {
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`;
}

export function rate(bytes: number, ms: number) {
  if (ms <= 0) return 'n/a';
  return `${(bytes / 1024 / 1024 / (ms / 1000)).toFixed(1)} MB/s`;
}

export function errText(e: unknown) {
  if (e instanceof Error) return `${e.name}: ${e.message}`;
  return String(e);
}
