import {
  emptyCounts,
  RETRYABLE_STATES,
  type ItemPatch,
  type ItemState,
  type NewItem,
  type QueueItem,
  type QueueStore,
  type StateCounts,
} from './types';

/**
 * An in-memory QueueStore.
 *
 * expo-sqlite has no Node build, so the engine cannot be tested off-device while
 * talking to SQLite directly. This is the other implementation of the same
 * interface, which is what lets resume, backoff and reconciliation be tested in
 * plain Node instead of only by killing an app on a phone.
 */
export class MemoryQueueStore implements QueueStore {
  private readonly items = new Map<string, QueueItem>();

  constructor(seed: QueueItem[] = []) {
    for (const item of seed) this.items.set(item.localId, { ...item });
  }

  async enqueue(items: NewItem[]): Promise<number> {
    let added = 0;
    for (const item of items) {
      if (this.items.has(item.localId)) continue;
      this.items.set(item.localId, {
        ...item,
        size: null,
        md5: null,
        state: 'pending',
        assetId: null,
        attempts: 0,
        nextAttemptAt: 0,
        lastError: null,
      });
      added += 1;
    }
    return added;
  }

  async pruneShared(keep: string[]): Promise<number> {
    const wanted = new Set(keep);
    let removed = 0;
    for (const item of [...this.items.values()]) {
      if (item.source !== 'shared' || item.state === 'done') continue;
      if (wanted.has(item.localId)) continue;
      this.items.delete(item.localId);
      removed += 1;
    }
    return removed;
  }

  async due(state: ItemState, limit: number, now: number): Promise<QueueItem[]> {
    return [...this.items.values()]
      .filter((item) => item.state === state && item.nextAttemptAt <= now)
      .sort(byNewestFirst)
      .slice(0, limit)
      .map((item) => ({ ...item }));
  }

  async update(localId: string, patch: ItemPatch): Promise<void> {
    const item = this.items.get(localId);
    if (!item) return;
    this.items.set(localId, { ...item, ...patch });
  }

  async nextRetryAt(): Promise<number | null> {
    let earliest: number | null = null;
    for (const item of this.items.values()) {
      if (!RETRYABLE_STATES.includes(item.state)) continue;
      if (earliest === null || item.nextAttemptAt < earliest) earliest = item.nextAttemptAt;
    }
    return earliest;
  }

  async counts(): Promise<StateCounts> {
    const counts = emptyCounts();
    for (const item of this.items.values()) counts[item.state] += 1;
    return counts;
  }

  async failed(limit: number): Promise<QueueItem[]> {
    return [...this.items.values()]
      .filter((item) => item.state === 'failed')
      .sort(byNewestFirst)
      .slice(0, limit)
      .map((item) => ({ ...item }));
  }

  async resetFailed(): Promise<number> {
    let reset = 0;
    for (const item of this.items.values()) {
      if (item.state !== 'failed') continue;
      this.items.set(item.localId, {
        ...item,
        state: item.md5 !== null && item.size !== null ? 'hashed' : 'pending',
        attempts: 0,
        nextAttemptAt: 0,
        lastError: null,
      });
      reset += 1;
    }
    return reset;
  }

  async reopenDone(): Promise<number> {
    let reopened = 0;
    for (const item of this.items.values()) {
      if (item.state !== 'done') continue;
      this.items.set(item.localId, {
        ...item,
        state: item.md5 !== null && item.size !== null ? 'hashed' : 'pending',
        assetId: null,
        attempts: 0,
        nextAttemptAt: 0,
        lastError: null,
      });
      reopened += 1;
    }
    return reopened;
  }

  /** Test affordance: the full queue, for asserting on end state. */
  snapshot(): QueueItem[] {
    return [...this.items.values()].map((item) => ({ ...item }));
  }

  get(localId: string): QueueItem | undefined {
    const item = this.items.get(localId);
    return item ? { ...item } : undefined;
  }
}

function byNewestFirst(a: QueueItem, b: QueueItem): number {
  const left = a.createdAt ?? 0;
  const right = b.createdAt ?? 0;
  if (left !== right) return right - left;
  return a.localId.localeCompare(b.localId);
}
