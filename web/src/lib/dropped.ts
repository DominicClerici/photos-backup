/**
 * The files behind a drop, including the ones inside folders.
 *
 * `DataTransfer.files` flattens a dropped folder to nothing — the folder itself
 * is not a file and its contents are not listed — so dragging `Iceland 2026`
 * onto the page would land as an empty selection with no explanation. The entry
 * API is what can see inside, and every browser this gallery runs in has it
 * under its original vendor-prefixed name.
 *
 * The entries have to be taken out of the DataTransfer synchronously, before
 * the first await: the list is emptied as soon as the drop event finishes
 * dispatching, and reading it one microtask later returns nothing.
 */
export async function filesFrom(transfer: DataTransfer): Promise<File[]> {
  const entries: FileSystemEntry[] = [];
  for (const item of Array.from(transfer.items ?? [])) {
    if (item.kind !== "file") continue;
    const entry = item.webkitGetAsEntry?.();
    if (entry) entries.push(entry);
  }
  if (entries.length === 0) return Array.from(transfer.files ?? []);

  const found: File[] = [];
  for (const entry of entries) await walk(entry, found);
  return found;
}

/** How deep a dropped folder is followed. */
const MAX_DEPTH = 8;

async function walk(entry: FileSystemEntry, into: File[], depth = 0): Promise<void> {
  if (entry.isFile) {
    const file = await asFile(entry as FileSystemFileEntry);
    // A directory the browser cannot open, or one that vanished mid-read. There
    // is no row to report it on — the row is made from the file — so the honest
    // thing is to leave it out rather than invent one.
    if (file) into.push(file);
    return;
  }
  if (!entry.isDirectory || depth >= MAX_DEPTH) return;

  // readEntries returns at most a hundred at a time and signals the end with an
  // empty batch, which is the one part of this API that is easy to get wrong.
  const reader = (entry as FileSystemDirectoryEntry).createReader();
  for (;;) {
    const batch = await new Promise<FileSystemEntry[]>((resolve) =>
      reader.readEntries(resolve, () => resolve([])),
    );
    if (batch.length === 0) return;
    for (const child of batch) await walk(child, into, depth + 1);
  }
}

function asFile(entry: FileSystemFileEntry): Promise<File | null> {
  return new Promise((resolve) =>
    entry.file(
      (file) => resolve(file),
      () => resolve(null),
    ),
  );
}

/** Whether a drag is carrying files, as opposed to selected text or a link. */
export function carriesFiles(transfer: DataTransfer | null): boolean {
  return Array.from(transfer?.types ?? []).includes("Files");
}
