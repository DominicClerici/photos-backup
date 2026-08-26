import type { NextConfig } from "next"

// photod is the front door, and this app sits behind it.
//
// It used to be the other way round: Next rewrote /api/* to a plaintext photod
// listener on loopback, and that listener served the whole archive — every
// photograph, the trash, the vault — to anything that could open the port. It
// was safe only because the port was bound to 127.0.0.1 and both processes were
// on the same machine, and it stopped being safe the moment the gallery needed
// to be reachable from anywhere else.
//
// So the rewrite is gone and the plaintext listener with it. photod terminates
// TLS, authenticates every request against a passkey session or a device token,
// serves /v1 and the media itself, and reverse-proxies everything else here.
// The browser therefore reaches the bundle, the JSON and the thumbnails at one
// origin under one cookie — which is the constraint PROJECT.md records from
// Phase 12, because a browser attaches a same-origin cookie to an <img> and
// will not attach a bearer header to one.
//
// What that means in practice: run photod and open photod's address. Opening
// this app's own port directly gets you a gallery with no API behind it. Hot
// reload still works — Go's ReverseProxy passes the upgrade through.
const nextConfig: NextConfig = {
  // Opt out of `next dev` auto-regenerating AGENTS.md/CLAUDE.md every run.
  agentRules: false,

  // A self-contained server, traced down to the modules actually imported.
  // The deployed tree is then a few megabytes that `next start` never has to
  // resolve a dependency out of — which is what lets the photoweb unit run
  // under ProtectSystem=strict with one writable path, and what keeps a
  // redeploy from needing a package manager on the archive machine at all.
  output: "standalone",
}

export default nextConfig
