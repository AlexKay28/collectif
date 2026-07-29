# GitHub mirror API contract — slice A (read-only)

All endpoints live under `/api/gh/` and are gated by the existing shared-secret
`Authorization: Bearer <token>` (or `?token=...`) middleware. Sibling slices B
(issues frontend), C (PR viewer), and D (offline write queue) build against
this document — they should not need to read the Go source.

Cache root on disk: `<repo-root>/.collectif/cache/gh/`. All handlers read from
this cache; the network is never on a request's critical path.

**Reserved for slice D**: write endpoints will live under `/api/gh/pending`
(compose comment, close/reopen, label, assign). Not implemented in slice A.

---

## GET `/api/gh/status`

Returns cache-wide metadata plus liveness of the syncer.

```json
{
  "repo": { "owner": "AlexKay28", "name": "collectif" },
  "defaultBranch": "main",
  "lastSyncAt": "2026-07-29T09:14:22.113Z",
  "syncing": false,
  "pendingCount": 0
}
```

Field notes:

- `repo` is derived from `git remote get-url origin` at first request; it is
  present even when no sync has run yet.
- `defaultBranch` and `lastSyncAt` are populated **only after the first
  successful sync**. Before that, `defaultBranch` is `""` and `lastSyncAt` is
  the JSON zero time (`"0001-01-01T00:00:00Z"`).
- `syncing` flips to `true` while a background sync runs.
- `pendingCount` is always `0` in slice A — slice D wires it to the queue.

Errors:

- `500` if the working directory is not a git repo or the `origin` remote
  isn't a GitHub URL. Body is plain text: `remote "..." is not a GitHub SSH or HTTPS URL`.

---

## POST `/api/gh/sync`

Kicks off a background sync. Sync workflow (per invocation):

1. `gh api repos/OWNER/REPO/issues?state=all&per_page=100&page=N` paginated
   until an empty page. Items with a `pull_request` field are skipped for
   `issues/*` (they land in the PR mirror instead).
2. For each issue, `gh api repos/OWNER/REPO/issues/N/comments` (paginated),
   inlined into the cached JSON under `comments_data`.
3. `gh api repos/OWNER/REPO/pulls?state=all&per_page=100` for the richer PR
   shape; per-PR `pulls/N/reviews` and `pulls/N/comments` inlined under
   `reviews_data` and `review_comments_data`.
4. Rebuild `issues/index.json` and `prs/index.json` (atomic write via
   `.tmp` + rename).
5. Refresh `repo.json` (owner, name, defaultBranch, lastSyncAt).

Query params:

- `wait=1` — block until the sync completes, then return. Intended for tests
  and CLI scripts; the UI should not use this.

Immediate response:

```json
{ "started": true }
```

If another sync is already in flight (coalesced):

```json
{ "started": false, "reason": "already syncing" }
```

Errors:

- `500` on any `gh` failure (non-zero exit or malformed JSON). Body contains
  the underlying stderr where available.

Timeout: each `gh` invocation is bounded at **60 s**.

---

## GET `/api/gh/issues`

Returns the cached issue index, filtered in-memory.

Query params (all optional, AND-combined):

- `state` — `open` | `closed` | `all` (default `all`).
- `label` — case-insensitive exact-name match against `labels[]`.
- `assignee` — case-insensitive exact-login match against `assignees[]`.
- `q` — case-insensitive substring match against the issue title.

Response:

```json
{
  "total": 11,
  "issues": [
    {
      "number": 44,
      "title": "Local GitHub-style issue & PR tracker",
      "state": "open",
      "labels": ["enhancement", "priority: medium"],
      "assignees": [],
      "author": "AlexKay28",
      "commentCount": 0,
      "createdAt": "2026-07-19T10:14:22Z",
      "updatedAt": "2026-07-19T10:14:22Z",
      "htmlUrl": "https://github.com/AlexKay28/collectif/issues/44"
    }
  ]
}
```

Empty state: `{ "total": 0, "issues": [] }` (never `null`).

---

## GET `/api/gh/issues/{n}`

Returns the full cached issue JSON — the raw shape from the GitHub REST API
extended with:

- `comments_data`: array of GitHub comment objects (id, user, body,
  created_at, updated_at, reactions, ...).

```json
{
  "number": 43,
  "title": "Right side panel: tabbed context view",
  "state": "open",
  "body": "## Context\n\nThe right side panel today ...",
  "labels": [
    { "name": "enhancement", "color": "a2eeef" },
    { "name": "priority: medium", "color": "fef2c0" }
  ],
  "assignees": [],
  "user": { "login": "AlexKay28", "avatar_url": "https://..." },
  "comments": 0,
  "created_at": "2026-07-17T12:59:11Z",
  "updated_at": "2026-07-17T12:59:11Z",
  "html_url": "https://github.com/AlexKay28/collectif/issues/43",
  "reactions": { "total_count": 0, "+1": 0 },
  "comments_data": [
    {
      "id": 1234567,
      "user": { "login": "AlexKay28" },
      "body": "lgtm",
      "created_at": "2026-07-18T09:00:00Z",
      "updated_at": "2026-07-18T09:00:00Z"
    }
  ]
}
```

Errors:

- `404 not cached` — the issue has not been synced yet. Trigger a sync and retry.
- `400 invalid issue number` — the path segment did not parse as a positive int.

---

## GET `/api/gh/prs`

Same shape as `/api/gh/issues` but keyed by `prs`. Each entry adds
`headRef` / `baseRef` / `headSha` / `baseSha` / `merged` / `draft`.

Query params: same as `/api/gh/issues` (`state`, `label`, `assignee`, `q`).

```json
{
  "total": 1,
  "prs": [
    {
      "number": 57,
      "title": "Add /healthz + CI workflow",
      "state": "open",
      "merged": false,
      "draft": false,
      "headRef": "feature/healthz",
      "baseRef": "main",
      "headSha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "baseSha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "labels": ["ci"],
      "assignees": [],
      "author": "AlexKay28",
      "commentCount": 3,
      "createdAt": "2026-07-20T08:00:00Z",
      "updatedAt": "2026-07-20T09:30:00Z",
      "htmlUrl": "https://github.com/AlexKay28/collectif/pull/57"
    }
  ]
}
```

---

## GET `/api/gh/prs/{n}`

Full cached PR JSON — the raw `/pulls/{n}` shape extended with:

- `comments_data`: PR conversation (uses the issue-comment endpoint).
- `reviews_data`: pull-request reviews (`pulls/{n}/reviews`).
- `review_comments_data`: inline review-thread comments (`pulls/{n}/comments`).

The fields `head`, `base`, `mergeable`, `mergeable_state`, `draft`,
`merged_at`, `commits`, `additions`, `deletions`, `changed_files` are all
present as GitHub returns them.

Errors: `404 not cached`, `400 invalid pr number`.

---

## GET `/api/gh/prs/{n}/diff`

Returns a unified diff as `text/plain; charset=utf-8`.

Resolution order:

1. If `<cache>/pr-diffs/{n}.diff` exists AND its mtime is newer than the
   cached PR's `updated_at`, stream it.
2. Otherwise read `head.sha` / `base.sha` from the cached PR and run
   `git diff base..head` inside `<repo-root>`.
3. If the local git command fails (missing ref), try
   `git fetch origin pull/{n}/head:refs/collectif/pr-{n}` and re-run the
   diff once.
4. Cache the successful diff to `<cache>/pr-diffs/{n}.diff`.

Errors:

- `404 pr not cached` — the PR JSON isn't on disk. Sync first.
- `500 pr missing head/base sha` — the cached PR JSON is corrupt/incomplete.
- `503` — both local git and the fetch fallback failed; body is a plain-text
  error describing what was attempted. Typical remediation: reconnect and
  retry, or wait for the sync to refresh the PR.

---

## Cache layout on disk

```
<repo-root>/.collectif/cache/gh/
  repo.json                # {owner, repo, defaultBranch, lastSyncAt}
  issues/
    index.json             # [ghIssueIndexEntry, ...] — see /issues response
    43.json                # full issue + inline comments_data
    ...
  prs/
    index.json             # [ghPRIndexEntry, ...]
    57.json                # full PR + comments_data + reviews_data + review_comments_data
    ...
  pr-diffs/
    57.diff                # populated on demand by /prs/{n}/diff
```

All JSON files use `application/json`. All writes go through a `.tmp` + rename
so a reader never sees a partial file.

---

## Error response shape

Every non-2xx response is `text/plain` with a human-readable body (matching
the existing `http.Error` convention in `src/api.go`). The status code is the
only machine-readable signal:

| Code | Meaning                                                         |
| ---- | --------------------------------------------------------------- |
| 400  | Bad path segment (non-numeric issue/PR number).                 |
| 401  | Missing or invalid `Authorization: Bearer` token.               |
| 404  | The requested entity is not in the cache. Run a sync and retry. |
| 405  | Method not allowed.                                             |
| 500  | Repo not resolvable, `gh` failure, or cache I/O failure.        |
| 503  | Diff endpoint could neither read local git nor fetch the ref.   |

---

## Fields populated only after first sync

| Field                                       | Endpoint          | Populated when                 |
| ------------------------------------------- | ----------------- | ------------------------------ |
| `defaultBranch`                             | `/status`         | after first successful sync    |
| `lastSyncAt`                                | `/status`         | after first successful sync    |
| `issues[]` / `prs[]`                        | `/issues`, `/prs` | after first successful sync    |
| full issue/PR JSON                          | `/issues/{n}`, `/prs/{n}` | after first successful sync |
| diff text                                   | `/prs/{n}/diff`   | on first request after sync (cached thereafter) |

Before the first sync, `/status` returns the resolved repo identity but empty
sync metadata; list endpoints return `{ "total": 0, "issues": [] }`; per-item
endpoints return `404 not cached`.
