import { CHUNK_SIZE, CHUNK_THRESHOLD, planChunks } from '../chunkPlan';
import { SyncError } from '../types';

test('plans contiguous ranges covering the whole file', () => {
  const ranges = planChunks(25, 0, 10);

  expect(ranges).toEqual([
    [0, 10],
    [10, 20],
    [20, 25],
  ]);
});

// Resuming is the whole point: the ranges start where the server already is,
// not at zero.
test('plans only what the server is still owed', () => {
  const ranges = planChunks(25, 10, 10);

  expect(ranges).toEqual([
    [10, 20],
    [20, 25],
  ]);
});

test('a file the server already holds in full needs no chunks', () => {
  expect(planChunks(25, 25, 10)).toEqual([]);
});

test('a file smaller than one chunk is a single range', () => {
  expect(planChunks(5, 0, 10)).toEqual([[0, 5]]);
});

test('an exact multiple of the chunk size produces no empty trailing range', () => {
  expect(planChunks(20, 0, 10)).toEqual([
    [0, 10],
    [10, 20],
  ]);
});

// A server claiming an offset past the end of the file is describing bytes the
// phone does not have. Sending anything on that basis would produce a blob that
// cannot match its digest.
test('an impossible offset is rejected rather than planned around', () => {
  expect(() => planChunks(10, 11, 4)).toThrow(SyncError);
  expect(() => planChunks(10, -1, 4)).toThrow(SyncError);
});

test('the 550MB video in the test corpus plans a manageable number of chunks', () => {
  const ranges = planChunks(576_507_547, 0);

  expect(ranges.length).toBe(Math.ceil(576_507_547 / CHUNK_SIZE));
  expect(ranges.length).toBeLessThan(100);
  expect(ranges[ranges.length - 1][1]).toBe(576_507_547);
});

test('the threshold and chunk size agree with the server defaults', () => {
  expect(CHUNK_THRESHOLD).toBe(64 * 1024 * 1024);
  expect(CHUNK_SIZE).toBe(8 * 1024 * 1024);
});
