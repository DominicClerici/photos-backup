import * as SQLite from 'expo-sqlite';

import {
  emptyCounts,
  ITEM_STATES,
  type ItemKind,
  type ItemPatch,
  type ItemState,
  type NewItem,
  type QueueItem,
  type QueueStore,
  type StateCounts,
} from './types';

export const QUEUE_DATABASE = 'photobackup-queue.db';

type ItemRow = {
  local_id: string;
  kind: string;
  parent_local_id: string | null;
  filename: string;
  created_at: number | null;
  modified_at: number | null;
  size: number | null;
  md5: string | null;
  state: string;
  asset_id: string | null;
  attempts: number;
  next_attempt_at: number;
  last_error: string | null;
};

/**
 * The on-device QueueStore.
 *
 * All SQL lives in this one file. The engine talks to the QueueStore interface
 * instead, so the parts worth testing off-device — reconciliation, backoff,
 * resume — are covered by MemoryQueueStore in Node, and this file is what the
 * device run exercises.
 */
export class SqliteQueueStore implements QueueStore {
  constructor(private readonly db: SQLite.SQLiteDatabase) {}

  async enqueue(items: NewItem[]): Promise<number> {
    if (items.length === 0) return 0;

    let added = 0;
    await this.db.withTransactionAsync(async () => {
      for (const item of items) {
        // `or ignore` is what makes re-enumeration idempotent: an item already in
        // the queue keeps whatever state it has reached.
        const result = await this.db.runAsync(
          `insert or ignore into items
             (local_id, kind, parent_local_id, filename, created_at, modified_at,
              size, md5, state, asset_id, attempts, next_attempt_at, last_error)
           values (?, ?, ?, ?, ?, ?, null, null, 'pending', null, 0, 0, null)`,
          [
            item.localId,
            item.kind,
            item.parentLocalId,
            item.filename,
            item.createdAt,
            item.modifiedAt,
          ]
        );
        added += result.changes;
      }
    });
    return added;
  }

  async due(state: ItemState, limit: number, now: number): Promise<QueueItem[]> {
    // SQLite sorts NULLs last under DESC, which puts assets with no capture time
    // at the back rather than the front.
    const rows = await this.db.getAllAsync<ItemRow>(
      `select * from items
        where state = ? and next_attempt_at <= ?
        order by created_at desc, local_id asc
        limit ?`,
      [state, now, limit]
    );
    return rows.map(toItem);
  }

  async update(localId: string, patch: ItemPatch): Promise<void> {
    const assignments: string[] = [];
    const values: (string | number | null)[] = [];

    for (const [field, column] of Object.entries(COLUMNS)) {
      if (!(field in patch)) continue;
      assignments.push(`${column} = ?`);
      values.push(patch[field as keyof ItemPatch] ?? null);
    }
    if (assignments.length === 0) return;

    values.push(localId);
    await this.db.runAsync(
      `update items set ${assignments.join(', ')} where local_id = ?`,
      values
    );
  }

  async nextRetryAt(): Promise<number | null> {
    const row = await this.db.getFirstAsync<{ at: number | null }>(
      `select min(next_attempt_at) as at from items
        where state in ('pending', 'unknown', 'hashed', 'want')`
    );
    return row?.at ?? null;
  }

  async counts(): Promise<StateCounts> {
    const rows = await this.db.getAllAsync<{ state: string; n: number }>(
      'select state, count(*) as n from items group by state'
    );
    const counts = emptyCounts();
    for (const row of rows) {
      if (isItemState(row.state)) counts[row.state] = row.n;
    }
    return counts;
  }

  async failed(limit: number): Promise<QueueItem[]> {
    const rows = await this.db.getAllAsync<ItemRow>(
      `select * from items where state = 'failed' order by created_at desc, local_id asc limit ?`,
      [limit]
    );
    return rows.map(toItem);
  }

  async resetFailed(): Promise<number> {
    // Back to 'hashed' where a digest survives, so a retry does not pay for the
    // same hash twice.
    const result = await this.db.runAsync(
      `update items
          set state = case
                        when md5 is not null and size is not null then 'hashed'
                        else 'pending'
                      end,
              attempts = 0,
              next_attempt_at = 0,
              last_error = null
        where state = 'failed'`
    );
    return result.changes;
  }

  async reopenDone(): Promise<number> {
    // Same landing state as resetFailed, for the same reason: a surviving digest
    // is the expensive half of the work and re-checking does not invalidate it.
    //
    // asset_id is cleared, though. It names a row on the server, and reopening
    // these items is precisely the act of deciding that claim can no longer be
    // trusted; the next check writes whatever the archive actually answers.
    const result = await this.db.runAsync(
      `update items
          set state = case
                        when md5 is not null and size is not null then 'hashed'
                        else 'pending'
                      end,
              asset_id = null,
              attempts = 0,
              next_attempt_at = 0,
              last_error = null
        where state = 'done'`
    );
    return result.changes;
  }
}

/** Opens the queue database, creating the schema on first run. */
export async function openQueueStore(name: string = QUEUE_DATABASE): Promise<SqliteQueueStore> {
  const db = await SQLite.openDatabaseAsync(name);
  await createSchema(db);
  return new SqliteQueueStore(db);
}

async function createSchema(db: SQLite.SQLiteDatabase): Promise<void> {
  await db.execAsync('pragma journal_mode = WAL');
  await db.execAsync(`
    create table if not exists items (
      local_id        text primary key,
      kind            text    not null,
      parent_local_id text,
      filename        text    not null,
      created_at      integer,
      modified_at     integer,
      size            integer,
      md5             text,
      state           text    not null,
      asset_id        text,
      attempts        integer not null default 0,
      next_attempt_at integer not null default 0,
      last_error      text
    );
    create index if not exists items_state_idx on items (state, next_attempt_at);
    create table if not exists meta (
      key   text primary key,
      value text
    );
  `);
}

/** Patch field to column. Also the allowlist that keeps update() SQL-safe. */
const COLUMNS: Record<keyof ItemPatch, string> = {
  state: 'state',
  size: 'size',
  md5: 'md5',
  assetId: 'asset_id',
  attempts: 'attempts',
  nextAttemptAt: 'next_attempt_at',
  lastError: 'last_error',
  modifiedAt: 'modified_at',
};

function toItem(row: ItemRow): QueueItem {
  return {
    localId: row.local_id,
    kind: row.kind as ItemKind,
    parentLocalId: row.parent_local_id,
    filename: row.filename,
    createdAt: row.created_at,
    modifiedAt: row.modified_at,
    size: row.size,
    md5: row.md5,
    state: isItemState(row.state) ? row.state : 'pending',
    assetId: row.asset_id,
    attempts: row.attempts,
    nextAttemptAt: row.next_attempt_at,
    lastError: row.last_error,
  };
}

function isItemState(value: string): value is ItemState {
  return (ITEM_STATES as string[]).includes(value);
}
