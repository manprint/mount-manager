# Phase 3 — Deploy systemd, e2e root di recovery, README completo

Intent: file di deploy (unit systemd + config di esempio) verificati staticamente; scenari root E3–E6 verdi
tramite `sudo -n <repo>/e2e-root.sh`; README italiano completo. Chiude lo scenario di riferimento.
Prerequisites: P2 DONE.
Phase closure: P3 (finale); review by agent-1:opus.

## State and ownership contract
Read STATE.md §0–1 first for scope, ownership, recovery, checks, and commit rules.
Open before edits; checkpoint at a recovery boundary; close with evidence. A
delegated worker follows its assigned OPEN unit. Missing design goes to agent-1:opus.

## Local design context
Plan revision: 1.
- D13/I-4: unit root, nessuna direttiva di namespacing filesystem. Vietate: `PrivateTmp`, `PrivateDevices`, `PrivateMounts`,
  `ProtectSystem`, `ProtectHome`, `ReadOnlyPaths`, `ReadWritePaths`, `InaccessiblePaths`, `BindPaths`, `BindReadOnlyPaths`,
  `TemporaryFileSystem`, `MountFlags`, `DynamicUser`, `RootDirectory`, `RootImage` (R5: disattivano la propagazione dei mount verso l'host).
- Esclusione: NON installare su `/usr/local/bin`, `/etc`, NON `systemctl daemon-reload/enable/start`. Solo file nel repo + verifica statica.
- D15/R6: sudoers utente `(root) NOPASSWD: /mnt/fabio/dati/Git/SperimentazioniAI/mount-manager/*`; `*` non matcha `/` →
  l'entrypoint root deve stare nella root del repo e va invocato con path assoluto. PATH di root può non includere `/usr/local/go/bin`:
  compilare binari come utente prima.
- I-6: e2e root usano lo stesso harness (remote alias locale); mai `./config`.
- R3/R8/R10: `docker kill rclone-mount` → mount host stale ENOTCONN; `docker pause` → FUSE appeso; abort fusectl sblocca.
- D14: stack idle + mount stale → cleanup senza avvio stack.
- D5: stato post-boot (superdufs running, rclone fermo) → recovery dopo `startup_grace`.

## Sub-phases

### 3.1 Unit systemd e config di esempio
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus a P3.
- **Files:** WRITE `internal/config/config_test.go`; NEW `deploy/mount-manager.service`, `deploy/config.example.yml`.
- **Change:**
  Preconditions: P2 DONE.
  Contract — `deploy/mount-manager.service` esatto:
  ```ini
  [Unit]
  Description=mount-manager supervisor for the rclone + superdufs compose stack
  Wants=network-online.target docker.service
  After=network-online.target docker.service

  [Service]
  Type=simple
  ExecStart=/usr/local/bin/mount-manager run --config /etc/mount-manager/config.yml
  Restart=always
  RestartSec=5
  TimeoutStopSec=90
  KillSignal=SIGTERM
  # Must run in the host mount namespace: do not add PrivateTmp=, ProtectSystem=, ProtectHome=,
  # PrivateMounts= or any other filesystem namespacing option (unmounts would not reach the host).

  [Install]
  WantedBy=multi-user.target
  ```
  `deploy/config.example.yml`: tutte le chiavi dello schema (0.2) con valori di default espliciti e commento inglese di una riga ciascuna;
  `mount_path: /opt/rclone-stack/mymount`, `compose_file: /opt/rclone-stack/compose.yml`, `project_directory: ""` (commento: default dir del compose),
  `checks.canary: ""`, `checks.http.url: "http://127.0.0.1:5000/"`, `checks.exec.service: superdufs`, `webhook.url: ""`, `webhook.headers: {}`.
  Steps: S1 — i due file; S2 — `TestExampleConfigLoads` in config_test.go: legge `../../deploy/config.example.yml`, sostituisce
  `/opt/rclone-stack/compose.yml` con un compose temporaneo esistente e `/opt/rclone-stack` restanti con la temp dir, `LoadBytes` ok,
  http url e exec service valorizzati; S3 — gate G-SYSTEMD.
  Recovery boundary: none.
  Failure handling: `systemd-analyze verify` segnala warning su unit di sistema non correlate → valutare solo righe che citano `mount-manager.service`.
- **Unit tests:** `TestExampleConfigLoads` (G-U3.1). G-SYSTEMD: verify senza errori riferiti al file e grep direttive vietate vuoto.
- **e2e tests:** N/A — installazione reale esclusa.
- **Done:** file creati; G-U3.1 e G-SYSTEMD pass; unit chiusa; commit.

### 3.2 Entrypoint root e scenari e2e E3–E6
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus dopo diff (oracoli, sicurezza script, I-6) e a P3.
- **Files:** READ `test/e2e/harness_test.go`; WRITE `test/e2e/harness_test.go` (solo helper aggiuntivi); NEW `e2e-root.sh` (root del repo, mode 0755),
  `test/e2e/recovery_test.go` (`//go:build e2e`).
- **Change:**
  Preconditions: 2.4 DONE; 3.1 non richiesto.
  `e2e-root.sh` esatto:
  ```bash
  #!/usr/bin/env bash
  # Runs the privileged e2e scenarios. Invoke as: sudo -n /mnt/fabio/dati/Git/SperimentazioniAI/mount-manager/e2e-root.sh [test flags]
  set -euo pipefail
  cd "$(dirname "$(readlink -f "$0")")"
  if [[ ${EUID} -ne 0 ]]; then
    echo "must run as root: sudo -n $(pwd)/e2e-root.sh" >&2
    exit 2
  fi
  if [[ ! -x bin/mount-manager || ! -x bin/e2e.test ]]; then
    echo "build first: go build -o bin/mount-manager ./cmd/mount-manager && go test -c -tags e2e -o bin/e2e.test ./test/e2e" >&2
    exit 2
  fi
  exec env MM_BIN="$(pwd)/bin/mount-manager" ./bin/e2e.test -test.v -test.count=1 -test.timeout=45m "$@"
  ```
  Helper aggiuntivi in harness: `requireRoot(t)` → `os.Geteuid() != 0` ⇒ `t.Fatal("root required: run via e2e-root.sh")` (Fatal, non Skip: zero-discovery non è verde);
  `containerID(e, name) string` (`docker inspect -f {{.Id}} <name>`); `containerState(e, name) string` (`{{.State.Status}}`);
  `statErr(path) error` (os.Stat in goroutine, timeout 5s → errore "stat timeout"); `mountPresent(e) bool` (mountinfo top a mountPath).
  Scenari (`recovery_test.go`, ognuno inizia con `requireRoot(t)` e `newEnv(t)` + config standard 2.4):
  - `TestE2E_KillRcloneRecovers` (E3): up `--wait`; startManager; waitEvent stack_up (60s); id0 := containerID(superdufs);
    `docker kill rclone-mount`; waitEvent failure_detected (30s), recovery_started (10s), recovery_succeeded (180s);
    `hostMountHealthy` true; `docker compose exec -T superdufs ls /data/canary.txt` ok; containerID(superdufs) != id0;
    `GET http://127.0.0.1:5000/` status 401.
  - `TestE2E_PausedRcloneRecovers` (E4): up; startManager; waitEvent stack_up; `docker pause rclone-mount`;
    ev := waitEvent failure_detected (40s); assert `strings.Contains(ev.Detail, "probe timeout")` (Summary include tutti i failure: con container
    in pausa il probe deve andare in timeout senza bloccare il loop); waitEvent recovery_started (10s) e recovery_succeeded (240s);
    hostMountHealthy; containerState(rclone-mount) == "running"; `runCheck(e) == 0` [I-3, I-7].
  - `TestE2E_StaleMountCleanedWhenIdle` (E5): up `--wait`; (manager NON avviato) `docker kill rclone-mount`; precondizione:
    `errors.Is(statErr(mountPath), syscall.ENOTCONN)` altrimenti Fatal "precondition: expected stale mount (R10)"; `compose down -t 5`;
    precondizione `mountPresent` true; startManager; waitEvent stale_mount_cleaned (20s); `mountPresent` false; `time.Sleep(5s)`;
    assertNoEvent recovery_started (da 0); `docker compose ps -a -q` vuoto [D14, I-1].
  - `TestE2E_PartialStackAfterBootRecovers` (E6): up `--wait`; `compose stop rclone-mount` (timeout 60s); precondizione containerState(superdufs)=="running";
    startManager; waitEvent waiting; waitEvent failure_detected (30s: grace 5s + intervalli), recovery_succeeded (180s); hostMountHealthy; runCheck == 0.
  Steps: S1 — script + chmod 0755; S2 — helper + scenari; S3 — `go vet -tags e2e ./test/e2e/`; S4 — esegui G-E2E-ROOT.
  Recovery boundary: prima di S4 (run lungo, root, container attivi): checkpoint §6 con le risorse e la pulizia manuale in caso di
  interruzione: `docker compose -f <work>/compose.yml --project-directory <work> down -t 5`, poi `fusermount3 -u -z <work>/mymount`
  (i mount rclone dello stack hanno user_id=1000, quindi `fusermount3` funziona anche senza root, R10). Mount residuo non rimovibile → blocker all'utente.
  Failure handling: precondizione E5 fallita → escalation agent-1:opus (R10 contraddetta); `sudo -n` chiede password → BLOCKED con richiesta all'utente
  (la regola sudoers è dell'utente, non modificarla); recovery che non converge → escalation con manager.log, niente aumento timeout senza approvazione.
- **Unit tests:** N/A — codice di test; G-FMT-E2E.
- **e2e tests:** G-E2E-ROOT: output contiene `--- PASS:` per `TestE2E_WaitThenMonitor`, `TestE2E_ManualDownNoRecovery`,
  `TestE2E_KillRcloneRecovers`, `TestE2E_PausedRcloneRecovers`, `TestE2E_StaleMountCleanedWhenIdle`, `TestE2E_PartialStackAfterBootRecovers` (6) e `PASS` finale.
- **Done:** 6 scenari PASS come root; nessun residuo (container, mount a `.work`, work dir); file creati da root in `.work` rimossi; `bin/` resta gitignored;
  review agent-1:opus registrata; unit chiusa; commit (`e2e-root.sh` incluso, eseguibile).

### 3.3 README completo (italiano)
- **Model:** agent-2:sonnet
- **Assignment:** implementa; review agent-1:opus a P3 (accuratezza rispetto al codice).
- **Files:** READ tutto `internal/`, `cmd/`, `deploy/`, `compose.yml`, `e2e-root.sh`; WRITE `README.md`.
- **Change:**
  Preconditions: 3.1, 3.2 DONE.
  Contract — sezioni (in quest'ordine, italiano, esempi di comandi in blocchi code):
  1. Scopo; 2. Come funziona (stati waiting/monitoring/verifying/backoff, cosa significa "stack idle", soglie, grace, sequenza recovery 1–5 di D6);
  3. Bootstrap compose (healthcheck + depends_on e perché); 4. Configurazione (tabella di tutte le chiavi: tipo, default, significato — deve
  coincidere con 0.2); 5. Comandi (`run`, `check` con exit code 0/1/2/3, `probe` interno, `version`); 6. Eventi webhook (elenco eventi D12 e payload JSON di esempio);
  7. Installazione come servizio systemd (passi manuali: build, `install` binario, config in `/etc/mount-manager/`, unit, `systemctl daemon-reload`,
  `enable --now`, `journalctl -u mount-manager -f`; avviso I-4 sulle direttive vietate); 8. Test (unit, e2e non-root, e2e root via `e2e-root.sh`,
  garanzia remote solo locali I-6, precondizioni); 9. Limiti noti (da overview §Risks: immagini locali richieste per `--pull never`,
  down manuale durante recovery viene ripristinato, perdita upload pendenti cache VFS, superdufs `restart: always` al boot gestito dalla grace,
  regola sudoers: mantenere la root del repo non scrivibile da altri).
  Steps: S1 — scrittura; S2 — verifica incrociata: ogni chiave config/evento/exit code citato esiste nel codice (grep).
  Recovery boundary: none.
  Failure handling: discrepanza codice/README → correggere il README, non il codice; discrepanza col piano → escalation.
- **Unit tests:** N/A — documentazione.
- **e2e tests:** N/A.
- **Done:** README con 9 sezioni; verifica incrociata registrata in STATE.md §7; unit chiusa; commit.

## Phase gates and closure
- Required gates: G-FMT, G-FMT-E2E, G-UNIT, G-SYSTEMD, G-E2E-NR, G-E2E-ROOT (= G-FINAL).
- README obligation: 3.3 (completo); a P3 il supervisor verifica accuratezza contro il codice finale.
- Every sub-phase DONE or explicitly justified SKIPPED. Run full phase gates now.
- Supervisor agent-1:opus reviews integration, tutte le invarianti I-1…I-9, scenario di riferimento 1–7, documentazione.
- Open closure unit P3, record review e risultato, mark phase DONE, `Scope result: COMPLETE` solo se G-FINAL pass. Con full-autonomous:true commit
  `phase(3): complete deployment files, root e2e and documentation`. Mai push.
- On final phase: scenario di riferimento 1–7 ↔ E1–E6 (1–2 E1, 3 E2, 4 E3, 5 E4, 6 E5, 7 E6) tutti PASS in G-E2E-ROOT; G-E2E-NR PASS come utente.
