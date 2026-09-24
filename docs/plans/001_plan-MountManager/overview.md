# MountManager — Plan overview

Redatto 2026-09-24 da agent-1:opus (Claude Opus 5.5). Cartella: `docs/plans/001_plan-MountManager/`.
L'esecuzione parte da [STATE.md](STATE.md): readiness e progresso live sono solo lì.
Lingua: piano, README e documentazione in italiano; codice, commenti, messaggi di log e nomi in inglese.

## Goal and reference scenario

Obiettivo: binario Go `mount-manager` che gira come servizio systemd root sull'host e supervisiona lo stack
`compose.yml` (servizi `rclone-mount` + `superdufs`): attende il mount FUSE rclone, lo monitora ogni
`check_interval`, e in caso di guasto esegue `umount -l` (MNT_DETACH) → `docker compose down` →
`docker compose up -d --pull never --wait`, verifica, e torna in monitoraggio. Il compose viene
modificato perché `superdufs` parta solo quando il mount è sano (healthcheck + `depends_on`).

Scenario di riferimento (coperto dagli e2e E1–E6, `test/e2e/`):
1. Stack spento, manager avviato → evento `startup`, `waiting`; nessuna azione (E1).
2. `docker compose up` manuale → superdufs parte dopo `rclone-mount` healthy; manager emette `stack_up` (E1).
3. `docker compose down` manuale → `stack_stopped`, nessuna recovery (E2).
4. `docker kill rclone-mount` → mount stale ENOTCONN → `failure_detected`, `recovery_started`,
   `recovery_succeeded`; mount sano, canary leggibile da host e superdufs, superdufs ricreato (E3).
5. `docker pause rclone-mount` (FUSE appeso) → probe in timeout, loop del manager non bloccato → recovery ok (E4).
6. Stack spento con mount stale residuo → manager lo pulisce (`stale_mount_cleaned`) senza avviare lo stack (E5).
7. Stato "post-boot" (superdufs running per `restart: always`, rclone fermo) → dopo `startup_grace` recovery ok (E6).

Esclusioni: installazione reale su `/usr/local/bin`, `/etc`, `systemctl enable` (solo file + verifica
statica); push git; modifiche ai remote reali in `./config/rclone.conf`; supporto multi-stack; hot reload
config; retry/persistenza webhook; UI.

## Decisions

| ID | Decision and consequence | Authority/source | Supersedes |
|----|--------------------------|------------------|------------|
| D1 | Go (toolchain locale 1.26.1), module `mount-manager`, un solo binario `cmd/mount-manager`; unica dipendenza esterna `go.yaml.in/yaml/v3` (R9). Logging `log/slog` TextHandler su stderr (journald). | utente (Go/Rust) + supervisor, 2026-09-24 | — |
| D2 | Docker solo via CLI: `docker compose -f <compose_file> --project-directory <project_directory> …` in subprocess con timeout. Nessun SDK. | supervisor; R1 | — |
| D3 | Suite di controlli: C1 mountinfo (fstype atteso sul path), C2 probe subprocess (stat+readdir+canary), C3 container di tutti i servizi `running` e health `""`/`healthy`, C4 HTTP opzionale, C5 `docker compose exec -T <svc> ls` opzionale; canary opzionale. | utente Q2.1 + Q3.2 | — |
| D4 | Stack "idle" = zero container del progetto in stato `running`/`restarting`/`paused` (include `down` e `stop` manuali). Idle → stato Waiting, mai recovery. | utente Q1.1; estensione `stop` supervisor | — |
| D5 | In Monitoring qualunque controllo fallito per `failure_threshold` cicli consecutivi → recovery completa. In Waiting, stack non-idle ma non sano per ≥ `startup_grace` → recovery (copre boot: superdufs `restart: always`, rclone `restart: no`). | utente Q3.1; grace supervisor | — |
| D6 | Sequenza recovery: (1) cleanup mount host [abort FUSE solo se il guasto riguarda mount/probe] + MNT_DETACH; (2) `down -t down_timeout`, se fallisce `kill` + `down -t 0`; (3) cleanup mount con abort; (4) `up -d --pull never --wait --wait-timeout`; (5) verifica tramite suite completa entro `verify_timeout`. | spec b→a→c + supervisor; R3 R4 R8 | — |
| D7 | `--pull never` in ogni `up` del manager; `pull_policy: always` del compose invariato. | utente Q1.2 | — |
| D8 | Retry infinito, backoff `min(backoff_initial·2^(n-1), backoff_max)`. | utente Q1.3 | — |
| D9 | Nessuna syscall bloccante sul mount nel processo manager: accesso al filesystem solo in subprocess `mount-manager probe` con timeout, kill e guardia "stuck" (max 3 processi abbandonati). | supervisor; R3 | — |
| D10 | Unmount = `syscall.Unmount(path, syscall.MNT_DETACH)` (equivalente `umount -l`); abort = scrivere `1` in `/sys/fs/fuse/connections/<minor>/abort`. | spec + supervisor; R3 R4 | — |
| D11 | compose: healthcheck su `rclone-mount` (`grep -q ' /mnt/mount fuse.rclone ' /proc/mounts`), `superdufs.depends_on.rclone-mount: {condition: service_healthy, restart: true}`. Niente `:rslave` (rifiutato: la recovery ricrea sempre superdufs; propagazione extra cambierebbe semantica). | spec + supervisor; R1 R2 | — |
| D12 | Notifiche: webhook JSON asincrono `{event,timestamp,host,mount_path,detail,attempt}`, header opzionali, coda 64, nessun retry, errori solo loggati. | utente Q2.2 + Q3.3 | — |
| D13 | Deploy: unit systemd root senza direttive di namespacing filesystem (I-4). File forniti, installazione manuale documentata. | utente Q1.4; R5 | — |
| D14 | In Waiting con stack idle e mount stale (in mountinfo ma probe fallisce) → cleanup (abort+detach), evento `stale_mount_cleaned`; nessun avvio stack. Motivo: un bind di sorgente stale fa fallire il successivo `up` manuale (R10). | supervisor; R10 | — |
| D15 | Test: unit con fake; e2e reali con remote `alias` → directory locale temporanea nel work dir (I-6). Scenari non-root (E1, E2) autonomi con gruppo docker; scenari root (E3–E6) via `sudo -n <repo>/e2e-root.sh` (regola sudoers utente, R6). | utente Q2.3 + vincolo 2026-09-24; R6 | — |
| D16 | CLI: `run`, `check`, `probe`, `version`; exit code `check`: 0 ok, 1 guasto, 2 idle, 3 errore config/uso. Default config `/etc/mount-manager/config.yml`. | supervisor | — |
| D17 | HTTP check default `accept_status: [200, 401, 403]` (superdufs anonimo risponde 401, R7). | supervisor; R7 | — |

## Open questions

Nessuna. Tutte le decisioni prodotto sono state risolte dall'utente il 2026-09-24 (due giri di domande +
vincolo remote locali + abilitazione root). Readiness in STATE.md.

## Architecture

```
cmd/mount-manager (run|check|probe|version)
  └─ config.Load ─► supervisor.Supervisor.Run(ctx)
        Step(): health.Checker.Evaluate ─┬─ mountinfo.Read (/proc/self/mountinfo, nessun accesso FUSE)
                                         ├─ probe.Runner (subprocess "mount-manager probe", timeout)
                                         ├─ compose.Client.Services/PS/Exec (docker CLI)
                                         └─ net/http GET (opzionale)
               recovery.Recoverer.Recover ─┬─ mountinfo + unmount.AbortConnection/Detach
                                           └─ compose.Client.Down/Kill/Up
               notify.Webhook.Notify (goroutine sender, coda)
```

Stati supervisore: `Waiting` → `Monitoring` → (`Verifying` ⇄ `Backoff`) → `Monitoring`. Un solo goroutine
esegue lo state machine (nessun lock condiviso); `Step` è deterministico dato `Clock`, report e fake,
e ritorna il ritardo al prossimo step. Dettaglio completo in phase_03.md §Local design context.

## Interfaces and compatibility

| Surface | Exact name/type | Default | Errors/conflicts | Compatibility |
|---------|-----------------|---------|------------------|---------------|
| CLI | `mount-manager run [--config P]` | `/etc/mount-manager/config.yml` | config invalida/docker assente → exit 3 | nuovo |
| CLI | `mount-manager check [--config P]` | idem | exit 0/1/2/3 (D16) | nuovo |
| CLI | `mount-manager probe --path P [--canary C]` (interno) | — | stampa JSON `probe.Result`, exit 0; flag errati exit 3 | interno |
| CLI | `mount-manager version` | stampa `mount-manager <version>` | — | `-ldflags -X main.version=` |
| Config | YAML strict (`KnownFields`), schema in phase_01.md 0.2 | vedi tabella default | errori aggregati con `errors.Join`, prefisso `config: <campo>:` | nuovo |
| Webhook | POST JSON `notify.Event` | disabilitato se `webhook.url` vuoto | non-2xx/timeout → log warn | nuovo |
| compose.yml | + healthcheck `rclone-mount`, + `depends_on` superdufs | — | `docker compose config -q` deve passare | resto invariato (I-9) |
| systemd | `deploy/mount-manager.service` | ExecStart `/usr/local/bin/mount-manager run` | — | nuovo |

## Phase map

| Logical phase | File | Sub-phase IDs | Depends on | Assignment |
|---------------|------|---------------|------------|------------|
| 0 Fondamenta (repo, compose, config, primitive host) | [phase_01.md](phase_01.md) | 0.1, 0.2, 0.3, 0.4 | none | agent-2:sonnet |
| 1 Docker, webhook, health, `check` | [phase_02.md](phase_02.md) | 1.1, 1.2, 1.3 | P0 | agent-2:sonnet |
| 2 Recovery, supervisore, `run`, e2e non-root | [phase_03.md](phase_03.md) | 2.1, 2.2, 2.3, 2.4 | P1 | agent-2:sonnet |
| 3 Deploy, e2e root, README | [phase_04.md](phase_04.md) | 3.1, 3.2, 3.3 | P2 | agent-2:sonnet |

No live status in this table.

## Reuse map

| Need | Path and symbol | Contract to preserve |
|------|-----------------|----------------------|
| Stack da supervisionare | `compose.yml` — servizi `rclone-mount`, `superdufs` | tutte le chiavi esistenti invariate (I-9); container_name fissi `rclone-mount`, `superdufs`; project name `rclone-stack-superdufs` |
| Mount host | `./mymount` (bind `:rshared` in rclone, `/data` in superdufs) | path host = `mount_path` in config |
| Nessun codice Go preesistente | — | repository greenfield |

## Research and evidence

Ricerca eseguita 2026-09-24 su fonti primarie e probe locali isolati (container `pevprobe-*` e directory
scratch, poi rimossi). Effetto collaterale dichiarato: pull locale delle immagini `rclone/rclone:latest` e
`fabiop85/superdufs:v1.5.0`.

| ID | Question / required or optional | Evidence state | Affected decisions / units / tests |
|----|---------------------------------|----------------|------------------------------------|
| R1 | Flag e output Compose v5.5.1 (`up --pull/--wait/--wait-timeout`, `down -t`, `ps -a --format json`, `depends_on` healthy) — required | CONFIRMED | D2 D6 D7 D11; 0.1 1.1; TestParsePS*, TestUpArgs |
| R2 | Immagine rclone: shell/tool, fstype, remote alias locale — required | CONFIRMED | D11 D15; 0.1 2.4 3.2 |
| R3 | FUSE abort via fusectl, numerazione connessioni, comportamento richieste — required | CONFIRMED (+inferenza etichettata) | D6 D9 D10; 0.3 2.1; E4 |
| R4 | `umount2(MNT_DETACH)` semantica, errori, propagazione shared — required | CONFIRMED | D10 I-2; 0.3 2.1 |
| R5 | systemd: direttive che isolano mount namespace — required | CONFIRMED | D13 I-4; 3.1 |
| R6 | sudoers wildcard e regola utente — required | CONFIRMED | D15; 3.2 |
| R7 | Status HTTP superdufs anonimo — required | CONFIRMED | D17; 1.3 |
| R8 | `docker stop` su container in pausa — required | CONFIRMED | D6; E4 |
| R9 | Libreria YAML mantenuta — required | CONFIRMED | D1; 0.2 |
| R10 | Mount stale dopo SIGKILL rclone e effetto su bind successivo — required | CONFIRMED | D14; E3 E5 |

### R1 — Compose CLI v5.5.1
- Sources: `docker compose up|down|ps --help` locale; probe progetto `pevprobe` (healthcheck + `depends_on: service_healthy`); https://docs.docker.com/reference/compose-file/services/ (§depends_on long syntax, §healthcheck).
- Applicability: Docker Engine 29.8.1, Compose v5.5.1, host Ubuntu kernel 7.0.0.
- Dates: accesso 2026-09-24.
- Documented fact: `up` ha `--pull string`, `--wait`, `--wait-timeout int` (secondi), `-t`; `down -t int`; `depends_on.condition: service_healthy` attende health; `restart: true` riavvia il dipendente solo su operazioni Compose esplicite; `healthcheck.start_interval` supportato.
- Local check: `up -d --pull never --wait --wait-timeout 60` → exit 0 dopo "Healthy"; `ps -a --format json` → **NDJSON, un oggetto per riga** con campi `Name`, `Service`, `State`, `Health`, `ExitCode`, `Project`; progetto assente → output vuoto.
- Decision impact: D2/D6/D7; parser accetta NDJSON, array JSON e vuoto (robustezza tra versioni).
- Unresolved/replacement: none.

### R2 — Immagine rclone
- Sources: probe `docker run rclone/rclone:latest` (v1.75.1, Alpine) con `/dev/fuse`, `SYS_ADMIN`, remote `alias`.
- Documented fact / Local check: presenti `/bin/sh`, `grep`, `mountpoint`, `stat`, `ls`; `/proc/mounts` nel container mostra `… /mnt/mount fuse.rclone …`; healthcheck `grep -q ' /mnt/mount fuse.rclone ' /proc/mounts` → exit 0; remote `[remote] type = alias, remote = /config/e2e-remote` espone i file locali; `--allow-non-empty` necessario quando `/mnt/mount` è già un bind mount; utente 1000 richiede `/etc/passwd` montato (già nel compose).
- Decision impact: D11 healthcheck; D15 fixture e2e.
- Unresolved/replacement: none.

### R3 — FUSE control filesystem
- Sources: https://raw.githubusercontent.com/torvalds/linux/master/Documentation/filesystems/fuse/fuse.rst (§Control filesystem, §Aborting a filesystem connection, §Interrupting filesystem operations); probe locale.
- Applicability: kernel 7.0.0; fusectl montato su `/sys/fs/fuse/connections`.
- Dates: accesso 2026-09-24 (master).
- Documented fact: ogni connessione ha una directory "named by a unique number"; scrivere nel file `abort` "will abort the filesystem connection… all waiting requests will be aborted an error returned for all aborted and new requests"; una richiesta non ancora inviata con segnale fatale (SIGKILL) viene rimossa e ritorna subito.
- Supervisor inference: il numero coincide col minor del device del mount. Local check: `/proc/self/mountinfo` `0:58`, `0:61`, `0:64` ↔ directory `58 61 64` → confermato su questo host; il codice ignora ENOENT (nessuna garanzia universale necessaria).
- Decision impact: D9 (subprocess killabile), D10 (abort via minor da mountinfo campo 3).
- Unresolved/replacement: none.

### R4 — umount2(MNT_DETACH)
- Sources: https://man7.org/linux/man-pages/man2/umount2.2.html (DESCRIPTION, ERRORS, NOTES "umount() and shared mounts").
- Documented fact: MNT_DETACH = lazy unmount; richiede CAP_SYS_ADMIN (EPERM altrimenti); `EINVAL` se target non è mount point; `ENOENT` se path inesistente; unmount su mount shared si propaga ai peer. `go doc syscall` include `MNT_DETACH`.
- Supervisor inference: il detach sull'host propaga al peer `/mnt/mount` del container rclone (voluto). Per non smontare mount estranei si agisce solo se il top mount al path ha `expected_fstype` (I-2).
- Decision impact: D10, I-2; EINVAL/ENOENT → successo idempotente.
- Unresolved/replacement: none.

### R5 — systemd namespacing
- Sources: https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html (§Sandboxing: PrivateMounts=, MountFlags=, elenco "Other file system namespace unit settings").
- Applicability: doc systemd 262; host systemd 255 (opzioni presenti da ≥239).
- Documented fact: `PrivateTmp=`, `PrivateDevices=`, `ProtectSystem=`, `ProtectHome=`, `ReadOnlyPaths=`, `InaccessiblePaths=`, `ReadWritePaths=`, `BindPaths=`, `BindReadOnlyPaths=` … abilitano un mount namespace equivalente a `PrivateMounts=`, con propagazione verso l'host disattivata.
- Decision impact: I-4; unit senza tali direttive (anche `TemporaryFileSystem=`, `MountFlags=`), verificato da grep nel gate G-SYSTEMD.
- Unresolved/replacement: none.

### R6 — sudoers
- Sources: `man sudoers` locale (sudo 1.9.15p5) "A forward slash ('/') will not be matched by wildcards used in the file name portion of the command"; `sudo -n -l`.
- Local check: regola utente `(root) NOPASSWD: /mnt/fabio/dati/Git/SperimentazioniAI/mount-manager/*`; `sudo -n true` richiede password.
- Decision impact: entrypoint root = `e2e-root.sh` direttamente nella root del repo, invocato con path assoluto; binari compilati prima come utente (PATH di root può non contenere `/usr/local/go/bin`).
- Unresolved/replacement: none. Rischio sicurezza in §Risks.

### R7 — superdufs HTTP
- Local check: `fabiop85/superdufs:v1.5.0` con gli argomenti del compose: `GET /` anonimo → 401, con `admin:pass` → 200. Immagine Alpine con `/bin/sh`, `ls`, `wget`; nessun HEALTHCHECK nell'immagine.
- Decision impact: D17; C5 usa `ls`.

### R8 — docker stop su container in pausa
- Local check: `docker pause` poi `docker stop -t 3` → exit 0 in ~3 s, stato `exited` (Engine 29.8.1).
- Decision impact: D6 (down funziona anche con rclone in pausa; fallback kill resta).

### R9 — YAML
- Local check: `go list -m -versions go.yaml.in/yaml/v3` → fino a v3.0.5; `gopkg.in/yaml.v3` fermo a v3.0.1 (archiviato).
- Decision impact: D1 usa `go.yaml.in/yaml/v3@latest` (API identica a yaml.v3: `yaml.NewDecoder`, `KnownFields`, `yaml.Node`); durate tramite tipo `config.Duration` custom (nessuna dipendenza dal decoding durate della libreria).

### R10 — Mount stale
- Local check (scratch, container `--user 1000:1000`, bind `:rshared`, remote alias): host mountinfo `0:110 fuse.rclone /config/e2e-remote`; dopo `docker kill` il mount resta e `stat` fallisce con ENOTCONN ("Transport endpoint is not connected"); resta anche dopo `docker rm`; `docker run -v <stale>:/data` fallisce; `fusermount3 -u -z` come uid 1000 lo rimuove.
- Decision impact: D14; precondizioni E3/E5; teardown e2e non-root usa `fusermount3 -u -z` come fallback.

## Invariants

| ID | Meaning | Guarding test |
|----|---------|---------------|
| I-1 | Con stack idle (D4) in Waiting/Monitoring non si esegue mai down/up. | TestWaitingIdleStaysWaiting, TestMonitoringIdleGoesWaiting, E2, E5 |
| I-2 | Mai abort/detach se il top mount a `mount_path` non ha fstype = `expected_fstype`. | TestCleanupRefusesForeignFSType |
| I-3 | Il processo manager non esegue syscall filesystem su `mount_path` (solo `/proc/self/mountinfo` e subprocess probe). | TestRunnerTimeout, review 0.4/1.3, E4 |
| I-4 | Il servizio gira nel mount namespace dell'host: nessuna direttiva di namespacing nella unit. | G-SYSTEMD |
| I-5 | Ogni `up` del manager usa `--pull never`. | TestUpArgs |
| I-6 | Test mai su remote reali: rclone.conf generato nel work dir e2e con solo `alias` locale; `./config` del repo mai letto né montato. | assertRemoteIsLocal nel harness, review 2.4/3.2 |
| I-7 | Il loop non si blocca: ogni comando esterno ha timeout; probe abbandonabile; webhook asincrono. | TestExecRunnerTimeout, TestRunnerTimeout, TestWebhookTimeoutDoesNotBlockNotify, E4 |
| I-8 | Ordine recovery: detach prima di down; down (o fallback) prima di up; cleanup post-down prima di up. | TestRecoverSequence*, TestRecoverDownFailsFallsBackToKill |
| I-9 | `compose.yml`: tutte le chiavi esistenti invariate; solo aggiunte healthcheck/depends_on. | TestComposeUnchangedFields |

## Risks

| Risk | Design mitigation | Verification |
|------|-------------------|--------------|
| FUSE appeso blocca processi (manager, down) | probe subprocess + abort fusectl prima del detach se guasto mount/probe; fallback kill | TestRunnerTimeout, E4 |
| Recovery smonta mount estraneo | I-2 check fstype prima di ogni abort/detach | TestCleanupRefusesForeignFSType |
| `up --pull never` senza immagini locali | documentato; retry infinito con log | README §Limiti |
| Down manuale durante Backoff/Verifying viene ripristinato | documentato: per fermare lo stack fermare prima il servizio | README §Limiti |
| Upload pendenti nella cache VFS (`/tmp/rclone` nel container) persi al down | intrinseco a down/up richiesto; documentato | README §Limiti |
| Regola sudoers: qualunque file nella root del repo eseguibile come root | nessun file root oltre `e2e-root.sh`; README avvisa di mantenere la root del repo non scrivibile da altri | review 3.2 |
| E2E collide con stack di produzione (container_name fissi, porta 5000) | preflight fallisce se esistono container `rclone-mount`/`superdufs` o porta 5000 occupata | preflight harness |
| Probe/recovery lanciati senza root | warning all'avvio; errori EPERM loggati; e2e root separati | TestClassifyDetachError, E3–E6 |

## Verification strategy

Registro comandi: STATE.md §3. Unit test per ogni sotto-fase (gate G-U*), full `G-FMT` + `G-UNIT` a ogni P<N>;
P2 aggiunge `G-E2E-NR`; P3 (finale) esegue `G-FMT`, `G-UNIT`, `G-SYSTEMD`, `G-E2E-NR`, `G-E2E-ROOT`
(6 scenari = scenario di riferimento). Un gate a zero test scoperti non è pass.

## Model-assignment summary

| Unit(s) | Implementer | Required supervisor review |
|---------|-------------|----------------------------|
| 0.1, 0.2, 0.3, 1.1, 1.2, 2.3, 3.1, 3.3 | agent-2:sonnet | agent-1:opus a P<N> |
| 0.4, 1.3 | agent-2:sonnet | agent-1:opus dopo diff (lifecycle probe / I-3) + P<N> |
| 2.1, 2.2 | agent-2:sonnet | agent-1:opus prima (conferma contratto) e dopo diff (state machine, I-1 I-2 I-8) + P2 |
| 2.4, 3.2 | agent-2:sonnet | agent-1:opus dopo diff (I-6, oracoli e2e) + P<N> |
| recon puntuale | agent-3:haiku | — |

Roster: agent-1:opus (Claude Opus 5.5, supervisor/coordinator), agent-2:sonnet (Claude Sonnet 5, implementer),
agent-3:haiku (Claude Haiku 4.5, recon read-only).
