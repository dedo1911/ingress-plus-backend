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

Both upload endpoints hash. v1 is the legacy route ported out of `pb_hooks` and
still serves clients that were never updated to v2 — it was carrying most of the
traffic — so leaving it unhashed would have kept writing raw player IDs into new
records.

1. Apply the schema above and set the pepper.
2. Deploy. New uploads on **both v1 and v2** are hashed and attributed from this
   point on; the raw IDs of existing records are untouched so far.
3. Dry-run the backfill and read the report:

       ./ingress-plus backfill-player-hashes --dry-run

   Against production data as of 2026-08-24 this reports 1754 medias scanned,
   22 scrubbed or without a player ID, 81 distinct player IDs, and 33 trusted
   username mappings — 16 above the trust threshold and 17 listed for manual
   assignment.
4. Run it for real:

       ./ingress-plus backfill-player-hashes

5. Assign the 17 sub-threshold usernames by hand, using the printed report.
   The 48 unattributed records need nothing: they fill themselves in.
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

## Hashes without an owner

A `players` record is created for **every** player ID in the table — 81 of them
— not just the 33 that could be tied to a username. The other 48 appear only in
the legacy import, so there is no honest way to say whose uploads they are, and
they are created bare: no `last_ign`, no relations, nothing claimed.

They are kept because the hash is stable. If one of those agents ever uploads
again, the live path hashes their player ID, finds the record already sitting
there, and fills in the nickname — at which point an admin can link their
history. Throwing the hash away at backfill time would forfeit that for nothing,
since the raw ID is being stripped either way.

This is why `players.Ensure` treats an empty nickname as "leave what's there
alone" rather than as a value to write: the backfill passes one for all 48, and
it must not erase a nickname a trusted record already proved.

The raw player ID is stripped from every record that holds a **real** one,
regardless of whether it could be attributed — the legacy IDs are real people's
IDs even where the attribution around them is not.

## What counts as a player ID

A player ID is a 32-character lowercase hex UUID followed by `.c`, e.g.
`cb3773292130450080439d75cdcff215.c`. Anything else is invalid and is never
hashed: hashing a shared non-ID value would collapse every record carrying it
onto one hash, which would then read as a single extremely prolific agent.

Validation is a format check, not a list of known scrub markers, so it also
rejects whatever wording a future manual scrub happens to use. Verified against
the full table as of 2026-08-24:

| | Count |
| --- | --- |
| Well-formed IDs (all lowercase, all `.c`, no exceptions) | 1732 |
| No `playerId` key at all (13 `kidobarrett`, 7 `NaiRoH`) | 20 |
| `playerId: ""` | 1 |
| `playerId: "USER DELETED"` | 1 |

Records whose `playerId` is not well-formed are left **completely untouched** by
the backfill. There is nothing in them left to protect, and overwriting a
moderator's marker would erase the evidence that the removal was deliberate
rather than data that never carried an ID.

IDs are lowercased before hashing so that the same UUID in different cases can
never fork into two identities.
