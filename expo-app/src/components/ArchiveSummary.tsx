import { useArchive } from '../state/archive';
import { formatAge, formatBytes, formatCount, formatLastBackup } from '../stats/format';
import { Count, Counts, Text } from '../ui';

/**
 * What is actually archived, as photod reports it.
 *
 * These numbers used to be counted from the local queue, which made them a
 * record of what this app had done rather than of what is stored: reinstalling
 * it, or losing the SQLite file, showed zero archived against a library that
 * was entirely backed up. The archive is the only thing that knows what the
 * archive holds, so it is asked.
 *
 * The cost is that they arrive over the network and can be missing. An
 * unreachable server shows the last known figures marked stale rather than
 * dashes, because the photos are archived either way and a blank card would
 * imply otherwise.
 */
export function ArchiveSummary() {
  const { stats, statsStale, loadingStats, credential } = useArchive();

  if (!credential) {
    return (
      <Text variant="small" tone="muted">
        Pair this phone to see what the archive holds.
      </Text>
    );
  }

  if (!stats) {
    return (
      <>
        <Counts>
          <Count label="archived" value="—" />
          <Count label="stored" value="—" />
          <Count label="last backup" value="—" />
        </Counts>
        <Text variant="small" tone="muted">
          {loadingStats ? 'asking the server…' : 'could not reach the server for these yet.'}
        </Text>
      </>
    );
  }

  const { device, archive } = stats.stats;
  const now = Date.now();

  return (
    <>
      <Counts>
        <Count label="archived" value={formatCount(device.archived)} tone="success" />
        <Count label="stored" value={formatBytes(device.bytes)} />
        <Count label="last backup" value={formatLastBackup(device.last_upload_at, now)} />
      </Counts>

      <Text variant="small" tone="muted">
        {formatCount(device.photos)} photos · {formatCount(device.videos)} videos from this phone
      </Text>
      <Text variant="small" tone="muted">
        The archive holds {formatCount(archive.assets)} items, {formatBytes(archive.bytes)}
        {archive.pending_jobs > 0 &&
          ` · ${formatCount(archive.pending_jobs)} thumbnails still being built`}
      </Text>

      {archive.failed_jobs > 0 && (
        <Text variant="small" tone="warning">
          {formatCount(archive.failed_jobs)} derivatives failed on the server. Nothing is lost —
          the originals are archived — but those tiles will not fill in.
        </Text>
      )}

      {statsStale && (
        <Text variant="small" tone="warning">
          as of {formatAge(stats.fetchedAt, now)} — could not reach the server to refresh.
        </Text>
      )}
    </>
  );
}
