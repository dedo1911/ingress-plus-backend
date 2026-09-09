# Agent verification: schema and rollout

Collection schemas live in the PocketBase instance, not in this repo, so
everything in §1 and §2 has to be done by hand in the admin UI before the routes
can run.

Verification establishes that an Ingress Plus account's `username` and `faction`
really are a given Ingress agent's. It is a **general site feature** — what it
produces is a value in `users.verification`, and who consumes that is not the
verification code's concern. Mediagress attribution (`players.user`) is the
first consumer, not the purpose. See the package comment on
[internal/verification](../internal/verification/tier.go).

## The three tiers

| Tier | Proof | Attests | Mediagress link | C.O.R.E. |
| --- | --- | --- | --- | --- |
| `basic` | The profile matches what IITC reports. Instant. | nothing | no | no |
| `advanced` | The agent posts a code to COMM; an admin reads it back. | nickname **and** faction, by Niantic | yes, by `last_ign` | no |
| `strong` | COMM **plus** the player ID from the agent's own inventory. | nickname, faction, and the hash | yes, by hash | yes |

A COMM plext carries the sender's team as well as their name, which is why
advanced attests the faction too.

`basic` is honest about proving nothing: it compares values from a page the
claimant controls against values they typed into their own profile.

## The flow

Identical for every tier up to step 4.

    1. /verify            the agent picks a tier and gets a code    IPV-K7M2QP4RXB
    2. plugin             the agent pastes it in
    3. plugin -> backend  POST /api/verification/claim
                          { code, nickname, faction, playerId? }
    4. backend            does the site profile match what IITC reports?
                          no  -> 400 naming both values; nothing happens
                          yes -> basic           : applied now
                                 advanced/strong : { status: "post_to_comm", ... }
    5. plugin             posts commMessage at commLatE6/commLngE6 (Point Nemo)
    6. admin              reads it and POSTs confirm with the COMM-attested pair
    7. backend            applies the tier using *that* pair

**Step 4 is a gate, not the proof.** It exists so an agent with a mismatched
profile finds out immediately instead of burning a COMM post and waiting on an
admin. The values it checks are self-asserted. The proof is step 6, and step 7
applies the COMM-attested nickname and faction, not the plugin-reported ones.
Do not "optimise" the confirm step away.

The backend supplies the COMM message and coordinates in the step-4 response
rather than the plugin hardcoding them, so moving the location is a backend
change rather than a plugin release chasing auto-update across a userbase that
opens the plugin about once a month.

## 1. New collection: `agent_verifications`

One record per attempt. Expired and rejected rows are the audit trail — there is
deliberately no cleanup cron.

| Field | Type | Req | Notes |
| --- | --- | --- | --- |
| `code` | text | yes | **unique index**. Plaintext: short-lived, single-use, and it is the string posted to COMM |
| `user` | relation → `users`, max 1 | yes | the only thing tying the unauthenticated claim route to an identity |
| `tier` | select `basic\|advanced\|strong` | yes | set at mint time, never taken from the claim body |
| `status` | select `pending\|claimed\|confirmed\|applied\|rejected` | yes | target of the atomic single-use UPDATE |
| `contested` | bool | no | the agent asserted the name is theirs although another account holds it |
| `claimed_username` | text | no | set at mint when contested: their profile cannot hold the name yet |
| `expires_at` | date | yes | 30 minutes to redeem the code, then **extended to 7 days** when it is claimed for COMM |
| `nickname`, `faction` | text | no | what was attested — written at claim, **overwritten from COMM at confirm** |
| `player_hash` | text, **hidden** | no | the strong tier's proof artifact. Not unique: an agent may re-verify |
| `claimed_at`, `confirmed_at` | date | no | audit |
| `confirmed_by` | relation → `_superusers`, max 1 | no | which admin read the COMM |
| `note` | text | no | rejection reason / trump record |

API rules:

- List / View: `user = @request.auth.id`.
- Create / Update / Delete: **empty (superusers only)** — same posture as
  `players`. The Go code writes through `app.Save`, which bypasses rules.

Indexes: unique on `code`; plain on `(user, status)`.

### Two clocks

A code lives 30 minutes, which is how long an agent needs to paste it into
the plugin. Claiming it for advanced or strong resets `expires_at` to a week
out: once the plext is in COMM the clock belongs to the admins, and nobody
watches Point Nemo continuously. It stays a deadline rather than becoming no
expiry at all so a verification nobody confirms lapses instead of staying
claimable forever - the agent can always mint a new code and post again.

## 2. The `players.user` unique index

Nothing currently stops two accounts owning one Ingress identity:

    CREATE UNIQUE INDEX idx_players_user ON players (user) WHERE user != ''

Partial for the reason [player-identity.md](player-identity.md) already gives for
`idx_media_uploads_player`: PocketBase stores an unset relation as `''`, not
NULL.

This index is load-bearing rather than tidy — it is what makes the ordering in
`players.link` (release the old row *before* saving the new one) fail loudly
instead of silently duplicating an identity.

## 3. Environment and the feature flag

`TELEGRAM_TOPIC_VERIFICATION` — the forum topic for verification notifications.
These go to the **admin group only** and are the queue of verifications waiting
to be confirmed; nothing about them reaches the agent, who is emailed instead.
Optional; unset posts to the group's General thread, and with no bot token every
send is a no-op like the rest of `internal/notify`.

`VERIFICATION_ENABLED` in `feature_flags` already exists and already gates the
website. It now also gates `mint` and `claim` on the backend, and **fails
closed** — a flag that cannot be read is treated as disabled. `confirm` is
deliberately not gated: an admin has to be able to finish a verification that is
already sitting in COMM after the flag goes off.

## 4. Routes

| Route | Auth | Body |
| --- | --- | --- |
| `POST /api/verification/mint` | `RequireAuth("users")` | `{tier, contested, claimedUsername}` → `{code, tier, expiresAt}` |
| `POST /api/verification/claim` | **none** | `{code, nickname, faction, playerId?}` |
| `POST /api/admin/verification/{code}/confirm` | `RequireSuperuserAuth()` | `{nickname, faction}` |

Unknown, expired and already-used codes all answer 404 with one message, so the
claim route is not an oracle for guessing codes.

## 5. The identity-match rule

**No tier is applied unless the site `username` and `faction` already match what
IITC or COMM reports.** The backend never quietly rewrites a profile to make a
verification succeed. On a mismatch nothing is written, the code stays `pending`
so it can be retried, and the agent is told which two values disagree — in the
response body at claim time, and by email if the COMM pair turns out to differ
from what the plugin reported.

Comparisons are case-insensitive both ways: `users.faction` is lowercase while
Ingress reports `RESISTANCE`. An account whose site faction is `machina` can
never match a COMM plext and stays a manual admin grant. That is a consequence
of the game, not a case to special-case.

### The one exception: a contested name

If another account already holds the username, the real agent *cannot* set it —
usernames are unique — so the rule above would lock them out permanently. The
escape hatch is theirs to take on `/verify`: when changing their name returns
"already used", they tick a box confirming it is genuinely their agent name.
That stores `contested` and the `claimed_username` they typed.

`contested` is accepted only with **advanced or strong**, and it is the only
path on which the backend writes a username. The displaced account is renamed to
its own record id (15 lowercase alphanumerics, already shown to them as
`ING+<id>`), has its verification cleared, and is emailed. A holder verified at
an equal or higher tier is never displaced — two agents can both be wrong, so a
human decides.

## 6. Rollout order

1. Create `agent_verifications` and add the `players.user` index (§1, §2).
2. Deploy. With `VERIFICATION_ENABLED` off, mint and claim answer 403.
3. Turn the flag on for a test account and walk one verification of each tier
   through mint → claim → confirm, checking `users.verification`,
   `users.username` and `players.user` after each.
4. Then the plugin work: the general Ingress Plus plugin that pastes the code,
   calls claim, and posts the returned message at the returned coordinates.

## Open: the 42 hand-granted accounts

40 `advanced` and 2 `strong` accounts were verified by hand over Telegram before
any of this existed, and have no `players.user`. They can either be linked in
one pass through `LinkVerifiedUser` as an `internal/backfill` subcommand, or
left to link on their next upload — which, at roughly one upload a month across
the whole site, means effectively never.
