# IITC plugin store: schema and rollout

The store lets agents publish IITC plugins on ingress.plus. A single
userscript, the Ingress Plus sideloader, runs inside IITC on
intel.ingress.com. It signs in to ingress.plus, asks which plugins the agent is
allowed to use, and loads the ones they turned on.

Collection schemas live in the PocketBase instance, so the collections below
have to be created by hand in the admin UI before this code can run. For a
local `pb_data`, `go run . iitc-setup` creates the same collections
([internal/iitc/schema.go](../internal/iitc/schema.go)), and the tests use it
too. If the two ever disagree, the code is right and this page is stale.

## 1. Environment

    IITC_WATERMARK_KEY=<a long random string>
    TELEGRAM_TOPIC_PLUGINS=<thread id>   # optional, review notifications

The process refuses to start without `IITC_WATERMARK_KEY`. Every copy of a
plugin the store serves is stamped with an HMAC of the requesting user's id.
With the key an admin can tell whose copy leaked; without it the stamp is just
a hash of an id that's visible on public profiles. Rotating the key only stops
older leaked copies from being traced, so it isn't as critical as
`PLAYER_ID_HASH_PEPPER`, but keep it stable.

## 2. Collections

### `iitc_plugins`

| Field | Type | Notes |
| --- | --- | --- |
| `slug` | text | required, 3-40 chars, pattern `^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`, **unique index**. Can't be changed after creation: the sideloader remembers enabled plugins by slug |
| `name` | text | required, max 80 |
| `description` | text | markdown, max 20000 |
| `category` | text | max 40 |
| `homepage_url` | url | |
| `icon` | file | max 1, 256 KB, png/jpeg/svg/webp. Public (not protected) |
| `author` | relation → `users` | required, max 1, cascade delete |
| `status` | select | `draft`, `pending_review`, `approved`, `rejected`, `disabled` |
| `access` | json | `{"rules": [...], "blockedUserIds": [...]}`, see below |
| `current_version` | relation → `iitc_plugin_versions` | max 1. The version being served |
| `review_notes` | text | max 5000. Set by admins |
| `created` / `updated` | autodate | |

API rules:

- List / View: `author = @request.auth.id`
- Create: `@request.auth.id != "" && @request.auth.collectionName = "users"`
- Update / Delete: `author = @request.auth.id`

List and View are author-only on purpose. A REST list rule can't express
"matches one of these JSON rules", so other agents reach a plugin only through
`/api/plugins` and `/api/iitc/*`, which check the access rules in Go.

### `iitc_plugin_versions`

| Field | Type | Notes |
| --- | --- | --- |
| `plugin` | relation → `iitc_plugins` | required, max 1, cascade delete |
| `version` | text | required, semver pattern `^\d+\.\d+\.\d+([-.+][0-9A-Za-z.-]+)?$` |
| `changelog` | text | max 5000 |
| `file` | file | required, max 1, 1 MB, **protected** |
| `sha256` | text | filled in by the server |
| `status` | select | `pending`, `approved`, `rejected` |
| `review_notes` | text | max 5000 |
| `created` / `updated` | autodate | |

Unique index on `(plugin, version)`.

API rules:

- List / View / Create: `plugin.author = @request.auth.id`
- Update / Delete: **empty (superusers only)**. Versions are immutable, so
  what was reviewed is exactly what gets served.

### `iitc_tokens`

| Field | Type | Notes |
| --- | --- | --- |
| `user` | relation → `users` | required, max 1, cascade delete |
| `label` | text | device name, max 60 |
| `token_hash` | text | required, **hidden**, **unique index** |
| `last_used_at` | date | updated at most every 10 minutes |
| `created` | autodate | |

API rules:

- List / View / Delete: `user = @request.auth.id`
- Create / Update: **empty**. Tokens are minted by `POST /api/iitc/tokens`.

Revoking a device means deleting its token record. Only the SHA-256 of a token
is stored; the plaintext exists once, in the response that creates it.

### Feature flag

Add a `feature_flags` record `PLUGIN_STORE_ENABLED` (off). The frontend hides
the store until it's turned on.

## 3. Access rules

`access.rules` uses the same shape as `email_campaigns.targeting.rules`, so the
frontend shares one editor between the two. Rules are OR'd together.

| `type` | Grants access to |
| --- | --- |
| `all` | every signed-in agent |
| `user` | the ids in `userIds` |
| `supporter` | `users.supporter = true` |
| `verified` | `users.verification != ""` |
| `faction` | verified agents whose `users.faction` is in `factions` (`enlightened`, `resistance`, `machina`) |

`access.blockedUserIds` is an optional list of user ids (max 500) that never
get access, whatever the rules say. It lets an author open a plugin to every
agent but a few. Blocked agents get the same 404 as everyone else without
access.

The author always has access to their own plugin, even if they put themselves
on the blocked list. With no rules, nobody else does.

Faction only counts for verified agents because users can edit their own
faction, but not while they're verified. **Check that the `users` update rule
really prevents an agent from setting `verification`, `supporter`, or (while
verified) `faction` on themselves.** Otherwise anyone could give themselves
access to a gated plugin.

An agent without access never learns the plugin exists. It doesn't appear in
either catalog, and the detail and load routes answer 404 for it, the same as
for a slug that doesn't exist.

## 4. Lifecycle

1. The author creates the plugin through the REST API. The server forces
   `author` to the caller and `status` to `draft`.
2. They upload a version: a `.js` userscript with a `// ==UserScript==`
   header whose `@version` matches the `version` field. The server sets
   `status = pending` and computes `sha256`. Only one version per plugin can
   be pending at a time.
3. A first pending version moves the plugin to `pending_review`. A plugin
   that's already published keeps serving its current version while an update
   is reviewed.
   Exception: when the uploader is an admin, meaning a verified agent whose
   email is also a superuser's, the version is created `approved` and goes
   live straight away (it also skips the one-pending-version limit).
4. While a version is pending, the author can already load it in their own
   IITC, so they can test it before review.
5. A superuser sets the version to `approved` (or `rejected` with
   `review_notes`). Approving makes it `current_version` and the plugin
   `approved`. Rejecting the version currently served falls back to the
   previous approved one, or unpublishes the plugin if there's none.
6. The author can unpublish (`approved → draft`) and republish
   (`draft → approved`, only if a version was approved before). `disabled` is
   an admin takedown that the author can't undo.

Metadata edits (name, description, icon, access rules) aren't reviewed and go
live immediately. Admins can disable a plugin that abuses this.

## 5. Routes

| Route | Auth | |
| --- | --- | --- |
| `GET /api/plugins` | PocketBase user | store catalog: the plugins the caller can access |
| `GET /api/plugins/{slug}` | PocketBase user | detail page and version history; 404 without access |
| `POST /api/iitc/tokens` | PocketBase user | `{label}` → `{id, label, token}` |
| `GET /api/iitc/me` | IITC token | `{username, faction, verified}` |
| `DELETE /api/iitc/tokens/current` | IITC token | revokes the token used for the call (the sideloader's Disconnect) |
| `GET /api/iitc/plugins` | IITC token | sideloader catalog, same filtering as the store |
| `GET /api/iitc/plugins/{slug}` | IITC token | same payload as `/api/plugins/{slug}`, for the changelog in the IITC store; 404 without access |
| `POST /api/iitc/plugins/{slug}/load` | IITC token | `{clientPub, version?, encodings?}` → `{slug, encoding, version, dev, serverPub, iv, ciphertext}` |

IITC tokens are sent as `Authorization: Bearer ipx_...`.

### What the encryption does

The sideloader generates an ECDH P-256 key pair for each load and sends the
public half. The server generates its own, derives an AES-256-GCM key with
HKDF-SHA256 (empty salt, info `ingress-plus-iitc-v1`) and returns the
watermarked code encrypted under it. A captured response can't be decrypted
later, and the code never shows up in plain text in the browser's network log
or in a proxy.

The browser still has to decrypt the code to run it, so an agent with
devtools open can read it. The watermark is what lets an admin trace a leaked
copy back to its owner.

### What gets encrypted

The served copy isn't byte for byte the reviewed file. Before the watermark
goes on, the code runs through esbuild to drop its comments
(`iitc.StripComments`). The `==UserScript==` block and legal comments
(`/*! */`, `@license`, `@preserve`) stay. esbuild reprints the code without
minifying or renaming anything. If it can't parse a file, the original is
served and a warning is logged. Results are cached in memory per version,
since versions can't change after upload.

Ciphertext doesn't compress, so HTTP compression does nothing for `/load`.
When the request has `"encodings": ["gzip"]`, the server gzips the
watermarked code before sealing it and answers `"encoding": "gzip"`. The
sideloader inflates it with `DecompressionStream`. Without the field
(sideloader 2.0.0 and older) the answer is `"encoding": "identity"`.

## 6. Production checklist

- Create the three collections and the feature flag above.
- Set `IITC_WATERMARK_KEY` (and optionally `TELEGRAM_TOPIC_PLUGINS`).
- CORS: the sideloader calls `/api/iitc/*` from `https://intel.ingress.com`.
  PocketBase allows every origin by default; if the server is started with
  `--origins`, add that origin to the list.
- Rate limits (Settings → Rate limiting): add rules for `POST /api/iitc/tokens`
  and `/api/iitc/`, for example 30 requests / 60 s per IP.
- Check the `users` update rule (see section 3).
