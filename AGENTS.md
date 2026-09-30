# Working Rules

## Spec Kit

- Follow the Spec Kit workflow for all code changes: specification → plan →
  tasks → consistency analysis → implementation.
- Use `speckit-specify`, optionally `speckit-clarify`, then `speckit-plan`,
  `speckit-tasks`, `speckit-analyze`, and `speckit-implement`.
- Before changing code, read the current `spec.md`, `plan.md`, and `tasks.md`.
  If they are missing, create them using Spec Kit first.
- During implementation, follow `tasks.md` and mark completed tasks.
- If Spec Kit is not configured or unavailable, inform the user;
  do not silently skip the workflow.

## Required Checks

After every code change, run all commands from the repository root
in the following order:

```sh
goimports -w .
go vet ./...
golangci-lint run --config .golangci-lint.yaml ./... --timeout 1m
go test -short ./...
govulncheck ./...
```

- `govulncheck` is the tool from `golang.org/x/vuln/cmd/govulncheck`.
- All checks are mandatory and must pass before committing and pushing.
- If a check fails, fix the cause and rerun the full set of checks.
- If a tool, Go module, or configuration is missing, report the blocker;
  do not claim that an unperformed check passed.

## Git

- Work directly on `main`, without feature branches or pull requests.
- After completing and validating a change, commit only the files related
  to the task and immediately push the commit to `main`.
- Use Conventional Commits for commit messages: `feat:`, `fix:`, `docs:`,
  `chore:`, or another appropriate type.

## Boundaries and Constraints

- Changing code in other repositories is prohibited. Make all code changes
  only in this repository.
- Reading files outside this repository, including other repositories, is allowed
  without additional permission.
- Obtain the user's explicit permission before writing any path outside this
  repository. The prohibition on changing code in other repositories still applies.
