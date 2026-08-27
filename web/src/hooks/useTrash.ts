"use client";

// Imported for the side effect: this is the hook that reports what a write did,
// and core's notifier has to be the app's toast before the first one lands.
import "@/lib/archive";
import "@/lib/notify";

export { useTrashActions } from "@photobackup/core/react";
