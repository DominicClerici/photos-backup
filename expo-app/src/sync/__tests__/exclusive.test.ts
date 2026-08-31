import { acquireRun, runHolder } from '../exclusive';

/**
 * The lock that stands in for an invariant the engine used to get for free.
 *
 * Worth testing off-device precisely because the situation it guards against
 * cannot be produced on one to order: it needs iOS to start a background window
 * at the moment somebody presses Start, which is not a thing anybody can arrange.
 */
describe('the run lock', () => {
  afterEach(() => {
    // Module state, so a test that took the lock and did not give it back would
    // fail every test after it rather than itself.
    acquireRun('foreground')?.();
  });

  it('is free when nothing is running', () => {
    expect(runHolder()).toBeNull();
  });

  it('is held by whoever took it', () => {
    const release = acquireRun('background');
    expect(release).not.toBeNull();
    expect(runHolder()).toBe('background');
    release?.();
    expect(runHolder()).toBeNull();
  });

  it('refuses a second holder', () => {
    const first = acquireRun('foreground');
    expect(acquireRun('background')).toBeNull();
    expect(runHolder()).toBe('foreground');
    first?.();
  });

  it('is free again once released, whichever order', () => {
    const first = acquireRun('background');
    expect(acquireRun('foreground')).toBeNull();
    first?.();

    const second = acquireRun('foreground');
    expect(second).not.toBeNull();
    second?.();
  });

  it('ignores a release called twice, rather than freeing somebody else’s run', () => {
    const first = acquireRun('foreground');
    first?.();
    first?.();

    const second = acquireRun('background');
    expect(second).not.toBeNull();
    // The stale release must not touch the lock the second run now holds.
    first?.();
    expect(runHolder()).toBe('background');
    second?.();
  });
});
