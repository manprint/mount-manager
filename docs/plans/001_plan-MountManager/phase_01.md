# Phase 0 — Fondamenta: repository, compose bootstrap, config, primitive host

Intent: repository git con baseline, modulo Go compilabile con CLI minima, `compose.yml` con bootstrap
(superdufs dopo rclone healthy), package `config`, `mountinfo`, `unmount`, `probe` testati.
Prerequisites: none.
Phase closure: P0; review by agent-1:opus.

## State and ownership contract
Read STATE.md §0–1 first for scope, ownership, recovery, checks, and commit rules.
Open before edits; checkpoint at a recovery boundary; close with evidence. A
delegated worker follows its assigned OPEN unit. Missing design goes to the
configured supervisor (agent-1:opus).

## Local design context
Plan revision: 1.
- D1: Go module `mount-manager` (`go 1.26` in go.mod), binario `cmd/mount-manager`, unica dipendenza
  `go.yaml.in/yaml/v3` (aggiungere con `go get go.yaml.in/yaml/v3@latest`). Log con `log/slog`.
  Codice/commenti/log in inglese.
- D9: il processo manager non tocca mai il filesystem del mount; solo il subprocess `probe` lo fa (I-3).
- D10: detach = `syscall.Unmount(path, syscall.MNT_DETACH)`; abort = scrivere `"1"` in
  `/sys/fs/fuse/connections/<minor>/abort`. EINVAL/ENOENT su detach = già smontato (nil).
- D11: healthcheck `rclone-mount` + `depends_on` superdufs (testo YAML esatto in 0.1).
- D16: CLI dispatch in `cmd/mount-manager/main.go`: `func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }`
  e `func run(args []string, stdout, stderr io.Writer) int`. Sottocomandi con `flag.NewFlagSet(name, flag.ContinueOnError)`,
  output flag su `stderr`. Exit 3 = uso/config errati.
- I-2 (enforcement in 2.1): i package di questa fase espongono `Entry.FSType` e `Entry.Minor` necessari.
- I-9: `compose.yml` — tutte le chiavi esistenti restano identiche (incluso il doppio `--size-only`);
  solo aggiunte.
- R3: numero connessione fusectl = minor del device (`mountinfo` campo 3 `major:minor`).
- R4: `EPERM` = serve CAP_SYS_ADMIN (root); `EINVAL` non mount point; `ENOENT` path inesistente.
- R9: usare `go.yaml.in/yaml/v3` (non `gopkg.in/yaml.v3`).

Formato `/proc/self/mountinfo` (una riga per mount):
`36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue`
campi: 0 mount ID, 1 parent ID, 2 `major:minor`, 3 root, 4 mount point, 5 opzioni mount, 6..k campi opzionali
fino al separatore `-`, poi fstype, source, super options. Mount point e root usano escape ottali
`\040` (spazio), `\011` (tab), `\012` (newline), `\134` (backslash).

## Sub-phases

### 0.1 Baseline git, scheletro Go, compose bootstrap, README iniziale
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus a P0.
- **Files:** READ `compose.yml`, `Specifications.txt`; WRITE `compose.yml`;
  NEW `.gitignore`, `go.mod`, `go.sum`, `cmd/mount-manager/main.go`, `cmd/mount-manager/main_test.go`,
  `test/composefile/compose_test.go`, `README.md`.
- **Change:**
  Preconditions: directory non è un repo git; file presenti `compose.yml`, `Specifications.txt`, `docs/plans/001_plan-MountManager/*`.
  Contract: `mount-manager version` → stdout `mount-manager <version>\n`, exit 0 (`var version = "dev"`).
  Nessun argomento o comando sconosciuto → usage su stderr (elenca `run`, `check`, `probe`, `version`), exit 3.
  Comandi `run`/`check`/`probe` non ancora implementati → NON aggiungerli ora (arrivano in 0.4, 1.3, 2.3); finché mancano sono "sconosciuti".
  Scope: nessuna altra modifica al compose.
  Steps:
  1. S1 — `git init -b main`; crea `.gitignore` con righe esatte: `/bin/`, `/config/`, `/mymount/`, `/test/e2e/.work/`, `*.test`.
     Commit baseline con SOLO: `.gitignore`, `compose.yml` (originale, non ancora modificato), `Specifications.txt`,
     `docs/plans/001_plan-MountManager/`. Messaggio: `chore: baseline before plan 001` (senza trailer PEV).
     Registra lo SHA come "Plan baseline" nell'intestazione di STATE.md. Expected: `git log --oneline` = 1 commit.
  2. S2 — `go mod init mount-manager`; imposta `go 1.26`; `go get go.yaml.in/yaml/v3@latest` (la usa 0.2 e il test compose;
     serve già qui per `test/composefile`). Crea `main.go`/`main_test.go` col contratto sopra. Expected: `go build ./...` ok.
  3. S3 — Modifica `compose.yml`: nel servizio `rclone-mount`, dopo `stop_grace_period: 30s` e prima di `command:`, aggiungi:
     ```yaml
         healthcheck:
           test: ["CMD-SHELL", "grep -q ' /mnt/mount fuse.rclone ' /proc/mounts"]
           interval: 15s
           timeout: 10s
           retries: 3
           start_period: 60s
           start_interval: 2s
     ```
     Nel servizio `superdufs`, dopo `restart: always`, aggiungi:
     ```yaml
         depends_on:
           rclone-mount:
             condition: service_healthy
             restart: true
     ```
     Expected: `docker compose -f compose.yml config -q` exit 0.
  4. S4 — `test/composefile/compose_test.go` (package `composefile_test`, solo test): legge `../../compose.yml`
     con `go.yaml.in/yaml/v3` in `map[string]any`. Poi `go mod tidy` (la dipendenza resta perché importata dal test).
     Expected: test verdi (sotto); `go.mod` contiene `go.yaml.in/yaml/v3`.
  5. S5 — `README.md` (italiano) scheletro: titolo, "Scopo" (3–5 righe dallo scenario), "Stato: in sviluppo — piano
     `docs/plans/001_plan-MountManager/`", "Build: `go build -o bin/mount-manager ./cmd/mount-manager`".
  Recovery boundary: dopo S1 (commit baseline): registra SHA in STATE.md §1/§6 prima di proseguire.
  Failure handling: `docker compose config -q` fallisce → controlla indentazione (2 spazi, sotto servizio); nessun altro cambio al file.
- **Unit tests:** `cmd/mount-manager/main_test.go`: `TestVersionCommand` (exit 0, stdout == "mount-manager dev\n"),
  `TestUnknownCommand` (args `["nope"]` → exit 3, stderr contiene "usage"), `TestNoArgs` (exit 3).
  `test/composefile/compose_test.go`: `TestComposeBootstrap` — `services.rclone-mount.healthcheck.test` è lista di 2 elementi,
  [0]=="CMD-SHELL", [1] contiene "fuse.rclone" e "/mnt/mount"; `start_interval`=="2s";
  `services.superdufs.depends_on.rclone-mount.condition`=="service_healthy", `restart`==true.
  `TestComposeUnchangedFields` — `name`=="rclone-stack-superdufs"; rclone `restart` stringificato ∈ {"no","false"};
  `pull_policy`=="always" per entrambi; rclone `volumes` contiene "./mymount:/mnt/mount:rshared" e "./config:/config:rw";
  superdufs `volumes` == ["./mymount:/data"]; rclone `command` ha 30 elementi, [0]=="--config=/config/rclone.conf",
  [1]=="mount", [2]=="remote:/", [3]=="/mnt/mount"; superdufs `image`=="fabiop85/superdufs:v1.5.0"; `ports`==["5000:5000"].
  Gate G-U0.1.
- **e2e tests:** N/A — il bootstrap reale del compose è verificato da E1 (2.4).
- **Done:** baseline commit esiste e SHA registrato; G-U0.1 e G-BASE passano con 5 test PASS; README scheletro;
  unit chiusa in STATE.md; commit di completamento (full-autonomous).

### 0.2 Package config
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus a P0.
- **Files:** NEW `internal/config/config.go`, `internal/config/config_test.go`.
- **Change:**
  Preconditions: 0.1 DONE.
  Contract (tipi esatti):
  ```go
  type Duration struct{ time.Duration }
  func (d *Duration) UnmarshalYAML(n *yaml.Node) error // scalar → time.ParseDuration; errore: fmt.Errorf("invalid duration %q: %w", n.Value, err)
  type Config struct {
      MountPath        string   `yaml:"mount_path"`
      ComposeFile      string   `yaml:"compose_file"`
      ProjectDirectory string   `yaml:"project_directory"`
      DockerBinary     string   `yaml:"docker_binary"`
      ExpectedFSType   string   `yaml:"expected_fstype"`
      CheckInterval    Duration `yaml:"check_interval"`
      CheckTimeout     Duration `yaml:"check_timeout"`
      FailureThreshold int      `yaml:"failure_threshold"`
      StartupGrace     Duration `yaml:"startup_grace"`
      LogLevel         string   `yaml:"log_level"`
      Recovery         Recovery `yaml:"recovery"`
      Checks           Checks   `yaml:"checks"`
      Webhook          Webhook  `yaml:"webhook"`
  }
  type Recovery struct {
      DownTimeout    Duration `yaml:"down_timeout"`
      UpWaitTimeout  Duration `yaml:"up_wait_timeout"`
      VerifyTimeout  Duration `yaml:"verify_timeout"`
      CommandGrace   Duration `yaml:"command_grace"`
      BackoffInitial Duration `yaml:"backoff_initial"`
      BackoffMax     Duration `yaml:"backoff_max"`
  }
  type Checks struct {
      Canary string    `yaml:"canary"`
      HTTP   HTTPCheck `yaml:"http"`
      Exec   ExecCheck `yaml:"exec"`
  }
  type HTTPCheck struct {
      URL          string   `yaml:"url"`
      Timeout      Duration `yaml:"timeout"`
      AcceptStatus []int    `yaml:"accept_status"`
  }
  type ExecCheck struct {
      Service string   `yaml:"service"`
      Path    string   `yaml:"path"`
      Timeout Duration `yaml:"timeout"`
  }
  type Webhook struct {
      URL     string            `yaml:"url"`
      Headers map[string]string `yaml:"headers"`
      Timeout Duration          `yaml:"timeout"`
  }
  const DefaultPath = "/etc/mount-manager/config.yml"
  func Load(path string) (*Config, error)      // os.ReadFile + LoadBytes; errore lettura: "config: read %s: %w"
  func LoadBytes(data []byte) (*Config, error) // decode strict → applyDefaults → Validate
  func (c *Config) Validate() error            // errors.Join di tutti gli errori
  ```
  Decoding: `dec := yaml.NewDecoder(bytes.NewReader(data)); dec.KnownFields(true)`; documento vuoto → errore
  "config: empty document". Errore decode → `fmt.Errorf("config: %w", err)`.
  Default (applicati quando il valore è zero): docker_binary `docker`; expected_fstype `fuse.rclone`;
  check_interval 10s; check_timeout 10s; failure_threshold 3; startup_grace 120s; log_level `info`;
  recovery: down_timeout 30s, up_wait_timeout 120s, verify_timeout 120s, command_grace 60s,
  backoff_initial 10s, backoff_max 5m; checks.http.timeout 5s; checks.http.accept_status [200,401,403]
  (solo se lista vuota); checks.exec.path `/data` (solo se service non vuoto e path vuoto); checks.exec.timeout 10s;
  webhook.timeout 5s; project_directory = `filepath.Dir(compose_file)`.
  Regole Validate (ogni errore formato `config: <campo yaml con path puntato>: <motivo>`):
  - `mount_path`: obbligatorio; `filepath.IsAbs`; `== filepath.Clean(v)`; `!= "/"`.
  - `compose_file`: obbligatorio; assoluto; `os.Stat` ok e file regolare.
  - `project_directory`: assoluto; `os.Stat` ok e directory.
  - `expected_fstype`, `docker_binary`: non vuoti dopo default.
  - durate: tutte > 0 dopo default (valori negativi → errore "must be > 0").
  - `failure_threshold` ≥ 1 (negativo → errore; 0 → default 3).
  - `log_level` ∈ {debug, info, warn, error}.
  - `recovery.backoff_initial` ≤ `recovery.backoff_max`.
  - `checks.canary`: se non vuoto: non assoluto, `== filepath.Clean(v)`, `!= "."`, non inizia con `..`.
  - `checks.http.url` e `webhook.url`: se non vuoti: `url.Parse` ok, scheme ∈ {http, https}, host non vuoto.
  - `checks.http.accept_status`: ogni valore 100..599.
  - `checks.exec.path`: se service non vuoto: path assoluto POSIX (`strings.HasPrefix(p, "/")`).
  - `webhook.headers`: chiavi non vuote.
  Helper: `func (c *Config) LogLevelValue() slog.Level`.
  Steps:
  1. S1 — tipi, `UnmarshalYAML`, `LoadBytes`, `applyDefaults`, `Validate`, `Load`; expected `go vet ./internal/config` ok.
  2. S2 — test sotto; expected G-U0.2 verde.
  Recovery boundary: none.
  Failure handling: comportamento `KnownFields` diverso dall'atteso → escalation supervisor.
- **Unit tests:** `internal/config/config_test.go` (temp dir con file `compose.yml` fittizio creato dal test):
  `TestLoadMinimalAppliesDefaults` (solo mount_path + compose_file → tutti i default sopra, project_directory = dir del compose);
  `TestLoadFullOverrides` (ogni campo impostato → valori letti; durate "1m30s" = 90s);
  `TestLoadUnknownFieldRejected` (`mount_pth:` → errore contiene "mount_pth");
  `TestLoadInvalidDuration` (`check_interval: 10x` → errore contiene "invalid duration");
  `TestLoadEmptyDocument`;
  `TestValidateErrors` (table, ognuno verifica sottostringa campo): mount_path relativo, "/", "/a/../b"; compose_file mancante;
  canary "/abs", "../x", "."; http url "ftp://h"; accept_status 99; backoff_initial 10m > max 5m; failure_threshold -1;
  log_level "trace"; exec.service "superdufs" + path "data"; webhook url "not a url"; header chiave vuota;
  `TestValidateAggregatesErrors` (2 errori → entrambi nel messaggio).
  Gate G-U0.2.
- **e2e tests:** N/A — solo parsing.
- **Done:** tutti i test sopra PASS (≥7 funzioni + subtest); unit chiusa; commit.

### 0.3 Package mountinfo e unmount
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus a P0.
- **Files:** NEW `internal/mountinfo/mountinfo.go`, `internal/mountinfo/mountinfo_test.go`,
  `internal/unmount/unmount.go`, `internal/unmount/unmount_test.go`.
- **Change:**
  Preconditions: 0.1 DONE.
  Contract:
  ```go
  // package mountinfo
  type Entry struct {
      MountID, ParentID int
      Major, Minor      int
      Root, MountPoint  string // unescaped
      Options           string
      FSType, Source    string
      SuperOptions      string
  }
  func Parse(r io.Reader) ([]Entry, error)          // riga malformata → fmt.Errorf("mountinfo: line %d: %s", n, motivo)
  func Read() ([]Entry, error)                       // os.Open("/proc/self/mountinfo") + Parse
  func Top(entries []Entry, path string) (Entry, bool) // ULTIMA entry con MountPoint == path (mount impilati: il più recente è in fondo)
  func Unescape(s string) string                     // \040 \011 \012 \134
  // package unmount (linux)
  var ConnectionsDir = "/sys/fs/fuse/connections"
  func AbortConnection(minor int) error // os.WriteFile(filepath.Join(ConnectionsDir, strconv.Itoa(minor), "abort"), []byte("1"), 0) ; errors.Is(err, fs.ErrNotExist) → nil ; altri → fmt.Errorf("abort fuse connection %d: %w", minor, err)
  func Detach(path string) error        // classify(syscall.Unmount(path, syscall.MNT_DETACH), path)
  func classify(err error, path string) error // nil→nil; EINVAL→nil; ENOENT→nil; EPERM→fmt.Errorf("detach %s: permission denied (root required): %w", path, err); altri→fmt.Errorf("detach %s: %w", path, err)
  ```
  Parse: split per spazi (`strings.Fields`); servono ≥10 campi; trova indice del primo `-` a partire dal campo 6;
  dopo `-` servono ≥2 campi (fstype, source; super options opzionale); `major:minor` via `strings.Cut` + `strconv.Atoi`.
  Righe vuote ignorate.
  Steps:
  1. S1 — mountinfo + test; expected G-U0.3 parte mountinfo verde.
  2. S2 — unmount + test; expected G-U0.3 verde.
  Recovery boundary: none.
  Failure handling: nessuna chiamata reale a Detach nei test (richiede root; coperto da E3–E6).
- **Unit tests:** `mountinfo_test.go`: `TestParseBasic` (fixture 3 righe con opzionali `shared:1 master:2` e senza →
  campi esatti incl. Major/Minor, FSType `fuse.rclone`, Source `remote:`); `TestParseEscapedMountPoint`
  (`/mnt/my\040mount` → `/mnt/my mount`); `TestParseMalformed` (riga senza `-` → errore contiene "line 2");
  `TestTopReturnsLastStacked` (2 entry stesso path → la seconda); `TestTopNotFound`; `TestReadSelf` (Read() ha ≥1 entry con MountPoint "/").
  `unmount_test.go`: `TestAbortConnectionWritesFile` (ConnectionsDir = t.TempDir con `58/abort` vuoto → contenuto "1");
  `TestAbortConnectionMissingIsNil` (minor inesistente → nil); `TestClassifyDetachError` (table: nil, EINVAL, ENOENT → nil;
  EPERM → errore contiene "root required"; EBUSY → errore contiene path e `errors.Is(err, syscall.EBUSY)`).
  Gate G-U0.3.
- **e2e tests:** N/A — Detach/Abort reali verificati in E3–E6 (3.2).
- **Done:** 9 test PASS; unit chiusa; commit.

### 0.4 Package probe e sottocomando `probe`
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus dopo diff (lifecycle subprocess, I-3, I-7) e a P0.
- **Files:** READ `cmd/mount-manager/main.go`; WRITE `cmd/mount-manager/main.go`, `cmd/mount-manager/main_test.go`;
  NEW `internal/probe/probe.go`, `internal/probe/probe_test.go`.
- **Change:**
  Preconditions: 0.1, 0.3 DONE.
  Contract:
  ```go
  type Result struct {
      OK         bool   `json:"ok"`
      HasEntries bool   `json:"has_entries"`
      Timeout    bool   `json:"timeout,omitempty"`
      Error      string `json:"error,omitempty"`
  }
  func Local(path, canary string) Result
  type Runner struct {
      Exe     string        // os.Executable() in produzione
      Prefix  []string      // argomenti prima di "probe" (test: ["-test.run=TestHelperProcess", "--"])
      Env     []string      // aggiunti a os.Environ()
      Timeout time.Duration // cfg.CheckTimeout
      stuck   atomic.Int32
  }
  const maxStuck = 3
  func (r *Runner) Probe(ctx context.Context, path, canary string) Result
  ```
  `Local`: (1) `os.Stat(path)`: errore → `Result{Error: err.Error()}`; non directory → Error "not a directory".
  (2) `f, err := os.Open(path)`; `names, err := f.Readdirnames(1)`; `io.EOF` → HasEntries=false senza errore;
  altro errore → Error; len(names)>0 → HasEntries=true; chiudi f. (3) se canary != "": `os.Stat(filepath.Join(path, canary))`;
  errore → Error `"canary: " + err.Error()`. (4) altrimenti OK=true.
  `Probe` algoritmo:
  1. se `r.stuck.Load() >= maxStuck` → `Result{Error: "probe_stuck: 3 abandoned probe processes"}` senza avviare nulla.
  2. `args := append(append([]string{}, r.Prefix...), "probe", "--path", path)`; se canary != "" aggiungi `"--canary", canary`.
     `cmd := exec.Command(r.Exe, args...)`; `cmd.Env = append(os.Environ(), r.Env...)`; stdout in `bytes.Buffer`; stderr scartato.
     NON usare CommandContext (gestione manuale sotto). `cmd.Start()` errore → `Result{Error: "probe start: ..."}`.
  3. `done := make(chan error, 1); go func(){ done <- cmd.Wait() }()`; `timer := time.NewTimer(r.Timeout)`.
  4. `select`: `err := <-done` → decodifica JSON di stdout in Result; errore decode → `Result{Error: "probe output invalid: ..."}`
     (includi `err` di Wait se non nil); ritorna. `<-timer.C` o `<-ctx.Done()` → `cmd.Process.Kill()`; poi
     `select { case <-done: case <-time.After(2*time.Second): r.stuck.Add(1); go func(){ <-done; r.stuck.Add(-1) }() }`;
     ritorna `Result{Timeout: true, Error: fmt.Sprintf("probe timeout after %s", r.Timeout)}` (per ctx: "probe canceled").
  Sottocomando in `main.go`: `probe --path P [--canary C]` → `json.NewEncoder(stdout).Encode(probe.Local(P, C))`, exit 0;
  `--path` mancante → stderr usage, exit 3. Aggiorna usage.
  Steps:
  1. S1 — `Local` + test; expected verdi.
  2. S2 — `Runner.Probe` + helper process test; expected verdi, incluso timeout < 5 s.
  3. S3 — sottocomando + `TestProbeCommand*`; expected G-U0.4 verde.
  Recovery boundary: none.
  Failure handling: race detector segnala problemi su `stuck` → usare solo atomic; test instabili sul timing → escalation, non allentare soglie.
- **Unit tests:** `internal/probe/probe_test.go`: `TestLocalEmptyDir` (OK, !HasEntries); `TestLocalWithEntries`;
  `TestLocalCanaryPresent`; `TestLocalCanaryMissing` (!OK, Error contiene "canary"); `TestLocalNotDir` (file regolare);
  `TestLocalMissingPath`. Helper: `TestHelperProcess` — se `os.Getenv("MM_PROBE_HELPER") == ""` return; modalità `ok`
  (stampa `{"ok":true,"has_entries":true}` e `os.Exit(0)`), `sleep` (`time.Sleep(30*time.Second)`), `garbage` (stampa `xx`, exit 0).
  `TestRunnerOK` (Result.OK && HasEntries); `TestRunnerTimeout` (sleep, Timeout 300ms → Timeout=true, durata < 5s);
  `TestRunnerGarbageOutput` (Error contiene "probe output invalid"); `TestRunnerStuckGuard` (stesso package: `r.stuck.Store(3)` → Error
  contiene "probe_stuck", nessun processo avviato: Exe = "/nonexistent" e nessun errore start); `TestRunnerContextCanceled` (ctx già cancellato → Timeout=true, Error "probe canceled").
  `cmd/mount-manager/main_test.go`: `TestProbeCommandOK` (temp dir con un file → JSON ok:true,has_entries:true, exit 0);
  `TestProbeCommandMissingPath` (exit 3). Gate G-U0.4 (con `-race`).
- **e2e tests:** N/A — E4 (3.2) copre probe su FUSE appeso reale.
- **Done:** tutti i test PASS con `-race`; review agent-1:opus registrata in STATE.md §7; unit chiusa; commit.

## Phase gates and closure
- Required gates: G-FMT, G-UNIT, G-BASE; assertions: `go test ./...` scopre i package `cmd/mount-manager`,
  `internal/config`, `internal/mountinfo`, `internal/unmount`, `internal/probe`, `test/composefile` tutti `ok`.
- README obligation: 0.1 crea lo scheletro; a P0 verificare che "Stato" e "Build" siano accurati.
- Every sub-phase DONE or explicitly justified SKIPPED. Run full phase gates now.
- Supervisor agent-1:opus reviews integration, I-2/I-3/I-9 prerequisiti, test e documentazione.
- Open closure unit P0, record review e risultato, mark phase DONE, close. Con full-autonomous:true commit
  `phase(0): complete foundations and review`. Never fabricate an empty commit.
