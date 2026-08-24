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

Also add two unique indexes to `medias`:

| Index | Why |
| --- | --- |
| unique on `media_id` | Niantic's id is the natural key. Without it two uploads of the same new Media can each create a record. |
| unique on `url_id` | The public identifier in `/media/<url_id>`. A duplicate means two records answer to the same URL. |

**Both will be rejected until the existing duplicates are removed** — see
"Cleaning up the duplicate media" below.

`createMedia` allocates the url_id and inserts inside a transaction, but the
existence check that precedes it runs outside, so two requests can both pass it.
The indexes are what actually enforce uniqueness; the code turns their rejection
into "someone else discovered it first" rather than a 500.

## Cleaning up the duplicate media

Production carries 7 duplicated `media_id`s, all created within two seconds of
each other on 2024-11-01 by a single agent's upload — concurrent requests that
each passed the existence check. Five pairs were assigned consecutive url_ids;
two pairs collided on the same url_id, which is what blocks the index.

| media_id | keep | delete | url_id of the deleted copy | media_uploads rows on it |
| --- | --- | --- | --- | --- |
| 4959 | `1h2kge9o6euz1uf` | `a8kpohh37vrhibe` | 12464 | 0 |
| 4962 | `7uzzky146p1uis5` | `520ifc8hpbiglgd` | 12465 (shared) | — |
| 4968 | `go9vt10pz28dbu8` | `10r1kw5udfh3419` | 12469 | 0 |
| 4969 | `8tcwpijphxeyr8z` | `one7kk247inuhrz` | 12471 | 0 |
| 4970 | `d802za8o34eo331` | `60lsek843jhypf4` | 12473 | 0 |
| 4971 | `z5btpnv8jrb0mk3` | `pfcl750kjvdbixg` | 12475 | 0 |
| 5003 | `sozbjsmk9gt8ji7` | `vaoixtf4nvblmsv` | 12489 (shared) | — |

Each pair is byte-identical — same media_id, short_description, level, topic and
destination — so nothing is lost by dropping one. The copy to keep is the one
holding the url_id that `media_uploads` rows actually reference; for the five
consecutive-id pairs the second record has no rows pointing at it at all, and
for the two shared-id pairs the single row survives either way because it
references the url_id, not the record.

Delete the seven records in the right-hand column, then add the indexes.

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
6. Once verified, drop `media_uploads.agent_guid_hashed`. It was added for this
   purpose and never populated; the `player` relation supersedes it.
7. Retiring v1 is a **separate deploy**. Doing it in the same release as the
   hashing would break every out-of-date plugin at the same moment the backfill
   runs, and those are two unrelated things to debug at once. Ship hashing,
   confirm the backfill, then ship the retirement.

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


---

# Decided, not yet built

Design decisions taken while building the hashing, recorded so the follow-up
work does not have to re-litigate them.

## Deduplicating media_uploads by identity

`media_uploads` is uniquely indexed on `(media_url_id, uploader_ign)`, so a
renamed agent gets a **second row** for a media they had already uploaded. That
inflates `total_media_uploads`, splits them on any ign-grouped leaderboard, and
makes `firstTimeUserUploadCount` overcount for that upload.

The fix is to dedupe on the identity instead:

- `ensureUpload` looks up `(media_url_id, player)` first when a player is known,
  falling back to `(media_url_id, uploader_ign)` when it is not.
- On a match, the existing row is **left untouched** - `uploader_ign` stays
  frozen at the name used at the time. `players.last_ign` carries the current
  one, so nothing is lost and 11877 historical rows are not rewritten.
- Add a **partial** unique index:

      CREATE UNIQUE INDEX idx_media_uploads_player
        ON media_uploads (media_url_id, player) WHERE player != ''

  Partial is not optional. PocketBase stores an unset relation as `''`, not
  NULL, and roughly half the rows will never have an identity - a plain unique
  index over those collides immediately and is rejected.

## Displaying the current username

`medias.uploader_ign` is the name at upload time. To show the *current* name,
read it through the relation - `medias.player` -> `players.last_ign` - and fall
back to `uploader_ign` when there is no player or `last_ign` is empty (the
unattributed records have no nickname).

Two schema changes make this possible:

- `players.viewRule` must become public (`""`). Expand enforces the *related*
  collection's view rule, so with `user = @request.auth.id` a logged-out visitor
  cannot expand the relation at all.
- `player_hash` and `user` must be marked **hidden**, which strips them from API
  responses for everyone. Superusers are exempt (`autoResolveRecordsFlags`
  unhides on superuser auth), so the admin panel still shows and sets them.

`listRule` can stay restrictive: expand only consults `viewRule`, so the table
still cannot be enumerated.

Coverage is partial and worth stating plainly: after the backfill, 1099 of 1747
medias have a player. The remaining 648 keep their frozen name, and the agents
who actually renamed are mostly in that group, because their records are all
from the legacy import.

## Growing coverage from uploads

An upload proves that a player ID is *currently* using a nickname, and Ingress
never releases a username - so every `medias` and `media_uploads` row carrying
that nickname belongs to that player. Linking them all on each upload, rather
than only the rows being written, pulls coverage from 63% toward complete as
agents return, using the same rule the backfill already applies.

## The name-source toggle

Per-agent, stored on `players` (not `users`) so it follows the Ingress identity:
it works before an agent has an Ingress Plus account, and an admin can set it on
request. A field such as `name_display` (`current` | `historical`, default
`current`), left visible rather than hidden.

Nothing extra is needed at read time. `expand=player` already returns
`last_ign` and the preference alongside the media's own frozen `uploader_ign`,
so a single query carries all three and the choice is a local expression:

    historical -> media.uploader_ign
    current    -> player.last_ign || media.uploader_ign

Writing it is the fiddly part, since `players` is superuser-only. Either add an
update rule scoped to the one field with the `@request.body.<field>` pattern
already used by `users.updateRule`, or expose a small authenticated route that
sets it for the caller's linked player. Only verified agents can set it at all -
an unverified `players` record has no `user`, so there is nobody to authorise.
