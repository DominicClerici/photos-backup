"use client";

import {
  Fragment,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import { Check, Images, ListPlus, Loader2, Plus, Search } from "lucide-react";

import type { Album, Bucket } from "@/lib/api";
import { useAlbums, useMembership } from "@/hooks/useAlbums";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuSub,
  ContextMenuSubContent,
  ContextMenuSubTrigger,
} from "@/components/ui/context-menu";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

/**
 * "Add to album", wherever a selection can be acted on.
 *
 * The same list is reached two ways — a submenu off the grid's context menu,
 * and a dropdown off a button in the selection sheet — because those are the
 * two places the rest of the filing actions already live. The rows are built
 * once, here, and each wrapper only decides which menu primitive draws them:
 * a context menu's items and a dropdown's are two components with one meaning,
 * and nothing about *what* the menu says should depend on which one it is.
 *
 * Search is inside the menu rather than in front of it. An archive with sixty
 * albums is a scroll; an archive with six is a glance, and putting a dialog
 * between somebody and six rows would be worse for the common case in order to
 * be slightly better for the rare one. Typing anywhere in the menu goes to the
 * box — see the keyboard handling below, which is most of what makes an input
 * inside a menu behave the way people expect.
 */

/** One row of the list, whichever menu is drawing it. */
interface Row {
  key: string;
  icon: ReactNode;
  label: string;
  /** Drawn with a tick: this photograph is already in that album. */
  ticked: boolean;
  /** A separator goes above this row. */
  divides?: boolean;
  run: () => void;
}

export interface AlbumPickerProps {
  /**
   * Which half of the archive the albums come from, and go into. Undefined is
   * the library's own. A hidden photograph can only be filed into a hidden
   * album — the server refuses anything else — so this decides the list as much
   * as it decides the request.
   */
  bucket?: Bucket;
  /**
   * The one photograph the menu is about, or null when it is about several.
   *
   * Set, the rows carry ticks for the albums it is already in and clicking a
   * ticked one takes it back out. Null, they do not: a selection of forty has
   * forty answers, and a tick that meant "some of them" is not a thing anybody
   * wants to read off a menu.
   */
  assetId: string | null;
  onAdd: (album: Album) => void;
  /**
   * Takes it back out. Only reachable through a tick, so a picker that cannot
   * draw ticks — one about several photographs at once — has no use for it.
   */
  onRemove?: (album: Album) => void;
  /** Opens the create dialog, with whatever was typed into the search box. */
  onCreate: (name: string) => void;
  disabled?: boolean;
}

/**
 * The submenu, for the grid's context menu.
 *
 * The open state is held rather than left to the primitive because two things
 * need it: the album list, which is not fetched until somebody actually opens
 * this, and the search box, which has to be focused and emptied on each open.
 */
export function AddToAlbumSub(props: AlbumPickerProps) {
  const [open, setOpen] = useState(false);
  const picker = usePicker(props, open);

  return (
    <ContextMenuSub open={open} onOpenChange={setOpen}>
      <ContextMenuSubTrigger disabled={props.disabled}>
        <ListPlus />
        Add to album
      </ContextMenuSubTrigger>
      <ContextMenuSubContent className="w-64 p-0 shadow-lg">
        <PickerBody picker={picker}>
          {picker.rows.map((row) => (
            <Fragment key={row.key}>
              {row.divides ? <ContextMenuSeparator /> : null}
              <ContextMenuItem onClick={row.run} className="px-2">
                <RowLabel row={row} />
              </ContextMenuItem>
            </Fragment>
          ))}
        </PickerBody>
      </ContextMenuSubContent>
    </ContextMenuSub>
  );
}

/**
 * The same list as a dropdown, for the sheet above the selection pill.
 *
 * The sheet is buttons rather than a menu — see SelectionPill — so this one
 * brings its own trigger with it and looks like the controls beside it.
 */
export function AddToAlbumMenu(props: AlbumPickerProps) {
  const [open, setOpen] = useState(false);
  const picker = usePicker(props, open);

  return (
    <DropdownMenu open={open} onOpenChange={setOpen}>
      <DropdownMenuTrigger
        disabled={props.disabled}
        render={<Button type="button" size="sm" variant="outline" />}
        className="w-full justify-start gap-2"
      >
        <ListPlus aria-hidden="true" />
        Add to album
      </DropdownMenuTrigger>
      <DropdownMenuContent side="top" align="start" className="w-60 p-0">
        <PickerBody picker={picker}>
          {picker.rows.map((row) => (
            <Fragment key={row.key}>
              {row.divides ? <DropdownMenuSeparator /> : null}
              <DropdownMenuItem onClick={row.run} className="px-2">
                <RowLabel row={row} />
              </DropdownMenuItem>
            </Fragment>
          ))}
        </PickerBody>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function RowLabel({ row }: { row: Row }) {
  return (
    <>
      {row.icon}
      <span className="min-w-0 flex-1 truncate">{row.label}</span>
      {row.ticked ? <Check className="shrink-0 text-muted-foreground" aria-hidden="true" /> : null}
    </>
  );
}

interface Picker {
  rows: Row[];
  query: string;
  setQuery: (next: string) => void;
  loading: boolean;
  error: string | null;
  input: React.RefObject<HTMLInputElement | null>;
  onKeyDownCapture: (ev: ReactKeyboardEvent) => void;
}

/**
 * The search box and the rows under it, inside whichever popup drew them.
 *
 * The keydown handler is on this wrapper and in the capture phase deliberately.
 * Base UI's menu owns keyboard on the popup — typeahead jumps to an item whose
 * label starts with what you typed, which is exactly the wrong thing to do to
 * somebody typing into a search box six pixels above it. Capturing here, one
 * element inside the popup, is what lets the box win without turning the menu's
 * own arrow keys off.
 */
function PickerBody({ picker, children }: { picker: Picker; children: ReactNode }) {
  return (
    <div onKeyDownCapture={picker.onKeyDownCapture}>
      <div className="flex items-center gap-2 border-b px-2.5 py-1.5">
        <Search className="size-3.5 shrink-0 text-faint" aria-hidden="true" />
        <Input
          ref={picker.input}
          value={picker.query}
          onChange={(ev) => picker.setQuery(ev.target.value)}
          placeholder="Search albums"
          aria-label="Search albums"
          autoComplete="off"
          spellCheck={false}
          className="h-6 rounded-none border-0 bg-transparent px-0 text-[13px] shadow-none focus-visible:border-0 focus-visible:ring-0 dark:bg-transparent"
        />
      </div>

      <div className="max-h-72 overflow-y-auto overscroll-contain p-1">
        {picker.error ? (
          <p className="px-2 py-2 text-xs text-destructive">{picker.error}</p>
        ) : picker.loading ? (
          <p className="flex items-center gap-2 px-2 py-2 text-xs text-faint">
            <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            Loading albums
          </p>
        ) : (
          children
        )}
      </div>
    </div>
  );
}

/**
 * Everything the two wrappers share: the list, the ticks, the filter, and the
 * keyboard.
 */
function usePicker(
  { bucket, assetId, onAdd, onRemove, onCreate }: AlbumPickerProps,
  open: boolean,
): Picker {
  const { albums, error } = useAlbums(bucket, open);
  const { held, mark } = useMembership(assetId, open);
  const [query, setQuery] = useState("");
  const input = useRef<HTMLInputElement>(null);

  // A menu opens on whatever was typed into it last otherwise, which is a
  // filtered list somebody has to clear before they can see anything.
  useEffect(() => {
    if (!open) {
      setQuery("");
      return;
    }
    // After the primitive has placed focus, not before: the menu moves focus to
    // itself as it opens, and racing that just loses.
    const frame = requestAnimationFrame(() => input.current?.focus());
    return () => cancelAnimationFrame(frame);
  }, [open]);

  const rows = useMemo<Row[]>(() => {
    const trimmed = query.trim();
    const needle = trimmed.toLowerCase();
    const matches = (albums ?? []).filter((album) =>
      album.title.toLowerCase().includes(needle),
    );

    const create: Row = {
      key: "create",
      icon: <Plus aria-hidden="true" />,
      label: trimmed ? `Create “${trimmed}”` : "Create album",
      ticked: false,
      run: () => onCreate(trimmed),
    };

    // With nothing typed, making a new album is the first thing offered, above
    // the list. With something typed it is the last, under the matches — the
    // thing you meant is almost certainly one of them, and an archive where it
    // is not says so by there being no matches at all.
    const rows: Row[] = trimmed ? [] : [create];
    matches.forEach((album, i) => {
      const ticked = held?.has(album.id) === true;
      rows.push({
        key: album.id,
        icon: <Images aria-hidden="true" />,
        label: album.title,
        ticked,
        divides: !trimmed && i === 0,
        run: () => {
          mark(album.id, !ticked);
          if (ticked) onRemove?.(album);
          else onAdd(album);
        },
      });
    });
    if (trimmed) rows.push({ ...create, divides: matches.length > 0 });
    return rows;
  }, [albums, held, query, mark, onAdd, onRemove, onCreate]);

  // Typing anywhere in the menu goes to the box, including after the pointer
  // has moved over a row and taken focus with it. Without this, the letters
  // somebody types after hovering land in the menu's own typeahead and the
  // search box quietly stops working halfway through a word.
  const onKeyDownCapture = useCallback(
    (ev: ReactKeyboardEvent) => {
      const inBox = ev.target === input.current;

      switch (ev.key) {
        // The menu's own: closing, moving between rows, leaving.
        case "Escape":
        case "Tab":
        case "ArrowDown":
        case "ArrowUp":
          return;
        // A caret, when there is a caret to move. Left otherwise closes the
        // submenu, which is right when a row is focused and infuriating when
        // somebody is editing what they typed.
        case "ArrowLeft":
        case "ArrowRight":
        case "Home":
        case "End":
          if (inBox) ev.stopPropagation();
          return;
        case "Enter":
          // A focused row answers for itself. From the box, Enter takes the
          // first thing in the list, which is the match when there is one and
          // "Create …" when there is not.
          if (!inBox) return;
          ev.preventDefault();
          ev.stopPropagation();
          rows[0]?.run();
          return;
        case "Backspace":
          ev.stopPropagation();
          if (!inBox) {
            ev.preventDefault();
            setQuery((q) => q.slice(0, -1));
            input.current?.focus();
          }
          return;
      }

      if (ev.ctrlKey || ev.metaKey || ev.altKey || ev.key.length !== 1) return;
      // Not prevented when the box already has it: the character has to reach
      // the input to be typed. Only stopped, so the menu's typeahead does not
      // also act on it.
      ev.stopPropagation();
      if (!inBox) {
        ev.preventDefault();
        const typed = ev.key;
        setQuery((q) => q + typed);
        input.current?.focus();
      }
    },
    [rows],
  );

  return {
    rows,
    query,
    setQuery,
    loading: albums === null && error === null,
    error,
    input,
    onKeyDownCapture,
  };
}
