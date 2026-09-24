# Phase 2 — Recovery, supervisore, comando `run`, e2e non-root

Intent: `mount-manager run` esegue lo state machine completo (attesa, monitoraggio, recovery con backoff,
webhook); harness e2e reale e scenari non-root E1 (attesa→monitoraggio) ed E2 (down manuale senza recovery) verdi.
Prerequisites: P1 DONE (`compose`, `notify`, `health`, `check`).
Phase closure: P2; review by agent-1:opus.

## State and ownership contract
Read STATE.md §0–1 first for scope, ownership, recovery, checks, and commit rules.
Open before edits; checkpoint at a recovery boundary; close with evidence. A
delegated worker follows its assigned OPEN unit. Missing design goes to agent-1:opus.

## Local design context
Plan revision: 1.
- D4/I-1: idle (nessun container running/restarting/paused) in Waiting o Monitoring → mai recovery.
- D5: Monitoring: `failure_threshold` fallimenti consecutivi → recovery. Waiting: stack non idle e non OK per
  ≥ `startup_grace` continui → recovery.
- D6/I-8: sequenza recovery (sotto, 2.1). abortFirst = `report.HasMountFailure()` quando la recovery nasce da un report;
  `true` per i tentativi da Backoff.
- D8: `Backoff(n) = min(initial·2^(n-1), max)`, n<1 → initial, senza overflow.
- D10/I-2: abort/detach solo se top mount al path ha `expected_fstype`.
- D12: eventi `notify.Event*`; Host = `os.Hostname()` (errore → "unknown"), MountPath = cfg.MountPath, Timestamp = clock.Now().
- D14: Waiting + idle + MountStale → `CleanupMount(ctx, true)`; successo → evento `stale_mount_cleaned`; errore → solo log error.
- I-6: e2e con solo remote `alias` locale generato nel work dir; mai `./config` del repo.
- I-7: `Run` non si blocca oltre i timeout dei comandi; cancellazione ctx termina il loop.
- R8: `docker stop` su container in pausa funziona (fallback kill resta comunque).
- R10: dopo SIGKILL di rclone il mount host resta stale (ENOTCONN) anche dopo `docker rm`; `fusermount3 -u -z` come uid 1000 lo rimuove.

### State machine (vincolante per 2.2)
Stati: `Waiting`, `Monitoring`, `Verifying`, `Backoff` (tipo `State int` con `String()` → "waiting", "monitoring", "verifying", "backoff").
Campi: `state`, `failCount int`, `attempt int`, `graceStart time.Time` (zero = non attivo), `verifyDeadline`, `backoffUntil time.Time`.
`Start()`: emette `startup` (detail "version <v>") poi `waiting`; state = Waiting.
`Step(ctx) time.Duration` (now := clock.Now() all'inizio):
```
case Backoff:
    if now.Before(backoffUntil) { return backoffUntil.Sub(now) }       // nessuna Evaluate
    return runRecovery(ctx, true, "retry after backoff")
r := health.Evaluate(ctx)
case Waiting:
    if r.Idle {
        graceStart = zero
        if r.MountStale { if err := rec.CleanupMount(ctx, true); err == nil { emit(stale_mount_cleaned, "stale mount detached", 0) } else { log.Error } }
        return interval
    }
    if r.OK { state = Monitoring; failCount = 0; graceStart = zero; emit(stack_up, "all checks passed", 0); return interval }
    if graceStart.IsZero() { graceStart = now }
    if now.Sub(graceStart) >= startupGrace { graceStart = zero; emit(failure_detected, r.Summary(), 0); return runRecovery(ctx, r.HasMountFailure(), r.Summary()) }
    return interval
case Monitoring:
    if r.OK { failCount = 0; return interval }
    if r.Idle { state = Waiting; failCount = 0; emit(stack_stopped, "no running containers", 0); return interval }
    failCount++ ; log.Warn("check failed", "count", failCount, "summary", r.Summary())
    if failCount >= threshold { failCount = 0; emit(failure_detected, r.Summary(), 0); return runRecovery(ctx, r.HasMountFailure(), r.Summary()) }
    return interval
case Verifying:
    if r.OK { emit(recovery_succeeded, "all checks passed", attempt); attempt = 0; failCount = 0; state = Monitoring; return interval }
    if !now.Before(verifyDeadline) { emit(recovery_failed, "verify timeout: "+r.Summary(), attempt); return enterBackoff() }
    return interval                                                     // idle ignorato in Verifying
runRecovery(ctx, abortFirst, reason):
    attempt++ ; emit(recovery_started, reason, attempt)
    err := rec.Recover(ctx, attempt, abortFirst)
    if err != nil { emit(recovery_failed, err.Error(), attempt); return enterBackoff() }
    state = Verifying; verifyDeadline = clock.Now().Add(verifyTimeout); return interval
enterBackoff():
    d := Backoff(attempt, initial, max); state = Backoff; backoffUntil = clock.Now().Add(d); return d
```
`Run(ctx) error`: `Start()`; loop `{ d := Step(ctx); if ctx.Err() != nil { break }; t := time.NewTimer(d); select { case <-ctx.Done(): t.Stop(); break loop; case <-t.C: } }`;
emette `shutdown` ("signal received"); ritorna nil. Ogni `emit` logga anche a livello Info (Warn per failure_detected/recovery_failed).
Solo un goroutine chiama Step: nessun mutex nel supervisore.

## Sub-phases

### 2.1 Package recovery
- **Model:** agent-2:sonnet
- **Assignment:** agent-1:opus conferma contratto prima di iniziare (breve) e rivede il diff (I-2, I-8); poi P2.
- **Files:** READ `internal/mountinfo`, `internal/unmount`, `internal/compose`, `internal/config`; NEW `internal/recovery/recovery.go`, `internal/recovery/recovery_test.go`.
- **Change:**
  Preconditions: P1 DONE.
  Contract:
  ```go
  type ComposeOps interface {
      Down(ctx context.Context, stop time.Duration) error
      Kill(ctx context.Context) error
      Up(ctx context.Context, wait time.Duration) error
  }
  type Deps struct {
      Mounts  func() ([]mountinfo.Entry, error)
      Abort   func(minor int) error
      Detach  func(path string) error
      Compose ComposeOps
      Log     *slog.Logger
  }
  type Recoverer struct{ /* cfg *config.Config; d Deps */ }
  func New(cfg *config.Config, d Deps) *Recoverer
  func (r *Recoverer) CleanupMount(ctx context.Context, abort bool) error
  func (r *Recoverer) Recover(ctx context.Context, attempt int, abortFirst bool) error
  const maxDetach = 5
  ```
  `CleanupMount`: ripeti fino a `maxDetach` volte: `entries, err := Mounts()` (err → return `fmt.Errorf("read mountinfo: %w", err)`);
  `top, ok := Top(entries, MountPath)`; `!ok` → return nil; `top.FSType != ExpectedFSType` → return
  `fmt.Errorf("refusing to unmount %s: fstype %s is not %s", …)` [I-2]; se `abort` → `Abort(top.Minor)` (errore → Log.Warn, continua);
  `Detach(MountPath)` errore → return err. Dopo il ciclo: un'ultima lettura; ancora presente → `fmt.Errorf("mount %s still present after %d detach attempts", …)`.
  `Recover` (log Info per ogni passo con `attempt`):
  1. `CleanupMount(ctx, abortFirst)` → err: `fmt.Errorf("pre-down cleanup: %w", err)`.
  2. `Down(ctx, DownTimeout)`; se err → Log.Warn; `Kill(ctx)` (err → Log.Warn); `Down(ctx, 0)`; se err → `fmt.Errorf("compose down failed: %w", err)` (Up non chiamato).
  3. `CleanupMount(ctx, true)` → err: `fmt.Errorf("post-down cleanup: %w", err)`.
  4. `Up(ctx, UpWaitTimeout)` → err: `fmt.Errorf("compose up failed: %w", err)`.
  5. return nil. Se `ctx.Err() != nil` prima di ogni passo → return `ctx.Err()`.
  Steps: S1 — implementazione; S2 — test con fake che registra sequenza di stringhe (`"mounts"`, `"abort 58"`, `"detach"`, `"down 30s"`, `"kill"`, `"down 0s"`, `"up 120s"`); expected G-U2.1 verde.
  Recovery boundary: none.
  Failure handling: ambiguità sull'ordine → escalation agent-1:opus.
- **Unit tests:** `recovery_test.go` (fake Mounts con coda di risultati: ogni chiamata consuma lo stato successivo, ultimo stato ripetuto):
  `TestRecoverSequenceHealthyMount` (mount fuse presente poi assente; abortFirst=false → nessun "abort" prima del primo "down"; ordine detach < down < up;
  post-down nessun detach perché assente) [I-8]; `TestRecoverAbortFirst` ("abort 58" precede il primo "detach");
  `TestRecoverPostDownStaleCleanup` (mount riappare stale dopo down → "abort 58","detach" tra "down" e "up");
  `TestRecoverDownFailsFallsBackToKill` (primo Down errore → sequenza "down 30s","kill","down 0s","up 120s");
  `TestRecoverDownAndFallbackFail` (errore contiene "compose down failed"; nessun "up");
  `TestRecoverUpFails` (errore contiene "compose up failed"); `TestCleanupRefusesForeignFSType` (ext4 → errore "refusing", nessun abort/detach) [I-2];
  `TestCleanupStackedMounts` (2 fuse impilati → 2 detach, nil); `TestCleanupGivesUpAfterMax` (sempre presente → 5 detach, errore "still present");
  `TestCleanupNoMount` (nessun detach); `TestRecoverContextCanceled` (ctx cancellato → ctx.Err(), nessun down). Gate G-U2.1.
- **e2e tests:** N/A qui — E3–E6 (3.2).
- **Done:** 11 test PASS; review diff agent-1:opus registrata; unit chiusa; commit.

### 2.2 Package supervisor (state machine)
- **Model:** agent-2:sonnet
- **Assignment:** agent-1:opus rivede il design (questa sezione) prima di aprire la unit e il diff dopo (I-1, I-7, I-8, transizioni); poi P2.
- **Files:** READ `internal/health`, `internal/recovery`, `internal/notify`, `internal/config`; NEW `internal/supervisor/supervisor.go`, `internal/supervisor/supervisor_test.go`.
- **Change:**
  Preconditions: 2.1 DONE; 1.2 DONE.
  Contract:
  ```go
  type Health interface{ Evaluate(ctx context.Context) health.Report }
  type Recovery interface {
      Recover(ctx context.Context, attempt int, abortFirst bool) error
      CleanupMount(ctx context.Context, abort bool) error
  }
  type Clock interface{ Now() time.Time }
  type RealClock struct{}; func (RealClock) Now() time.Time { return time.Now() }
  type Options struct {
      Config   *config.Config
      Health   Health
      Recovery Recovery
      Notifier notify.Notifier
      Clock    Clock
      Log      *slog.Logger
      Host     string
      Version  string
  }
  func New(o Options) *Supervisor
  func (s *Supervisor) Start()
  func (s *Supervisor) Step(ctx context.Context) time.Duration
  func (s *Supervisor) Run(ctx context.Context) error
  func (s *Supervisor) State() State
  func Backoff(n int, initial, max time.Duration) time.Duration
  ```
  Semantica: esattamente lo pseudocodice di §State machine (interval = CheckInterval, threshold = FailureThreshold,
  startupGrace = StartupGrace, verifyTimeout = Recovery.VerifyTimeout, initial/max = Recovery.BackoffInitial/Max).
  `Backoff`: `if n < 1 { n = 1 }; d := initial; for i := 1; i < n; i++ { if d >= max/2 { return max }; d *= 2 }; if d > max { return max }; return d`.
  Steps: S1 — tipi, Backoff, Start/emit; S2 — Step secondo pseudocodice; S3 — Run; S4 — test; expected G-U2.2 verde con `-race`.
  Recovery boundary: dopo S2 se la sessione si interrompe (checkpoint §6 con stato test).
  Failure handling: qualunque deviazione necessaria dallo pseudocodice → escalation agent-1:opus (non inventare transizioni).
- **Unit tests:** `supervisor_test.go`. Fake: `fakeHealth` (coda di Report; ultimo ripetuto; conta chiamate), `fakeRecovery` (registra
  `(attempt, abortFirst)` e chiamate CleanupMount; errori configurabili; opzionale hook per avanzare il clock), `fakeClock` (Now settabile),
  `recNotifier` (slice di Event). Config test: interval 10s, threshold 3, grace 120s, verify 120s, backoff 10s/5m.
  Report helper: `ok := Report{OK:true}`, `idle := Report{Idle:true}`, `stale := Report{Idle:true, MountPresent:true, MountStale:true}`,
  `probeFail := Report{Failures: []Failure{{"probe","timeout"}}}`, `httpFail := Report{Failures: []Failure{{"http","status 500"}}}`.
  - `TestStartEmitsStartupAndWaiting` (eventi [startup, waiting], state waiting).
  - `TestWaitingIdleStaysWaiting` (3 step idle → nessun Recover/CleanupMount, nessun evento nuovo, return 10s) [I-1].
  - `TestWaitingToMonitoringOnOK` (evento stack_up, state monitoring).
  - `TestWaitingStaleMountCleaned` (stale → CleanupMount(abort=true) 1 volta, evento stale_mount_cleaned; con errore cleanup → nessun evento).
  - `TestWaitingGraceThenRecovery` (probeFail a t0, t0+119s → nessun Recover; t0+120s → eventi failure_detected, recovery_started attempt 1; Recover(1, true); state verifying).
  - `TestWaitingGraceResetsWhenIdle` (probeFail t0, idle t0+60s, probeFail t0+100s, t0+200s → nessuna recovery a t0+200s perché grace ripartita a t0+100s; recovery a t0+220s).
  - `TestMonitoringThresholdTriggersRecovery` (ok→monitoring; fail, fail, ok, fail, fail → nessun Recover; terzo fail consecutivo → Recover).
  - `TestMonitoringIdleGoesWaiting` (monitoring + idle → stack_stopped, state waiting, nessun Recover) [I-1].
  - `TestRecoveryAbortFirstFlag` (3×probeFail → abortFirst=true; altro supervisor 3×httpFail → abortFirst=false).
  - `TestVerifyingSuccess` (dopo recovery, ok → recovery_succeeded attempt 1, state monitoring; recovery successiva usa attempt 1 di nuovo).
  - `TestVerifyingIgnoresIdle` (verifying + idle prima del deadline → resta verifying, nessun evento).
  - `TestVerifyingTimeoutBackoff` (fail fino a deadline 120s → recovery_failed "verify timeout", Step ritorna 10s, state backoff;
    Step a +5s → ritorna 5s, nessun Evaluate/Recover; Step a +10s → Recover(2, true)).
  - `TestRecoverErrorGoesBackoff` (Recover errore → recovery_failed con messaggio, backoff 10s; tentativo 2 dopo 10s, fallisce → backoff 20s).
  - `TestBackoffSequence` (n=0,1..8 con 10s/5m → 10s,10s,20s,40s,80s,160s,5m,5m,5m; n=1000 → 5m; initial==max → max).
  - `TestRunStopsOnContextCancel` (clock reale, interval 10ms, health ok; cancel dopo 50ms → Run ritorna nil entro 1s; ultimo evento shutdown) [I-7].
  Gate G-U2.2.
- **e2e tests:** N/A qui — comportamento reale in E1–E6.
- **Done:** 15 test PASS con `-race`; review agent-1:opus (design prima, diff dopo) registrata; unit chiusa; commit.

### 2.3 Comando `run` (wiring)
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus a P2.
- **Files:** WRITE `cmd/mount-manager/main.go`, `cmd/mount-manager/main_test.go`.
- **Change:**
  Preconditions: 2.2 DONE.
  Contract: `run [--config P]` (default `config.DefaultPath`):
  1. `config.Load` → errore: stderr, exit 3. `exec.LookPath(cfg.DockerBinary)` → errore: stderr, exit 3.
  2. Logger `slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevelValue()}))`.
  3. `os.Geteuid() != 0` → `log.Warn("not running as root: unmount and fuse abort will fail")`.
  4. `exe, err := os.Executable()` (errore → exit 3). Deps: `probe.Runner{Exe: exe, Timeout: cfg.CheckTimeout.Duration}`;
     `cc := compose.NewClient(cfg)`; HTTP client come in 1.3 se `checks.http.url` non vuoto;
     `health.New(cfg, health.Deps{Mounts: mountinfo.Read, Probe: runner.Probe, Compose: cc, HTTP: hc})`;
     `recovery.New(cfg, recovery.Deps{Mounts: mountinfo.Read, Abort: unmount.AbortConnection, Detach: unmount.Detach, Compose: cc, Log: log})`;
     notifier: `notify.NewWebhook(...)` se `webhook.url` non vuoto, altrimenti `notify.Nop{}`.
  5. `ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)`; `sup.Run(ctx)`;
     se webhook: `Close` con ctx timeout 5s. Exit 0.
  Refactor ammesso: estrarre `buildDeps(cfg, log)` condiviso con `check`.
  Steps: S1 — wiring; S2 — test; expected G-U2.3 verde.
  Recovery boundary: none.
  Failure handling: nessuna.
- **Unit tests:** `main_test.go`: `TestRunCommandMissingConfig` (exit 3, stderr contiene "config"); `TestRunCommandInvalidConfig` (YAML con campo sconosciuto → exit 3);
  `TestRunCommandMissingDocker` (config valida con `docker_binary: /nonexistent/docker` → exit 3); `TestUsageListsCommands` (usage contiene run, check, probe, version). Gate G-U2.3.
- **e2e tests:** E1/E2 (2.4) avviano `run` reale.
- **Done:** 4 test PASS + regressione `cmd/mount-manager` verde; unit chiusa; commit.

### 2.4 Harness e2e e scenari non-root E1, E2
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus dopo diff (I-6, oracoli, teardown) e a P2.
- **Files:** READ `compose.yml`; WRITE `README.md` (sezione "Test"); NEW `test/e2e/harness_test.go`, `test/e2e/scenarios_test.go`
  (entrambi con `//go:build e2e`, package `e2e`).
- **Change:**
  Preconditions: 2.3 DONE; immagini `rclone/rclone:latest` e `fabiop85/superdufs:v1.5.0` presenti o scaricabili; nessun container
  `rclone-mount`/`superdufs` esistente; porta 127.0.0.1:5000 libera.
  Harness (`harness_test.go`) — funzioni esatte:
  - `repoRoot()`: da `runtime.Caller(0)` → `filepath.Join(dir, "..", "..")` pulito.
  - `managerBin(t)`: `os.Getenv("MM_BIN")` o `<repo>/bin/mount-manager`; non eseguibile → `t.Fatalf("build first: go build -o bin/mount-manager ./cmd/mount-manager")`.
  - `type env struct { t *testing.T; work, composeFile, mountPath, cfgPath string; hook *hookServer; mgr *exec.Cmd; mgrLog *os.File }`.
  - `newEnv(t)`: preflight (`docker ps -a --format {{.Names}}` non contiene `rclone-mount` né `superdufs`, altrimenti Fatal
    "stop the production stack first"; `net.Listen("tcp","127.0.0.1:5000")` ok e chiuso; `docker image inspect` delle 2 immagini,
    se mancano `docker compose -f <work>/compose.yml pull` timeout 10m). Work dir `<repo>/test/e2e/.work/<t.Name()>-<unixnano>`.
    Copia byte-per-byte `<repo>/compose.yml` → `<work>/compose.yml`. Scrive `<work>/config/rclone.conf` =
    `"[remote]\ntype = alias\nremote = /config/e2e-remote\n"`; crea `<work>/config/e2e-remote/canary.txt` ("canary\n"),
    `<work>/config/e2e-remote/dir1/file.txt` ("x\n"), `<work>/mymount/`. Se euid==0: `chown -R 1000:1000 <work>`
    (filepath.WalkDir + os.Lchown). Chiama `assertRemoteIsLocal(t, work)` [I-6]: il file rclone.conf letto è identico alla stringa sopra
    e il path config è sotto `<work>` e diverso da `<repo>/config`. Registra `t.Cleanup(e.teardown)` per primo.
  - `writeManagerConfig(e, overrides func(*map[string]any))`: YAML con mount_path `<work>/mymount`, compose_file `<work>/compose.yml`,
    check_interval 1s, check_timeout 3s, failure_threshold 2, startup_grace 5s, log_level debug,
    recovery {down_timeout 10s, up_wait_timeout 90s, verify_timeout 60s, command_grace 30s, backoff_initial 2s, backoff_max 8s},
    checks {canary: canary.txt, http {url: "http://127.0.0.1:5000/", timeout 3s, accept_status [200,401,403]}, exec {service: superdufs, path: /data, timeout 10s}},
    webhook {url: <hook.URL>, timeout 2s}.
  - `hookServer`: `httptest.Server` che decodifica `notify.Event`-compatibili (struct locale con stessi tag JSON) in slice protetta da mutex.
    `waitEvent(name string, after int, timeout time.Duration) (ev, idx)` polling 200ms, Fatal con elenco eventi ricevuti se scade.
    `assertNoEvent(name string, after int)`. `count()`.
  - `startManager(e)`: `exec.Command(bin, "run", "--config", cfgPath)`, stdout+stderr su `<work>/manager.log`; `stopManager(e)`: SIGTERM, attesa 20s, poi Kill.
  - `compose(e, timeout, args...)`: `docker compose -f <work>/compose.yml --project-directory <work> args...`; errore → Fatal con output.
  - `hostMountHealthy(e) bool`: mountinfo top a mountPath con fstype `fuse.rclone` E lettura `<mountPath>/canary.txt` == "canary\n"
    eseguita in goroutine con timeout 5s (se scade → false; I-3 vale anche per i test).
  - `runCheck(e) int`: esegue `bin check --config cfgPath` (timeout 2 min), ritorna exit code.
  - `teardown()`: stopManager; `compose down -t 5` (errori loggati); se top mount presente: euid==0 → `syscall.Unmount(path, MNT_DETACH)` in loop (max 5),
    altrimenti `fusermount3 -u -z <path>`; se `t.Failed()` stampa ultime 100 righe di manager.log; `os.RemoveAll(work)` (errori loggati).
  Scenari (`scenarios_test.go`, sequenziali, nessun `t.Parallel`):
  - `TestE2E_WaitThenMonitor` (E1): newEnv; config; startManager; waitEvent startup e waiting (10s); `time.Sleep(3s)`; assertNoEvent stack_up;
    `runCheck` == 2 (idle); `compose up -d --pull never --wait --wait-timeout 120`; waitEvent stack_up (60s); `hostMountHealthy` true;
    `docker compose exec -T superdufs ls /data/canary.txt` ok (superdufs vede il mount → bootstrap D11 funziona); `runCheck` == 0;
    assertNoEvent recovery_started (da indice 0).
  - `TestE2E_ManualDownNoRecovery` (E2): newEnv; config; `compose up … --wait`; startManager; waitEvent stack_up (60s); `compose down -t 10`;
    waitEvent stack_stopped (30s); `time.Sleep(10s)`; assertNoEvent recovery_started (da 0); `docker compose ps -a -q` vuoto [I-1].
  README sezione "Test": comandi G-UNIT e G-E2E-NR, precondizioni (stack produzione fermo, porta 5000), garanzia I-6.
  Steps: S1 — harness; S2 — E1; S3 — E2; S4 — esegui G-E2E-NR; expected 2 PASS.
  Recovery boundary: prima di S4 (esecuzione lunga con container): checkpoint §6 con risorse attive (container, work dir) e comando di pulizia
  `docker compose -f <work>/compose.yml --project-directory <work> down -t 5`.
  Failure handling: E1 fallisce su superdufs che non vede il mount → escalation agent-1:opus (possibile revisione D11); timeout insufficienti →
  escalation, non allungare senza approvazione.
- **Unit tests:** N/A — codice di test; `go vet -tags e2e ./test/e2e/` deve passare (parte di G-FMT-E2E).
- **e2e tests:** G-E2E-NR: `--- PASS: TestE2E_WaitThenMonitor` e `--- PASS: TestE2E_ManualDownNoRecovery` presenti nell'output.
- **Done:** 2 scenari PASS come utente non-root; nessun container/mount residuo dopo il run (`docker ps -a` senza rclone-mount/superdufs, work dir rimossa);
  review agent-1:opus registrata; README "Test" aggiornato; unit chiusa; commit.

## Phase gates and closure
- Required gates: G-FMT (+ G-FMT-E2E), G-UNIT, G-E2E-NR; assertions: tutti i package ok, 2 scenari e2e PASS.
- README obligation: 2.4 aggiorna "Test" e "Stato"; a P2 verificare accuratezza.
- Every sub-phase DONE or explicitly justified SKIPPED. Run full phase gates now.
- Supervisor agent-1:opus reviews integration, I-1, I-2, I-6, I-7, I-8, test e documentazione.
- Open closure unit P2, record review e risultato, mark phase DONE, close. Con full-autonomous:true commit
  `phase(2): complete supervisor, recovery and non-root e2e`.
