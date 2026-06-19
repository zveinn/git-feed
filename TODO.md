# TODO

This document tracks planned fixes, improvements, and optimizations for **GitHub Feed**.

The tool's goal is to discover **all PRs and Issues** a user is involved in (via author, assignee, mentions, commenter, reviewed-by, and review-requested roles), while being efficient with the GitHub Search API and providing useful offline caching and display.

> **Out of scope**: GitHub Discussions (tool focuses on PRs and Issues only).

---

## API Efficiency

- [ ] Reduce the number of Search API calls
  - Currently performs 10 parallel queries (6 for PRs + 4 for issues).
  - GitHub's `involves:USERNAME` qualifier covers `author | assignee | mentions | commenter`.
  - `reviewed-by:` and `review-requested:` are **not** included in `involves`.
  - Target: ~3 queries total (`involves:...` + reviewed-by + review-requested for PRs).
  - Consider a hybrid approach (run a few high-priority role queries + broad `involves`) to preserve specific labels.

- [ ] Evaluate a single broad `involves:USER updated:>=DATE` query (mixed PRs + issues) with post-filtering
  - Fewer initial queries and better pagination behavior for the union of results.

---

## Completeness

- [ ] Support team-based review requests
  - `review-requested:USER` only catches direct requests.
  - Team requests use `team-review-requested:ORG/TEAM`.
  - Users who are members of requested teams may miss PRs unless they have other involvement signals.

- [ ] Document (or handle) "merged by" / "closed by" involvement
  - No reliable search qualifier surfaces these actions.
  - Such items only appear today if the user is also an author, assignee, commenter, reviewer, or mentioned.

---

## Correctness

- [ ] Paginate `PullRequests.ListComments` during cross-reference checks
  - Current code uses `PerPage: 100` with no pagination loop.
  - Mentions after the first 100 review comments are missed.

- [ ] Make cross-reference detection symmetric and more robust
  - Currently only scans PR review comments for references to issues.
  - Does not scan regular issue comments for references to PRs.
  - Body checks are done both ways, but comment fallback is one-sided.

---

## Code Quality

- [ ] Remove dead code from removed event collection feature
  - `initialTotal += 3 // Add 3 for event pages` (and related logic/comments).
  - This still inflates progress totals in online mode.

- [ ] Clean up unused label colors
  - `"Involved"` and `"Recent Activity"` are defined in `getLabelColor()` but never assigned by current queries.

- [ ] Refactor duplicated logic between collectors
  - `collectSearchResults` (PRs) and `collectIssueSearchResults` contain nearly identical code for:
    - Pagination
    - Deduplication via `sync.Map`
    - Label priority updates
    - DB caching + `HasUpdates` detection
  - Extract shared helpers.

- [ ] Consider populating additional fields on PR objects constructed from search results
  - Only a minimal subset is currently copied (`Number`, `Title`, `Body`, `State`, `UpdatedAt`, `User`, `HTMLURL`).
  - Other useful fields (e.g. `Draft`, `Head`, `Base`, `Merged`, `RequestedReviewers`) are absent.

---

## UX / Observability

- [ ] Fix progress reporting in `--local` mode
  - Progress total is initialized to 10 (or 13), but local-mode collectors never call `increment()` or `addToTotal()`.
  - Progress bar is not meaningful (or is misleading) when using offline mode.

---

## Documentation

- [ ] Update README.md "How It Works" section to reflect reality
  - Remove references to removed "recent activity events".
  - Clarify the actual search qualifiers used.
  - Update description of "PRs involving you" and involvement coverage.
  - Ensure "How It Works" matches the queries in `fetchAndDisplayActivity`.

---

## Scope / Involvement Model

- [ ] Decide on and document treatment of reactions
  - Emoji reactions (👍, etc.) do not create records that match current search qualifiers.
  - Not currently treated as involvement.
