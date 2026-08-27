"use client";

// The unlock is server-wide process state rather than per-session — see
// internal/api/vault.go — which is why this polls rather than remembering that
// it unlocked something. The phone will show the same state, and either can
// change it under the other.
import "@/lib/archive";

export {
  askToUnlock,
  BUCKET_LABEL,
  BUCKET_VERB,
  closeGate,
  needsVault,
  onGate,
  useVault,
  type VaultState,
} from "@photobackup/core/react";
