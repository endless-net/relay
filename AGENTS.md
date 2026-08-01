# Agents

## Git workflow

- Work directly on `main`. Do not create feature branches or pull requests.
- After completing and validating a change, commit only its intended files and
  push the commit directly to `main` immediately.
- Format every commit message according to Conventional Commits, for example
  `feat: ...`, `fix: ...`, `docs: ...`, or `chore: ...`.

## Repository boundary

- Work only within this repository.
- Before reading from or writing to any path outside this repository, request
  and receive the user's explicit permission.

- Run `git status --short` before reading or editing.
- Never inspect secrets, private keys, `.env`, or production credentials.
- Relay wire and gRPC contracts are versioned; reject unknown versions and fields.
- Production listeners use TLS 1.3. Relay control and mesh use mTLS identities.
- Migrations must not add `DEFAULT`, explicit `NOT NULL`, or PostgreSQL foreign keys.
- Run `gofmt -w .`, `go vet ./...`, `go test -race ./...`, and `go build ./cmd/...` before handoff.
- Product CI uses GitHub-hosted runners without production access. Runner infrastructure outside GitHub belongs to its operator; keep repository workflows autonomous.
- Production SPIRE Server/Agent operations and workload-entry reconciliation belong to the operator. Keep Relay SPIFFE IDs and systemd workload requirements here, but never operate or inspect production SPIRE from Actions or deployment playbooks. Autonomous product E2E may create and destroy isolated ephemeral SPIRE servers, agents and workload entries on GitHub-hosted runners without production access.
- Release publishes immutable artifacts after product CI/E2E. Production rollout belongs to the operator; this repository must not initiate it.

## Documentation

- Write repository documentation, comments, diagnostics and workflow labels in English.
- Describe Relay as a standalone public product with a compatible upstream and operator-owned infrastructure. Keep integration-specific deployment history outside this repository.
- Preserve published technical identifiers, protocol fields, versions and required license notices when editing prose.
