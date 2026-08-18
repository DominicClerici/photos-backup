import { formatAge, formatBytes, formatCount, formatLastBackup } from '../format';

describe('formatBytes', () => {
  it('uses decimal units, matching how the drive is sold', () => {
    expect(formatBytes(100_000_000_000)).toBe('100 GB');
  });

  it('keeps one decimal below a hundred and none above it', () => {
    expect(formatBytes(98_200_000_000)).toBe('98.2 GB');
    expect(formatBytes(402_000_000_000)).toBe('402 GB');
  });

  it('never shows a fraction of a byte', () => {
    expect(formatBytes(512)).toBe('512 B');
  });

  it('reads as empty rather than as an error when nothing is stored', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(Number.NaN)).toBe('0 B');
  });
});

describe('formatAge', () => {
  const now = Date.parse('2026-08-11T12:00:00Z');

  it('does not tick by the second on a screen nobody is watching', () => {
    expect(formatAge(now - 30_000, now)).toBe('just now');
  });

  it('coarsens as it gets older', () => {
    expect(formatAge(now - 5 * 60_000, now)).toBe('5m ago');
    expect(formatAge(now - 2 * 3_600_000, now)).toBe('2h ago');
    expect(formatAge(now - 3 * 86_400_000, now)).toBe('3d ago');
  });
});

describe('formatLastBackup', () => {
  const now = Date.parse('2026-08-11T12:00:00Z');

  // The server omits the field for a phone that has had nothing archived, which
  // has to read differently from a backup that happened a long time ago.
  it('says never when the server reported no backup at all', () => {
    expect(formatLastBackup(undefined, now)).toBe('never');
  });

  it('ages a real timestamp', () => {
    expect(formatLastBackup('2026-08-11T10:00:00Z', now)).toBe('2h ago');
  });

  it('does not render a garbled timestamp as a date in 1970', () => {
    expect(formatLastBackup('not a date', now)).toBe('unknown');
  });
});

describe('formatCount', () => {
  it('separates thousands, because the counts get big', () => {
    expect(formatCount(1284)).toBe('1,284');
    expect(formatCount(0)).toBe('0');
  });
});
