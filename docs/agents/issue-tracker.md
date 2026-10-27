# Issue tracker: GitHub

Issues and specs for this repo live as GitHub issues. Use the `gh` CLI for operations. The repo is inferred from `git remote -v`.

## Conventions

- Create: `gh issue create --title "..." --body "..."`.
- Read: `gh issue view <number> --json number,title,body,labels,comments`.
- List: `gh issue list --state open --json number,title,body,labels,comments`, with appropriate filters.
- Comment: `gh issue comment <number> --body "..."`
- Apply or remove labels: `gh issue edit <number> --add-label "..."` / `--remove-label "..."`
- Close: `gh issue close <number> --comment "..."`
- Link a child issue with `gh issue create --parent <parent>` or `gh issue edit <parent> --add-sub-issue <child>`. If sub-issues aren’t available, add `Part of #<parent>` to the child issue.

## Pull requests as a triage surface

**PRs as a request surface: no.**

When a skill says to publish to the issue tracker, create a GitHub issue. When it says to fetch a ticket, read it with `gh issue view`.

## Wayfinding

- The map is one issue labelled `wayfinder:map`.
- Child issues link to the map as sub-issues. If that isn’t available, use a task list in the map and put `Part of #<map>` in each child issue. Use `wayfinder:<type>` labels (`research`, `prototype`, `grilling`, or `task`).
- Represent blockers with GitHub issue dependencies when available; otherwise, add `Blocked by: #<n>` to the child issue. A child is unblocked when all blockers are closed.
- Claim a ticket with `gh issue edit <n> --add-assignee @me`.
- Resolve it by commenting with the answer, closing the issue, and adding a context pointer to the map’s Decisions-so-far.
