/**
 * Cloudflare Worker for https://get.pghardstorage.org
 *
 * Serves the canonical install script as text/plain so that
 *
 *     curl -sSL https://get.pghardstorage.org | sh
 *
 * pipes the real installer into the shell.  The script body is fetched
 * from the repo's main branch (raw.githubusercontent.com) and cached at
 * the edge, so updating scripts/install.sh on main updates the served
 * installer without redeploying this Worker.
 *
 * Why a Worker rather than a plain redirect:
 *   - We can pin the Content-Type to text/plain (some clients choke on
 *     GitHub raw's charset quirks; `curl | sh` doesn't care, but a
 *     human opening the URL in a browser gets readable text).
 *   - We control caching + can swap the upstream (e.g. pin to a tag)
 *     in one place.
 *   - No dependence on a 30x redirect surviving every client's flags.
 *
 * Deploy: see deploy/cloudflare/README.md.
 */

// Source of truth for the installer.  Pin to a tag instead of `main`
// (e.g. .../v1.0.0/scripts/install.sh) if you want the served installer
// frozen to a release rather than tracking main.
const INSTALL_SCRIPT_URL =
  "https://raw.githubusercontent.com/cybertec-postgresql/pg_hardstorage/main/scripts/install.sh";

// Edge cache lifetime for the fetched script (seconds).  Short enough
// that a fix to install.sh propagates quickly, long enough to absorb
// install spikes without hammering the origin.
const CACHE_TTL_SECONDS = 300;

export default {
  async fetch(request) {
    // Only GET/HEAD make sense for a script endpoint.
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("Method Not Allowed\n", {
        status: 405,
        headers: { Allow: "GET, HEAD", "Content-Type": "text/plain" },
      });
    }

    const upstream = await fetch(INSTALL_SCRIPT_URL, {
      cf: { cacheTtl: CACHE_TTL_SECONDS, cacheEverything: true },
    });

    if (!upstream.ok) {
      return new Response(
        "Installer temporarily unavailable. " +
          "See https://github.com/cybertec-postgresql/pg_hardstorage/releases\n",
        { status: 502, headers: { "Content-Type": "text/plain" } }
      );
    }

    const body = await upstream.text();

    return new Response(body, {
      status: 200,
      headers: {
        // text/plain so `curl | sh` and a browser both behave.
        "Content-Type": "text/plain; charset=utf-8",
        "Cache-Control": `public, max-age=${CACHE_TTL_SECONDS}`,
        // Defensive headers — this endpoint only ever emits a script.
        "X-Content-Type-Options": "nosniff",
        "Referrer-Policy": "no-referrer",
      },
    });
  },
};
