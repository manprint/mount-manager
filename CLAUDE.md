# mount-manager — onboarding

Go 1.27 daemon supervising the `compose.yml` stack (rclone FUSE mount + superdufs): wait → monitor →
recovery (detach mount, compose down/up) → verify → backoff. Full behavior and invariants (I-1..I-9,
D6..D17): `README.md`. Spec: `Specifications.txt`. Plan/audit history: `docs/plans/001_plan-MountManager/`.

## Layout
- `cmd/mount-manager` — CLI (`run`, `check`, `probe`, `version`); exit 3 = config/usage error.
- `internal/supervisor` — single-goroutine state machine (`Step`); no locking.
- `internal/health` — check suite C1-C5; never touches the mount FS (I-3), probe runs out-of-process.
- `internal/recovery` — CleanupMount (fusectl abort + MNT_DETACH, never foreign mounts, I-2) + down/up.
- `internal/compose` — `docker compose` wrapper; `ExecRunner` uses a process group per command.
- `internal/mountinfo`, `internal/probe`, `internal/unmount`, `internal/notify` (webhook), `internal/config`.

## Gates (same as CI)
```
gofmt -l . ; go vet ./... ; go vet -tags e2e ./test/e2e/
staticcheck ./... ; staticcheck -tags e2e ./test/e2e/   # v0.8.2
govulncheck ./...                                        # v1.9.0
go test -count=1 -race ./...
```

## E2E (docker + FUSE; root for E3-E6)
- Rebuild first, `e2e-root.sh` does not: `go build -o bin/mount-manager ./cmd/mount-manager && go test -c -tags e2e -o bin/e2e.test ./test/e2e`
- Preconditions: no `rclone-mount`/`superdufs` containers, port 5000 free.
- The work dir must be on a *shared* mount (bind source uses `rshared`). In a sandbox where everything is
  `private`: `W=test/e2e/.work; mkdir -p $W && sudo mount --bind $W $W && sudo mount --make-rshared $W`
  (undo: `sudo umount $W`).
- Non-root: `MM_BIN=$PWD/bin/mount-manager go test -count=1 -tags e2e -v -timeout 20m -run '^TestE2E_(WaitThenMonitor|ManualDownNoRecovery)$' ./test/e2e/`
- Root: `sudo -n $PWD/e2e-root.sh -test.run '^TestE2E_'`
- Shared docker daemon: clean up only the e2e project, never `docker system/volume prune`.

## Conventions
- Behavior changes: update `README.md` (Italian) in the same change; tests first.
- Durations validated > 0; compose timeouts >= 1s; errors never log webhook/check URLs.
