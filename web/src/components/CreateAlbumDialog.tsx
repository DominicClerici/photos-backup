"use client";

import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Loader2 } from "lucide-react";

import { ApiError, createAlbum, type Bucket, type CreatedAlbum, type Target } from "@/lib/api";
import { albumsChanged } from "@/hooks/useAlbums";
import { needsVault } from "@/hooks/useVault";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";

/** The one dialog every "Create album" in the app opens. */
export interface CreateAlbumRequest {
  /**
   * What the name box starts with. The search box in the "Add to album" menu
   * hands over whatever was typed into it, so "Create 'Iceland'" opens on a
   * form that already says Iceland and needs one keystroke to finish.
   */
  name: string;
  /** Which half of the archive the album is made in. */
  bucket?: Bucket;
  /**
   * What to put in it, when the dialog was opened from a selection. Captured at
   * the moment the menu item was clicked rather than read on submit: by the
   * time somebody has typed a name the selection may be gone, and every
   * position in it would mean a different photograph anyway.
   */
  target?: Target;
}

/**
 * Making an album, as the only surface in the app that asks for a name.
 *
 * Two fields and nothing else, because an album is two fields. The description
 * is optional and rarely used — an import fills it from a Takeout's per-folder
 * metadata and most people never type one — so it is here rather than hidden
 * behind a disclosure, and simply left empty.
 *
 * @param request What to make, or null while the dialog is shut. One object
 * rather than an `open` flag beside three props, so a dialog that is open
 * cannot be open about nothing.
 * @param onCreated Called with the album once it exists. The caller decides
 * what that means — the collections page opens it, a menu over a selection
 * stays put and says what went in.
 */
export function CreateAlbumDialog({
  request,
  onClose,
  onCreated,
}: {
  request: CreateAlbumRequest | null;
  onClose: () => void;
  onCreated: (album: CreatedAlbum) => void;
}) {
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  // Reset on each open rather than on close, so the fields do not visibly empty
  // themselves during the closing animation.
  useEffect(() => {
    if (!request) return;
    setTitle(request.name);
    setDescription("");
    setError(null);
    setSaving(false);
  }, [request]);

  const submit = useCallback(
    (ev: FormEvent) => {
      ev.preventDefault();
      if (!request || saving) return;

      const name = title.trim();
      if (!name) {
        setError("An album needs a name.");
        return;
      }

      setSaving(true);
      setError(null);
      createAlbum({ title: name, description: description.trim(), bucket: request.bucket }, request.target)
        .then((album) => {
          albumsChanged();
          onClose();
          onCreated(album);
        })
        .catch((err: unknown) => {
          setSaving(false);
          // A locked vault is already a password prompt on screen; a second
          // error under the name field would be telling somebody off for a
          // thing they are in the middle of doing.
          if (needsVault(err)) {
            onClose();
            return;
          }
          // 409 is the archive saying the name is taken, which belongs under
          // the field somebody typed it into rather than in a toast across the
          // screen from it.
          if (err instanceof ApiError && err.status === 409) {
            setError("An album with that name already exists.");
            return;
          }
          setError(err instanceof Error ? err.message : "Could not create the album.");
        });
    },
    [request, saving, title, description, onClose, onCreated],
  );

  // Whether anything is going in with it, which is the whole difference between
  // the two sentences under the heading.
  const filling = request?.target !== undefined;

  return (
    <Dialog open={request !== null} onOpenChange={(open) => (open ? undefined : onClose())}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4">
          <DialogHeader>
            <DialogTitle>New album</DialogTitle>
            <DialogDescription>
              {filling
                ? "The photos you selected go in as soon as it exists."
                : "It starts empty. Add photos to it from any grid."}
            </DialogDescription>
          </DialogHeader>

          <Field data-invalid={error !== null ? true : undefined}>
            <FieldLabel htmlFor="album-name">Name</FieldLabel>
            <Input
              id="album-name"
              value={title}
              onChange={(ev) => {
                setTitle(ev.target.value);
                setError(null);
              }}
              placeholder="Iceland 2026"
              autoFocus
              autoComplete="off"
              maxLength={200}
              aria-invalid={error !== null}
            />
            {error ? <FieldError>{error}</FieldError> : null}
          </Field>

          <Field>
            <FieldLabel htmlFor="album-description">Description</FieldLabel>
            <Textarea
              id="album-description"
              value={description}
              onChange={(ev) => setDescription(ev.target.value)}
              placeholder="Optional"
              maxLength={2000}
              rows={2}
            />
            <FieldDescription>Shown at the top of the album.</FieldDescription>
          </Field>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose} disabled={saving}>
              Cancel
            </Button>
            <Button type="submit" disabled={saving || title.trim() === ""}>
              {saving ? <Loader2 className="animate-spin" aria-hidden="true" /> : null}
              Create album
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
