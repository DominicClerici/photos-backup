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
  type SharedOrigin,
  type StateCounts,
} from './types';

export const QUEUE_DATABASE = 'photobackup-queue.db';

/** How many local ids one delete statement binds. Well under SQLite's ceiling. */
const DELETE_BATCH = 400;

type ItemRow = {
  local_id: string;
  kind: string;
  source: string;
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
  shared: string | null;
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
             (local_id, kind, source, parent_local_id, filename, created_at, modified_at,
              shared, size, md5, state, asset_id, attempts, next_attempt_at, last_error)
           values (?, ?, ?, ?, ?, ?, ?, ?, null, null, 'pending', null, 0, 0, null)`,
          [
            item.localId,
            item.kind,
            item.source,
            item.parentLocalId,
            item.filename,
            item.createdAt,
            item.modifiedAt,
            item.shared === null ? null : JSON.stringify(item.shared),
          ]
        );
        added += result.changes;
      }
    });
    return added;
  }

  async forgetShared(): Promise<number> {
    const result = await this.db.runAsync(`delete from items where source = 'shared'`);
    return result.changes;
  }

  async pruneShared(keep: string[]): Promise<number> {
    const wanted = new Set(keep);
    const rows = await this.db.getAllAsync<{ local_id: string }>(
      `select local_id from items where source = 'shared' and state <> 'done'`
    );
    const doomed = rows.map((row) => row.local_id).filter((id) => !wanted.has(id));
    if (doomed.length === 0) return 0;

    // Read and diffed here rather than expressed as one `not in (…)` statement,
    // because the set being kept is every asset in every chosen album — several
    // thousand of them — and SQLite has a ceiling on how many parameters one
    // statement may bind. The deletes are batched under it for the same reason.
    let removed = 0;
    await this.db.withTransactionAsync(async () => {
      for (let at = 0; at < doomed.length; at += DELETE_BATCH) {
        const batch = doomed.slice(at, at + DELETE_BATCH);
        const result = await this.db.runAsync(
          `delete from items where local_id in (${batch.map(() => '?').join(', ')})`,
          batch
        );
        removed += result.changes;
      }
    });
    return removed;
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
      source          text    not null default 'library',
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
      last_error      text,
      shared          text
    );
    create index if not exists items_state_idx on items (state, next_attempt_at);
    create table if not exists meta (
      key   text primary key,
      value text
    );
  `);

  // Every queue that existed before shared albums did holds nothing but camera
  // roll, so the default is the truth for every row already in it. The column is
  // added rather than the table recreated because those rows are the record of
  // what has already been archived, and losing them would re-upload a library.
  await addColumn(db, 'source', "text not null default 'library'");

  // Nullable with no default, because null is the honest value for every row
  // already here: a camera roll item has no shared origin, and a shared item
  // queued by a build that could not read one has none recorded. Telling those
  // two apart is what forgetShared() is for.
  await addColumn(db, 'shared', 'text');
}

/** Adds a column to `items` unless it is already there. */
async function addColumn(db: SQLite.SQLiteDatabase, name: string, definition: string): Promise<void> {
  const columns = await db.getAllAsync<{ name: string }>('pragma table_info(items)');
  if (columns.some((column) => column.name === name)) return;
  await db.execAsync(`alter table items add column ${name} ${definition}`);
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
    source: row.source === 'shared' ? 'shared' : 'library',
    parentLocalId: row.parent_local_id,
    filename: row.filename,
    createdAt: row.created_at,
    modifiedAt: row.modified_at,
    shared: parseShared(row.shared),
    size: row.size,
    md5: row.md5,
    state: isItemState(row.state) ? row.state : 'pending',
    assetId: row.asset_id,
    attempts: row.attempts,
    nextAttemptAt: row.next_attempt_at,
    lastError: row.last_error,
  };
}

/**
 * Reads back what enumerate() wrote, and treats anything unreadable as absent.
 *
 * A row whose JSON will not parse is a row written by some version of this app
 * that this one cannot understand, and the cost of ignoring it is one photograph
 * filed under no album — where throwing would take down a whole backup run over
 * a caption.
 */
function parseShared(value: string | null): SharedOrigin | null {
  if (value === null) return null;
  try {
    const parsed = JSON.parse(value) as SharedOrigin;
    if (!parsed || !Array.isArray(parsed.albums)) return null;
    return { albums: parsed.albums, contributor: parsed.contributor ?? null };
  } catch {
    return null;
  }
}

function isItemState(value: string): value is ItemState {
  return (ITEM_STATES as string[]).includes(value);
}
