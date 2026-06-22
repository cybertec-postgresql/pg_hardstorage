# Cloudflare Worker — `get.pghardstorage.org`

Serves the one-line installer so this works:

```sh
curl -sSL https://get.pghardstorage.org | sh
```

The Worker (`get-installer-worker.js`) fetches `scripts/install.sh` from
the repo's `main` branch and returns it as `text/plain`, cached at the
edge. Editing `scripts/install.sh` on `main` updates the served
installer within the cache TTL — no Worker redeploy needed.

`wrangler.toml` here carries the name (`pghardstorage-get`), entrypoint,
and the `get.pghardstorage.org` custom-domain route, so a single deploy
wires up serving end-to-end.

## Setup — GitHub integration (recommended)

Cloudflare's Git integration ("Workers Builds") redeploys the Worker on
every push to `main`. One-time wiring in the Cloudflare dashboard:

1. **Workers & Pages → Create → Workers tab → Connect to Git**
   (a.k.a. "Import a repository").
2. Authorise GitHub and pick `cybertec-postgresql/pg_hardstorage`.
3. **Build settings:**
   - **Root directory:** `deploy/cloudflare`  ← where this `wrangler.toml` lives
   - **Build command:** *(leave empty — plain JS, nothing to build)*
   - **Deploy command:** `npx wrangler deploy` *(default)*
   - **Production branch:** `main`
4. **Save and Deploy.** The first deploy reads `wrangler.toml`, publishes
   the Worker, and binds `get.pghardstorage.org` automatically
   (`custom_domain = true` creates the DNS record + TLS cert).

Prerequisite: the `pghardstorage.org` zone is on this Cloudflare account.

## Setup — manual (fallback)

If you'd rather not connect Git, deploy from this directory with an
authenticated `wrangler` (`npm i -g wrangler && wrangler login`):

```sh
cd deploy/cloudflare
wrangler deploy        # reads wrangler.toml: name, entrypoint, route
```

## Verify (either path)

```sh
curl -sSL https://get.pghardstorage.org | head -20   # should print the script
curl -sSL https://get.pghardstorage.org | sh         # should install
```

## Notes

- **Pin to a release instead of `main`:** edit `INSTALL_SCRIPT_URL` in
  the Worker to `.../<tag>/scripts/install.sh` if you want the served
  installer frozen to a tagged release.
- **Cache TTL:** `CACHE_TTL_SECONDS` (default 300s) controls how fast an
  `install.sh` change propagates.
- This Worker only ever emits the installer script; it rejects non-GET
  methods and sends `X-Content-Type-Options: nosniff`.
