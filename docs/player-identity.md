# Player identity: schema and rollout

Collection schemas live in the PocketBase instance, not in this repo, so the
changes below have to be made by hand in the admin UI before this code can run.

## 1. Environment

Set on the backend deployment:

    PLAYER_ID_HASH_PEPPER=<a long random string>

The process refuses to start without it. This is deliberate — see the comment on
`Hasher` in [internal/players/hash.go](../internal/players/hash.go): the raw
player IDs were publicly readable through the API, so an unkeyed digest would be
reversible by anyone who scraped them. The pepper is the only thing they lack.

**This value can never be rotated or lost.** Once the backfill has stripped the
raw IDs there is nothing left to re-derive the hashes from, and every link
between a player and their uploads is gone permanently. Back it up with the same
care as the database.

## 2. New collection: `players`

One record per Ingress account. The hash is the identity; the nickname is only
there so the admin panel has something readable to match against.

| Field | Type | Notes |
| --- | --- | --- |
| `player_hash` | text | required; **add a unique index** |
| `user` | relation → `users` | max 1, optional — set only by the verification flow |
| `last_ign` | text | most recently seen nickname |
| `last_faction` | text | plain text, not select: the value is client-supplied |
| `verified_at` | date | optional |

API rules:

- List / View: `user = @request.auth.id` — a signed-in agent can find only their
  own record.
- Create / Update / Delete: **empty (superusers only)**. The Go code writes
  through `app.Save`, which bypasses collection rules.

## 3. Relations on the existing collections

Add to **both** `medias` and `media_uploads`:

| Field | Type | Notes |
| --- | --- | --- |
| `player` | relation → `players` | max 1, optional |

Also add a **unique index on `medias.url_id`** if one does not already exist.
`ensureMedia` allocates the id and inserts inside a transaction, but without the
index a duplicate would still pass silently rather than being rejected.

## 4. Rollout order

1. Apply the schema above and set the pepper.
2. Deploy. New uploads are hashed and attributed from this point on; the raw IDs
   of existing records are untouched so far.
3. Dry-run the backfill and read the report:

       ./ingress-plus backfill-player-hashes --dry-run

   Against production data as of 2026-08-24 this reports 1754 medias scanned,
   22 scrubbed or without a player ID, and 33 trusted username mappings — 16
   above the trust threshold and 17 listed for manual assignment.
4. Run it for real:

       ./ingress-plus backfill-player-hashes

5. Assign the 17 sub-threshold usernames by hand, using the printed report.
6. Once verified, drop the now-redundant `media_uploads.agent_guid_hashed`
   column — it was added for this purpose but never populated.

The backfill is idempotent and can be re-run.

## Why attribution goes by username

The backfill does not read each record's own `original_data` to decide who
uploaded it. The 2024-01-11 mediagress.net import wrote payloads that do not
correspond to their `uploader_ign` — one username carries up to twelve different
player IDs there, and one player ID up to six usernames — so trusting those rows
individually would misattribute them.

Instead it derives a `username → player` map from the plugin-era records only,
where the data is perfectly consistent (33 usernames, 33 players, one to one),
requires at least 3 records before trusting a mapping, and then applies that map
by username across every record including the legacy ones. This is sound because
an Ingress username, once taken, is never released to another agent — so a
proven `(username, player)` pair holds for every record that username ever
touched. It is also the only way to attribute `media_uploads` at all, since that
table never stored a player ID.

A username that resolves to more than one player is dropped rather than guessed
at. Renames are the mirror image and are kept: one player legitimately appears
under several usernames, each of which was theirs alone.

The raw player ID is stripped from **every** record regardless of whether it
could be attributed — the legacy IDs are real people's IDs even where the
attribution around them is not.
