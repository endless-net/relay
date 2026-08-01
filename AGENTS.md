# Agents

## Git workflow

- Work on a short-lived branch and submit every change through a pull request
  targeting `main`. Do not push changes directly to `main`.
- Format every commit message according to Conventional Commits, for example
  `feat: ...`, `fix: ...`, `docs: ...`, or `chore: ...`.

- Run `git status --short` before reading or editing.
- Never inspect secrets, private keys, `.env`, or production credentials.
- Relay wire and gRPC contracts are versioned; reject unknown versions and fields.
- Production listeners use TLS 1.3. Relay control and mesh use mTLS identities.
- Migrations must not add `DEFAULT`, explicit `NOT NULL`, or PostgreSQL foreign keys.
- Run `gofmt -w .`, `go vet ./...`, `go test -race ./...`, and `go build ./cmd/...` before handoff.
- GitHub Actions runner-unit infrastructure (installation, registration, systemd policy, inventory, recovery, and rollout) is owned by `endless-net/observability`; keep only the minimal `runs-on` selectors in this repository.
- SPIRE Server/Agent operations and workload-entry reconciliation are owned by `endless-net/observability` through guarded direct SSH. Keep Relay SPIFFE IDs and systemd workload requirements here, but never operate or inspect SPIRE from Actions or deployment playbooks.
- Production changes use the guarded release/deploy workflows.
