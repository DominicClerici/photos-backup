import type { NextConfig } from "next"

// photod is a separate process, so the browser has to reach two origins. The
// rewrite makes the JSON API same-origin, which sidesteps CORS entirely — there
// is no preflight, no allowed-origin list, and nothing to get wrong when this
// eventually sits behind a real domain.
//
// Media is exempt from that reasoning: <img> and <video> are not subject to CORS
// unless you ask them to be, so thumbnails can point straight at photod and skip
// the proxy hop. See NEXT_PUBLIC_MEDIA_BASE in src/lib/api.ts.
//
// Port 8788, not 8787. Since Phase 5 photod serves :8787 over HTTPS with a
// certificate it signs itself, and puts the read endpoints the gallery needs on a
// plaintext loopback listener alongside it. Proxying to the TLS port would mean
// teaching Node to trust a private CA to render a thumbnail, which is exactly what
// the second listener exists to avoid.
const photod = process.env.PHOTOD_URL ?? "http://localhost:8788"

const nextConfig: NextConfig = {
  // Opt out of `next dev` auto-regenerating AGENTS.md/CLAUDE.md every run.
  agentRules: false,
  async rewrites() {
    return [{ source: "/api/:path*", destination: `${photod}/:path*` }]
  },
}

export default nextConfig
