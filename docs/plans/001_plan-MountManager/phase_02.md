# Phase 1 — Docker client, webhook, health checker, comando `check`

Intent: `mount-manager check --config P` valuta l'intera suite C1–C5 e restituisce exit 0/1/2/3;
client compose e notifier webhook pronti e testati per la fase 2.
Prerequisites: P0 DONE (package `config`, `mountinfo`, `probe`, CLI dispatch).
Phase closure: P1; review by agent-1:opus.

## State and ownership contract
Read STATE.md §0–1 first for scope, ownership, recovery, checks, and commit rules.
Open before edits; checkpoint at a recovery boundary; close with evidence. A
delegated worker follows its assigned OPEN unit. Missing design goes to agent-1:opus.

## Local design context
Plan revision: 1.
- D2: ogni comando docker = `<docker_binary> compose -f <ComposeFile> --project-directory <ProjectDirectory> <sub> …`.
- D3: suite C1 mount (mountinfo), C2 probe, C3 container, C4 HTTP (se `checks.http.url`), C5 exec (se `checks.exec.service`); canary se `checks.canary`.
- D4: idle = nessun container con State ∈ {running, restarting, paused}.
- D7/I-5: `Up` usa sempre `--pull never`.
- D12: webhook asincrono, coda 64, nessun retry, errori solo loggati.
- D16: `check` exit 0 OK, 1 guasto, 2 idle, 3 errore config/uso.
- D17: default accept_status [200,401,403].
- I-3: `health` non accede mai al filesystem del mount; usa solo `mountinfo` e la funzione probe iniettata.
- I-7: ogni comando esterno ha timeout; `ExecRunner` termina il processo al timeout.
- R1: `docker compose ps -a --format json` produce NDJSON (un oggetto per riga), vuoto se nessun container;
  campi usati: `Name`, `Service`, `State`, `Health`, `ExitCode`. Compose v5.5.1.
- R7: superdufs anonimo `GET /` → 401.

Durate → secondi CLI: `secs(d) = strconv.Itoa(int(d.Round(time.Second) / time.Second))`, minimo "0".

## Sub-phases

### 1.1 Package compose (client CLI)
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus a P1.
- **Files:** NEW `internal/compose/compose.go`, `internal/compose/runner.go`, `internal/compose/compose_test.go`.
- **Change:**
  Preconditions: P0 DONE.
  Contract:
  ```go
  type Runner interface {
      Run(ctx context.Context, timeout time.Duration, name string, args ...string) (stdout, stderr []byte, err error)
  }
  type ExecRunner struct{}
  type Container struct {
      Name     string `json:"Name"`
      Service  string `json:"Service"`
      State    string `json:"State"`
      Health   string `json:"Health"`
      ExitCode int    `json:"ExitCode"`
  }
  type Client struct {
      Runner     Runner
      Docker     string
      File       string
      ProjectDir string
      Grace      time.Duration // cfg.Recovery.CommandGrace
  }
  func NewClient(cfg *config.Config) *Client // Runner: ExecRunner{}, Docker: cfg.DockerBinary, File: cfg.ComposeFile, ProjectDir: cfg.ProjectDirectory, Grace: cfg.Recovery.CommandGrace
  func (c *Client) Services(ctx context.Context) ([]string, error)                       // config --services ; timeout 30s
  func (c *Client) PS(ctx context.Context) ([]Container, error)                          // ps -a --format json ; timeout 30s
  func (c *Client) Down(ctx context.Context, stop time.Duration) error                   // down -t <secs(stop)> ; timeout stop+Grace
  func (c *Client) Kill(ctx context.Context) error                                       // kill ; timeout Grace
  func (c *Client) Up(ctx context.Context, wait time.Duration) error                     // up -d --pull never --wait --wait-timeout <secs(wait)> ; timeout wait+Grace
  func (c *Client) Exec(ctx context.Context, timeout time.Duration, service string, cmd ...string) ([]byte, error) // exec -T <service> <cmd...> ; timeout dato
  func ParsePS(b []byte) ([]Container, error)
  ```
  Argomenti: `base := []string{"compose", "-f", c.File, "--project-directory", c.ProjectDir}` + sotto-argomenti esatti sopra.
  Errori: se Run ritorna err → `fmt.Errorf("docker compose %s: %w: %s", sub, err, strings.TrimSpace(string(stderr)))`
  (stderr troncato a 500 byte).
  `ExecRunner.Run`: `ctx2, cancel := context.WithTimeout(ctx, timeout)`; `cmd := exec.CommandContext(ctx2, name, args...)`;
  `cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }`; `cmd.WaitDelay = 5 * time.Second`;
  stdout/stderr in buffer; se `ctx2.Err() == context.DeadlineExceeded` → err `fmt.Errorf("timeout after %s", timeout)`.
  `ParsePS`: `bytes.TrimSpace`; vuoto → `nil, nil`; primo byte `[` → `json.Unmarshal` in slice; altrimenti per ogni riga non vuota
  `json.Unmarshal` in Container; errore → `fmt.Errorf("parse compose ps output: %w", err)`.
  `Services`: righe di stdout non vuote, trim.
  Steps:
  1. S1 — ParsePS + Client con fake Runner; S2 — ExecRunner; expected G-U1.1 verde.
  Recovery boundary: none.
  Failure handling: nessuna chiamata docker reale nei test unit.
- **Unit tests:** `compose_test.go` con `fakeRunner` che registra `(timeout, name, args)` e restituisce output configurato:
  `TestParsePSNDJSON` (2 righe → 2 container, campi esatti), `TestParsePSArray`, `TestParsePSEmpty` (""/"\n" → len 0, nil),
  `TestParsePSInvalid`; `TestServices` ("rclone-mount\nsuperdufs\n" → 2); `TestPSArgs`
  (args == base+["ps","-a","--format","json"]); `TestDownArgs` (stop 30s, grace 60s → args base+["down","-t","30"], timeout 90s);
  `TestUpArgs` (wait 120s → base+["up","-d","--pull","never","--wait","--wait-timeout","120"], timeout 180s) [I-5];
  `TestKillArgs`; `TestExecArgs` (base+["exec","-T","superdufs","ls","-A","--","/data"], timeout passato);
  `TestRunErrorIncludesStderr` (fake err + stderr "boom" → errore contiene "boom" e "down");
  `TestExecRunnerCapturesOutput` (`sh -c 'echo out; echo err >&2'` → stdout "out\n", stderr "err\n");
  `TestExecRunnerTimeout` (`sleep 5`, timeout 200ms → errore contiene "timeout", durata < 3s) [I-7].
  Gate G-U1.1.
- **e2e tests:** N/A — uso reale in E1–E6.
- **Done:** 13 test PASS; unit chiusa; commit.

### 1.2 Package notify (webhook)
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus a P1.
- **Files:** NEW `internal/notify/notify.go`, `internal/notify/webhook.go`, `internal/notify/webhook_test.go`.
- **Change:**
  Preconditions: P0 DONE.
  Contract:
  ```go
  const (
      EventStartup           = "startup"
      EventWaiting           = "waiting"
      EventStackUp           = "stack_up"
      EventStackStopped      = "stack_stopped"
      EventFailureDetected   = "failure_detected"
      EventRecoveryStarted   = "recovery_started"
      EventRecoverySucceeded = "recovery_succeeded"
      EventRecoveryFailed    = "recovery_failed"
      EventStaleMountCleaned = "stale_mount_cleaned"
      EventShutdown          = "shutdown"
  )
  type Event struct {
      Event     string    `json:"event"`
      Timestamp time.Time `json:"timestamp"`
      Host      string    `json:"host"`
      MountPath string    `json:"mount_path"`
      Detail    string    `json:"detail"`
      Attempt   int       `json:"attempt"`
  }
  type Notifier interface{ Notify(Event) }
  type Nop struct{}; func (Nop) Notify(Event) {}
  type Webhook struct { /* url, headers, client, queue chan Event, log, mu sync.Mutex, closed bool, done chan struct{}, dropped atomic.Int64 */ }
  func NewWebhook(url string, headers map[string]string, timeout time.Duration, log *slog.Logger) *Webhook // capacity 64
  func newWebhook(url string, headers map[string]string, timeout time.Duration, log *slog.Logger, capacity int) *Webhook
  func (w *Webhook) Notify(ev Event)            // non bloccante
  func (w *Webhook) Close(ctx context.Context)  // idempotente; chiude coda, attende sender o ctx
  func (w *Webhook) Dropped() int64
  ```
  Algoritmo: costruttore avvia 1 goroutine sender `for ev := range w.queue { w.send(ev) }; close(w.done)`.
  `send`: `json.Marshal(ev)`; `http.NewRequest(POST)`; header `Content-Type: application/json` + headers config;
  `client := &http.Client{Timeout: timeout}`; errore o status ≥300 → `log.Warn("webhook delivery failed", "event", ev.Event, "error"/"status", …)`;
  body chiuso sempre. Nessun retry.
  `Notify`: `w.mu.Lock()`; se closed → unlock, return; `select { case w.queue <- ev: default: w.dropped.Add(1); log.Warn("webhook queue full, dropping event", …) }`; unlock.
  `Close`: sotto lock se !closed → closed=true, close(queue); poi `select { case <-w.done: case <-ctx.Done(): }`.
  Steps: S1 — tipi e Webhook; S2 — test; expected G-U1.2 verde con `-race`.
  Recovery boundary: none.
  Failure handling: nessuna.
- **Unit tests:** `webhook_test.go` con `httptest.Server`: `TestWebhookPostsJSON` (method POST, Content-Type, header custom `X-Token`,
  body decodificato uguale all'Event inviato incl. timestamp RFC3339); `TestWebhookOrder` (3 eventi → ricevuti in ordine);
  `TestWebhookNon2xxNotRetried` (server 500 → esattamente 1 richiesta dopo Close); `TestWebhookTimeoutDoesNotBlockNotify`
  (server dorme 2s, timeout 100ms; 5 Notify completano in < 50ms totali) [I-7]; `TestWebhookQueueFullDrops` (capacity 2, server
  bloccato su canale; 10 Notify ritornano subito; `Dropped() >= 7`); `TestCloseDrains` (3 eventi, Close con ctx 5s → 3 ricevuti);
  `TestNotifyAfterCloseNoPanic`; `TestNopNotify`. Gate G-U1.2.
- **e2e tests:** N/A — il webhook è l'oracolo degli e2e (2.4, 3.2).
- **Done:** 8 test PASS con `-race`; unit chiusa; commit.

### 1.3 Package health e comando `check`
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus dopo diff (I-3, ordine controlli, semantica idle) e a P1.
- **Files:** READ `internal/config`, `internal/mountinfo`, `internal/probe`, `internal/compose`; WRITE `cmd/mount-manager/main.go`,
  `cmd/mount-manager/main_test.go`, `README.md` (riga "Stato"); NEW `internal/health/health.go`, `internal/health/health_test.go`.
- **Change:**
  Preconditions: 1.1 DONE.
  Contract:
  ```go
  type ComposeAPI interface {
      Services(ctx context.Context) ([]string, error)
      PS(ctx context.Context) ([]compose.Container, error)
      Exec(ctx context.Context, timeout time.Duration, service string, cmd ...string) ([]byte, error)
  }
  type Deps struct {
      Mounts  func() ([]mountinfo.Entry, error)
      Probe   func(ctx context.Context, path, canary string) probe.Result
      Compose ComposeAPI
      HTTP    *http.Client // nil se checks.http.url vuoto
  }
  type Failure struct{ Check, Detail string } // Check ∈ {"mount","probe","docker","containers","http","exec"}
  type Report struct {
      Idle, OK, MountPresent, MountStale bool
      MountEntry mountinfo.Entry
      Running    int
      Failures   []Failure
  }
  func (r Report) Summary() string            // "ok" | "idle" | "<check>: <detail>; <check>: <detail>"
  func (r Report) HasMountFailure() bool      // true se esiste Failure con Check "mount" o "probe"
  type Checker struct { /* cfg *config.Config; d Deps; services []string */ }
  func New(cfg *config.Config, d Deps) *Checker
  func (c *Checker) Evaluate(ctx context.Context) Report
  ```
  Algoritmo `Evaluate` (ordine obbligatorio):
  1. C1: `entries, err := d.Mounts()`; err → Failure{mount, "read mountinfo: …"}. Altrimenti `top, ok := mountinfo.Top(entries, cfg.MountPath)`:
     !ok → Failure{mount, "not mounted"}; ok && top.FSType != cfg.ExpectedFSType → Failure{mount, "unexpected fstype <x>"};
     altrimenti MountPresent=true, MountEntry=top.
  2. C3: se `c.services == nil` → `Services(ctx)`; successo → cache (solo se len>0). Poi `PS(ctx)`. Qualsiasi errore → Failure{docker, err}
     (Idle resta false). Altrimenti: Running = conteggio State ∈ {running, restarting, paused}; Idle = Running == 0.
     Per ogni servizio in cache: nessun container con quel Service → Failure{containers, "<svc>: missing"};
     State != "running" → Failure{containers, "<svc>: state=<State>"}; Health ∉ {"", "healthy"} → Failure{containers, "<svc>: health=<Health>"}.
  3. Se Idle: se MountPresent → `res := d.Probe(ctx, MountPath, "")`; `MountStale = !res.OK`. Ritorna Report con OK=false (Failures come raccolte).
  4. C2: se MountPresent: `res := d.Probe(ctx, MountPath, cfg.Checks.Canary)`; !res.OK → Failure{probe, res.Error}; hostEntries = res.HasEntries.
  5. C4: se `cfg.Checks.HTTP.URL != ""`: `GET` con `http.NewRequestWithContext`; errore → Failure{http, err}; status ∉ AcceptStatus →
     Failure{http, "status <n>"}; chiudi body.
  6. C5: se `cfg.Checks.Exec.Service != ""` e C2 eseguito con OK: `out, err := Exec(ctx, Exec.Timeout, svc, "ls", "-A", "--", Exec.Path)`;
     err → Failure{exec, err}; altrimenti se hostEntries && `strings.TrimSpace(string(out)) == ""` →
     Failure{exec, "container sees empty directory while host mount has entries"}; se canary != "" →
     `Exec(ctx, …, "ls", "-d", "--", path.Join(Exec.Path, canary))` errore → Failure{exec, "canary: …"}.
  7. OK = len(Failures) == 0 && !Idle.
  HTTP client (costruito dal chiamante): `&http.Client{Timeout: cfg.Checks.HTTP.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}`.
  Comando `check [--config P]` in main.go: `config.Load` (errore → stderr, exit 3); `exe, _ := os.Executable()`;
  `probe.Runner{Exe: exe, Timeout: cfg.CheckTimeout.Duration}`; `compose.NewClient(cfg)`; ctx con timeout 5 min;
  `rep := health.New(cfg, deps).Evaluate(ctx)`; stampa su stdout `status: ok|idle|failing` e una riga `- <check>: <detail>` per failure;
  se MountStale aggiungi riga `- mount: stale`; exit 0 OK, 2 Idle, 1 altrimenti. Usage aggiornato.
  README: riga "Stato" → menziona che `check` è disponibile.
  Steps: S1 — health + test fake; S2 — comando check + test; expected G-U1.3 verde.
  Recovery boundary: none.
  Failure handling: dubbi sulla semantica idle/failure → escalation agent-1:opus.
- **Unit tests:** `health_test.go` con fake (Mounts restituisce slice configurata; Probe registra chiamate e restituisce Result; ComposeAPI fake con
  contatore chiamate Services; HTTP via `httptest.Server`). Config base: MountPath `/mnt/m`, fstype `fuse.rclone`, servizi `rclone-mount`,`superdufs`.
  `TestEvaluateAllOK` (tutti i check abilitati, OK=true, Summary "ok"); `TestEvaluateIdleNoContainers` (PS vuoto, mount assente → Idle, !OK, Probe NON chiamato);
  `TestEvaluateIdleStaleMount` (PS vuoto, mount presente, probe !OK → MountStale=true); `TestEvaluateMountMissingWhileRunning`
  (Failure mount, Probe e Exec non chiamati); `TestEvaluateWrongFSType` (ext4 al path → Failure mount "unexpected fstype ext4", MountPresent=false);
  `TestEvaluateProbeTimeout` (Result{Timeout:true, Error:"probe timeout after 10s"} → Failure probe con "timeout"; HasMountFailure=true);
  `TestEvaluateContainerNotRunning` (rclone State "exited", superdufs running → Idle=false, Failure "rclone-mount: state=exited");
  `TestEvaluateContainerUnhealthy`; `TestEvaluateContainerMissing`; `TestEvaluateDockerError` (PS err → Failure docker, Idle=false);
  `TestEvaluateHTTPAccepted` (401 → nessun failure); `TestEvaluateHTTPRejected` (500 → "status 500", HasMountFailure=false);
  `TestEvaluateHTTPDisabled` (URL vuoto, HTTP nil → nessuna richiesta); `TestEvaluateExecEmptyWhileHostHasEntries`;
  `TestEvaluateExecCanaryMissing`; `TestServicesCached` (2 Evaluate → Services chiamato 1 volta); `TestReportSummary`.
  `main_test.go`: `TestCheckCommandConfigError` (config inesistente → exit 3). Gate G-U1.3.
- **e2e tests:** exit code reali di `check` verificati in E1/E2 (2.4).
- **Done:** 18 test PASS; review agent-1:opus registrata; README "Stato" aggiornato; unit chiusa; commit.

## Phase gates and closure
- Required gates: G-FMT, G-UNIT; assertions: package `internal/compose`, `internal/notify`, `internal/health` presenti e `ok`; nessuna regressione fase 0.
- README obligation: 1.3 aggiorna "Stato"; a P1 verificare accuratezza.
- Every sub-phase DONE or explicitly justified SKIPPED. Run full phase gates now.
- Supervisor agent-1:opus reviews integration, I-3, I-5, I-7, test e documentazione.
- Open closure unit P1, record review e risultato, mark phase DONE, close. Con full-autonomous:true commit
  `phase(1): complete docker client, webhook and health checks`.
