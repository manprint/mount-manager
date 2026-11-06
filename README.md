# mount-manager

## 1. Scopo

`mount-manager` supervisiona lo stack Docker Compose composto dal mount rclone (`rclone-mount`) e da
`superdufs`, che espone i file del mount via web. L'applicativo attende che il mount configurato
(`mount_path`) diventi disponibile, poi lo controlla periodicamente. Se il mount smette di rispondere,
cade o presenta un problema di trasporto, l'applicativo smonta la cartella, riavvia il compose
(down/up) e verifica che mount e stack tornino disponibili prima di rimettersi in attesa.

## 2. Come funziona

### Stati

Il supervisore (`internal/supervisor`) è una macchina a stati con un solo goroutine:

- **waiting** — stato iniziale. Attende che lo stack diventi sano. Se lo stack è idle (vedi sotto)
  resta in attesa senza mai avviare recovery [I-1]. Se lo stack è avviato ma non sano da almeno
  `startup_grace`, avvia la recovery (copre il boot: `superdufs` ha `restart: always`, `rclone-mount`
  `restart: no`, quindi dopo un riavvio host lo stack può risultare parzialmente su senza essere sano).
  La grace inizia a contare solo quando nessun container in esecuzione è più in `health=starting`.
- **monitoring** — lo stack è sano; i controlli vengono ripetuti ogni `check_interval`. Se lo stack
  torna idle si torna in waiting (evento `stack_stopped`); se un controllo fallisce per
  `failure_threshold` cicli consecutivi si avvia la recovery. Un ciclo fallito mentre un container in
  esecuzione è in `health=starting` (es. riavviato a mano) non viene contato.
- **verifying** — dopo un tentativo di recovery, si attende fino a `recovery.verify_timeout` che la
  suite di controlli torni tutta positiva; in caso di successo si torna a monitoring, altrimenti si
  entra in backoff. Se lo stack torna idle (fermato a mano dopo l'`up` della recovery) si torna in
  waiting (evento `stack_stopped`) senza altri tentativi [I-1].
- **backoff** — attesa prima del prossimo tentativo di recovery, con durata calcolata dalla formula
  sotto. Scaduta l'attesa la suite di controlli viene rieseguita: se è tutta positiva si torna in
  monitoring (evento `recovery_succeeded`), se docker non risponde si resta in attesa, altrimenti si
  ritenta la recovery.

### Stack "idle"

Lo stack è considerato **idle** quando zero container del progetto Compose sono in stato `running`,
`restarting` o `paused` — questo include sia lo stack mai avviato sia un `docker compose down`/`stop`
manuale. Con stack idle il supervisore non esegue mai `down`/`up` (invariante I-1); in waiting o
monitoring lo stato idle riporta (o mantiene) il supervisore in waiting.

### Docker non raggiungibile

Se `docker compose config`/`ps` falliscono (daemon fermo o in riavvio, CLI in timeout, `compose.yml`
non valido), lo stato dello stack — idle o no — è sconosciuto e `down`/`up` non potrebbero comunque
funzionare. In questo caso il supervisore **resta fermo**: in waiting azzera la `startup_grace`, in
monitoring non conta il ciclo verso `failure_threshold`, in backoff non consuma tentativi; non avvia
mai una recovery e non smonta il mount. L'inizio e la fine dell'indisponibilità sono loggati una sola
volta (`docker unavailable, holding` / `docker available again`). Così, ad esempio, un
`systemctl stop docker` di manutenzione con lo stack fermo apposta non porta a riavviare lo stack.

### Shutdown

Un `SIGINT`/`SIGTERM`/`SIGHUP` ricevuto mentre è in corso un controllo non produce azioni: il report di quel
ciclo (probe e richiesta HTTP annullati) viene scartato, quindi nessun conteggio di fallimento, nessun
cleanup del mount e nessuna recovery. Una recovery interrotta dal segnale non emette `recovery_failed`
e non tenta il fallback `kill` dopo un `down` interrotto: è l'evento `shutdown` a riportarla (vedi §6). `SIGHUP` termina come `SIGTERM`, salvo che sia ignorato
all'avvio (es. `nohup`): in quel caso resta ignorato.

### Soglie e grace

- `failure_threshold` (default 3): numero di cicli consecutivi con controllo fallito, in monitoring,
  prima di avviare la recovery.
- `startup_grace` (default 120s): tempo massimo, in waiting, che uno stack avviato ma non sano può
  restare tale prima che scatti la recovery. Conta solo dopo la fase di avvio di Docker: finché un
  container in esecuzione è in `health=starting` (limitata da `start_period`, `interval` e `retries`
  dell'healthcheck) la grace non parte, così la recovery (`compose down`) non può partire mentre è in
  corso un `docker compose up --wait` manuale. Il limite vale per ogni avvio: un servizio con
  healthcheck e `restart: always` in crash loop rimetterebbe la grace a zero a ogni riavvio e la
  recovery non partirebbe (con il `compose.yml` attuale non succede: `rclone-mount` ha `restart: no`,
  `superdufs` non ha healthcheck).

### Sequenza di recovery (D6)

Ad ogni tentativo di recovery (`internal/recovery.Recoverer.Recover`), in ordine (I-8):

1. **Cleanup pre-down** del mount host: se il guasto rilevato riguarda mount o probe, prima
   dell'unmount si scrive `abort` sulla connessione FUSE (fusectl, directory dal device del mount:
   `major<<20 | minor`); in ogni caso si esegue il detach lazy (`MNT_DETACH`).
2. **`docker compose down -t <recovery.down_timeout>`**; se fallisce, fallback a `kill` seguito da
   `down -t 0`.
3. **Cleanup post-down** del mount host, sempre con abort prima del detach.
4. **`docker compose up -d --pull never --wait --wait-timeout <recovery.up_wait_timeout>`**.
5. **Verifica**: il supervisore entra in stato verifying e attende fino a `recovery.verify_timeout`
   che l'intera suite di controlli (C1-C5) torni positiva.

Ogni comando docker gira in un proprio process group: allo scadere del timeout (o allo shutdown) riceve
`SIGTERM` (dopo 5s `SIGKILL`) e poi l'intero gruppo viene ucciso, così nessun processo compose orfano
(es. il plugin lanciato dal CLI `docker`) resta in esecuzione sovrapponendosi al tentativo successivo.

Il cleanup agisce solo sui mount con `expected_fstype` presenti a `mount_path` e non tocca mai un
mount estraneo (I-2). La cima dello stack di mount a `mount_path` è determinata dai parent ID di
`/proc/self/mountinfo` (il mount che non è padre di un altro mount allo stesso path), non dall'ordine
delle righe: se un filesystem diverso è montato *sopra* il mount FUSE rifiuta di agire; se un
filesystem diverso sta solo *sotto* (ad esempio `mount_path` è a sua volta un bind mount reso
`rshared`), una volta staccato il FUSE non c'è altro da pulire e la recovery prosegue.

### Formula di backoff (D8)

Retry infinito; la durata dell'n-esimo tentativo è:

```
backoff(n) = min(backoff_initial · 2^(n-1), backoff_max)
```

(`internal/supervisor.Backoff`, default `backoff_initial=10s`, `backoff_max=5m`).

### Pulizia mount stale quando idle (D14)

In waiting, con stack idle, se il mount è presente in `/proc/self/mountinfo` ma il probe fallisce
(mount "stale", tipicamente dopo un `docker kill` di `rclone-mount` senza fermare lo stack), il
supervisore esegue il cleanup (abort + detach) senza avviare lo stack ed emette l'evento
`stale_mount_cleaned`. Se il cleanup fallisce (es. manager non root) viene ritentato a ogni controllo, ma
l'errore è loggato solo quando cambia. Motivo: un bind di sorgente stale farebbe fallire il successivo `up` manuale.

## 3. Bootstrap compose

`compose.yml` aggiunge, rispetto allo stack di base (I-9, tutte le altre chiavi invariate):

- un `healthcheck` su `rclone-mount`:
  ```
  test: ["CMD-SHELL", "grep -q ' /mnt/mount fuse.rclone ' /proc/mounts"]
  ```
  verifica dentro al container che il mount FUSE sia effettivamente presente in `/proc/mounts`, non
  solo che il processo `rclone mount` sia vivo.
- `superdufs.depends_on.rclone-mount: {condition: service_healthy, restart: true}`: `superdufs` parte
  solo quando `rclone-mount` è sano, e viene **ricreato** ogni volta che `rclone-mount` viene
  ricreato da un'operazione Compose esplicita (come la `up` della recovery). Questo garantisce che
  `superdufs` non resti a servire un bind mount ormai stale dopo una recovery.

Non è stata aggiunta alcuna opzione di propagazione dei mount (`:rslave` e simili) al bind di
`superdufs`: la recovery ricrea sempre `superdufs`, quindi una propagazione aggiuntiva cambierebbe
semantica senza necessità.

## 4. Configurazione

File YAML, decodifica strict (campi sconosciuti rifiutati). Percorso di default:
`/etc/mount-manager/config.yml` (override con `--config`). Esempio completo con tutti i default
espliciti: [`deploy/config.example.yml`](deploy/config.example.yml).

| Chiave | Tipo | Default | Significato |
|--------|------|---------|-------------|
| `mount_path` | stringa (path assoluto) | *(obbligatorio)* | Path host del mount FUSE da supervisionare. Non deve contenere symlink (`/proc/self/mountinfo` riporta il path risolto, quindi un path con symlink non corrisponderebbe mai). |
| `compose_file` | stringa (path assoluto) | *(obbligatorio)* | Path del file `docker compose` dello stack. |
| `project_directory` | stringa (path assoluto) | dir di `compose_file` | Directory di progetto passata a `docker compose --project-directory`. |
| `docker_binary` | stringa | `docker` | Nome o path del binario docker usato per pilotare compose. |
| `expected_fstype` | stringa | `fuse.rclone` | Filesystem atteso in cima a `mount_path` quando sano. |
| `check_interval` | durata | `10s` | Intervallo tra un controllo e il successivo mentre lo stack gira. |
| `check_timeout` | durata | `10s` | Timeout del subprocess di probe per un singolo controllo. Deve coprire un listing a freddo della radice del mount: con `--dir-cache-time=30s` rclone la rilegge dal remote circa ogni 30s. Un timeout isolato non avvia la recovery (servono `failure_threshold` cicli consecutivi). |
| `failure_threshold` | intero | `3` | Cicli falliti consecutivi in monitoring prima della recovery. |
| `startup_grace` | durata | `120s` | In waiting, tempo tollerato di stack avviato ma non sano prima della recovery. |
| `log_level` | stringa | `info` | Uno tra `debug`, `info`, `warn`, `error`. |
| `recovery.down_timeout` | durata (>= 1s) | `30s` | Timeout di `docker compose down -t` durante la recovery (compose usa secondi interi). |
| `recovery.up_wait_timeout` | durata (>= 1s) | `120s` | Timeout di `docker compose up --wait-timeout` durante la recovery (compose usa secondi interi). |
| `recovery.verify_timeout` | durata | `120s` | Tempo massimo in stato verifying prima di considerare la recovery fallita. |
| `recovery.command_grace` | durata | `60s` | Tempo extra aggiunto ai timeout dei comandi `down` e `up` prima di forzarne la terminazione; è l'intero timeout del fallback `kill`. |
| `recovery.backoff_initial` | durata | `10s` | Backoff iniziale tra tentativi di recovery. |
| `recovery.backoff_max` | durata | `5m` | Backoff massimo tra tentativi di recovery. |
| `checks.canary` | stringa (path relativo) | `""` | Nome file relativo dentro il mount che deve esistere; vuoto disabilita il controllo canary. Non può uscire dal mount (`..` o prefisso `../`). |
| `checks.http.url` | stringa (URL) | `""` | URL del check HTTP opzionale (C4); vuoto lo disabilita. |
| `checks.http.timeout` | durata | `5s` | Timeout della richiesta HTTP del check. |
| `checks.http.accept_status` | lista di interi | `[200, 401, 403]` | Codici di stato HTTP considerati sani (superdufs anonimo risponde 401, D17); non può essere vuota se `checks.http.url` è impostato. |
| `checks.exec.service` | stringa | `""` | Servizio compose del check exec (C5): `ls` viene eseguito con `docker exec` nel container in esecuzione del servizio (nome preso da `docker compose ps`); vuoto lo disabilita. |
| `checks.exec.path` | stringa (path assoluto) | `/data` (se `checks.exec.service` è impostato) | Path assoluto dentro il container del servizio exec da controllare. |
| `checks.exec.timeout` | durata | `10s` | Timeout del comando `exec` del check. |
| `webhook.url` | stringa (URL) | `""` | URL che riceve le notifiche JSON; vuoto disabilita il webhook. |
| `webhook.headers` | mappa stringa→stringa | `{}` | Header HTTP aggiuntivi inviati con ogni richiesta webhook. Le chiavi devono essere nomi di header validi (token RFC 9110), i valori non possono contenere CR, LF o NUL. |
| `webhook.timeout` | durata | `5s` | Timeout della richiesta HTTP del webhook. |

Le durate sono stringhe accettate da `time.ParseDuration` (es. `10s`, `2m`, `1h`). Tutte le durate,
`failure_threshold`, `log_level`, i path e gli URL sono validati al caricamento; gli errori vengono
riportati tutti insieme, con prefisso `config: <campo>: <motivo>`.

Una chiave assente prende il default; un valore scritto esplicitamente viene sempre preso così com'è.
Quindi `check_interval: 0s` o `failure_threshold: 0` sono errori di configurazione, non un modo per
ottenere il default (per usare il default basta omettere la chiave).

Il file può contenere segreti (es. `webhook.headers` con un token): installarlo leggibile solo da root
(`0600`). All'avvio `run` logga un warning se il file di configurazione o `compose_file` sono
scrivibili da gruppo/altri (chi li scrive controlla cosa esegue il manager, di norma root) o se la
configurazione è leggibile da altri con un webhook configurato. Non vengono controllati `.env`,
`./config/rclone.conf` né la directory del progetto.

## 5. Comandi

```
mount-manager run [--config P]
mount-manager check [--config P]
mount-manager probe --path P [--canary C]
mount-manager version
```

- **`run`** — avvia la supervisione continua: attesa, monitoraggio, recovery con backoff e notifiche
  webhook. Gira finché non riceve `SIGINT`/`SIGTERM`/`SIGHUP`, poi emette l'evento `shutdown` ed esce con
  codice 0. Richiede il binario docker in `PATH` (o a `docker_binary`); errori di configurazione o
  docker mancante restituiscono exit code 3 prima di avviare la supervisione.
- **`check`** — esegue la suite di controlli una sola volta e stampa un report testuale:
  ```
  status: ok|idle|failing
  - <check>: <dettaglio>   # una riga per ogni controllo fallito
  - mount: stale           # solo se il mount è presente ma stale
  ```
  Exit code:
  | Codice | Significato |
  |--------|-------------|
  | `0` | tutti i controlli passano (`status: ok`) |
  | `1` | almeno un controllo fallisce, stack non idle (`status: failing`) |
  | `2` | stack idle (`status: idle`) |
  | `3` | errore di configurazione o di uso (config non valida, flag errati) |
- **`probe`** (comando interno) — controlla un mount point in questo stesso processo e stampa il
  risultato in JSON su stdout; usato da `run`/`check` come subprocess a vita breve (con timeout),
  in modo che un mount bloccato non blocchi mai il processo principale (I-3, I-7). Flag mancanti o
  non validi → exit 3.
- **`version`** — stampa `mount-manager <version>` ed esce con 0.

## 6. Eventi webhook

Se `webhook.url` non è vuoto, ogni evento del supervisore viene inviato in modo asincrono (coda 64,
nessun retry; errori solo loggati) come richiesta HTTP POST con corpo JSON e gli header opzionali di
`webhook.headers`. I redirect non vengono seguiti (un POST rediretto diventerebbe GET e l'evento andrebbe
perso): una risposta 3xx è loggata come consegna fallita. Gli errori loggati non riportano mai l'URL,
che spesso contiene il segreto del webhook nel path o nella query (lo stesso vale per gli errori di
validazione di `webhook.url` e `checks.http.url`).

Eventi (`internal/notify`):

| Evento | Quando |
|--------|--------|
| `startup` | all'avvio del processo `run`. |
| `waiting` | all'avvio, subito dopo `startup` (il ritorno in waiting da monitoring emette `stack_stopped`). |
| `stack_up` | quando lo stack passa da waiting a monitoring (tutti i controlli positivi). |
| `stack_stopped` | quando, in monitoring o in verifying, lo stack torna idle (si torna in waiting; in verifying `attempt` riporta il tentativo interrotto). |
| `failure_detected` | quando scatta la recovery (soglia fallimenti in monitoring o grace scaduta in waiting). |
| `recovery_started` | a ogni tentativo di recovery avviato. |
| `recovery_succeeded` | quando, in verifying (o allo scadere del backoff), la suite di controlli torna positiva. |
| `recovery_failed` | quando la recovery fallisce o il timeout di verifica scade. |
| `stale_mount_cleaned` | quando viene ripulito un mount stale mentre lo stack è idle (D14). |
| `shutdown` | alla terminazione del processo `run` (segnale ricevuto). Se arriva durante una recovery (verifying o backoff), `detail` lo segnala e `attempt` riporta il tentativo: lo stack può essere rimasto fermo e un manager riavviato non lo rialza (vedi §9). |

Esempio di payload:

```json
{
  "event": "recovery_started",
  "timestamp": "2026-09-24T10:15:32.123456789Z",
  "host": "storage01",
  "mount_path": "/opt/rclone-stack/mymount",
  "detail": "probe: probe timeout after 10s; http: context deadline exceeded",
  "attempt": 1
}
```

Il primo tentativo porta in `detail` il riepilogo dei controlli falliti; i tentativi successivi, avviati
allo scadere del backoff, hanno `detail` = `retry after backoff`. Negli errori dei comandi docker
(`recovery_failed`) `detail` riporta la parte finale dello stderr, dove compose scrive l'errore vero.

Campi: `event` (stringa, uno dei nomi sopra), `timestamp` (RFC3339 nano), `host` (hostname del
processo), `mount_path` (dalla configurazione), `detail` (testo libero, dipende dall'evento),
`attempt` (numero del tentativo di recovery corrente; `0` per gli eventi non legati a un tentativo).

## 7. Installazione come servizio systemd

L'installazione è manuale; non esiste un installer automatico.

1. Ottieni il binario, scaricandolo da una
   [release GitHub](https://github.com/manprint/mount-manager/releases) (`linux-amd64` o `linux-arm64`,
   con `SHA256SUMS`):
   ```
   curl -fsSLO https://github.com/manprint/mount-manager/releases/download/v1.1.1/mount-manager-linux-amd64
   curl -fsSLO https://github.com/manprint/mount-manager/releases/download/v1.1.1/SHA256SUMS
   sha256sum --ignore-missing -c SHA256SUMS
   mv mount-manager-linux-amd64 bin/mount-manager
   ```
   oppure compilandolo (Go 1.27; con `GOTOOLCHAIN=auto` il comando `go` scarica da solo la toolchain
   `go1.27.3` indicata in `go.mod`):
   ```
   go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always)" -o bin/mount-manager ./cmd/mount-manager
   ```
2. Installa il binario:
   ```
   sudo install -m 0755 bin/mount-manager /usr/local/bin/
   ```
3. Crea la configurazione in `/etc/mount-manager/config.yml`, leggibile solo da root, partendo da
   [`deploy/config.example.yml`](deploy/config.example.yml) e adattando i path allo host:
   ```
   sudo install -d -m 0755 /etc/mount-manager
   sudo install -m 0600 deploy/config.example.yml /etc/mount-manager/config.yml
   ```
4. Copia la unit in `/etc/systemd/system/`:
   ```
   sudo cp deploy/mount-manager.service /etc/systemd/system/
   ```
5. Ricarica systemd e abilita/avvia il servizio:
   ```
   sudo systemctl daemon-reload
   sudo systemctl enable --now mount-manager
   ```
6. Segui i log:
   ```
   journalctl -u mount-manager -f
   ```

La unit usa `Restart=always` ma `RestartPreventExitStatus=3`: un errore di configurazione (exit 3) non
viene riavviato in loop; il servizio resta `failed` finché la configurazione non viene corretta.

**Avviso (I-4)**: la unit **non** deve includere alcuna direttiva di namespacing filesystem, perché
il servizio deve girare nel mount namespace dell'host per poter smontare/riavviare il mount reale.
Sono vietate in particolare: `PrivateTmp`, `PrivateDevices`, `PrivateMounts`, `ProtectSystem`,
`ProtectHome`, `ReadOnlyPaths`, `ReadWritePaths`, `InaccessiblePaths`, `BindPaths`,
`BindReadOnlyPaths`, `TemporaryFileSystem`, `MountFlags`, `DynamicUser`, `RootDirectory`,
`RootImage`: attivano (o equivalgono a) un mount namespace privato la cui propagazione verso l'host è
disattivata, quindi gli unmount eseguiti dal servizio non raggiungerebbero l'host.

## 8. Test

Test unitari (tutti i package, race detector attivo):

```
go test -count=1 -race ./...
```

La CI GitHub (`.github/workflows/ci.yml`) esegue a ogni push e pull request `gofmt`, `go vet`,
`staticcheck`, `govulncheck`, i test unitari con `-race` e la build `linux/amd64` + `linux/arm64`;
a ogni tag `v*` pubblica una release con i binari e `SHA256SUMS`. Gli e2e richiedono docker, FUSE e
root e si eseguono solo in locale.

Test e2e non-root (scenari E1 "attesa→monitoraggio" ed E2 "down manuale senza recovery"), che avviano
davvero `mount-manager run` e il compose stack reale (`rclone-mount` + `superdufs`):

```
go build -o bin/mount-manager ./cmd/mount-manager && MM_BIN=$PWD/bin/mount-manager go test -count=1 -tags e2e -v -timeout 20m -run '^TestE2E_(WaitThenMonitor|ManualDownNoRecovery)$' ./test/e2e/
```

Precondizioni per gli e2e (non-root e root):

- lo stack di produzione è fermo: nessun container `rclone-mount` o `superdufs` esistente (`docker ps -a`);
- la porta `127.0.0.1:5000` è libera;
- le immagini `rclone/rclone:latest` e `fabiop85/superdufs:v1.5.0` sono presenti (o scaricabili: il test le
  scarica automaticamente se mancano);
- la work dir `test/e2e/.work` sta su un mount con propagazione `shared` (il bind di `rclone-mount` usa
  `rshared`; altrimenti docker risponde `... is mounted on ... but it is not a shared mount`). Se il
  filesystem del repository è `private` (`findmnt -o TARGET,PROPAGATION -T test/e2e/.work`):
  ```
  mkdir -p test/e2e/.work && sudo mount --bind test/e2e/.work test/e2e/.work && sudo mount --make-rshared test/e2e/.work
  ```
  (per annullare: `sudo umount test/e2e/.work`).

Test e2e come root (scenari privilegiati E3-E6, che uccidono/mettono in pausa container e verificano la
recovery reale). Prima si compilano il binario e il pacchetto di test:

```
go build -o bin/mount-manager ./cmd/mount-manager && go test -c -tags e2e -o bin/e2e.test ./test/e2e
```

poi si eseguono via lo script root del repo (richiede la regola sudoers dedicata dell'utente):

```
sudo -n /mnt/fabio/dati/Git/SperimentazioniAI/mount-manager/e2e-root.sh -test.run '^TestE2E_'
```

Scenari root:

- `TestE2E_KillRcloneRecovers` (E3) — `docker kill rclone-mount` con manager attivo: la recovery
  smonta, riavvia lo stack e verifica che torni sano.
- `TestE2E_PausedRcloneRecovers` (E4) — `docker pause rclone-mount`: il probe va in timeout senza
  bloccare il loop, la recovery converge.
- `TestE2E_StaleMountCleanedWhenIdle` (E5) — mount reso stale a manager fermo, poi `compose down`:
  all'avvio del manager il mount stale viene ripulito senza avviare lo stack (D14, I-1).
- `TestE2E_PartialStackAfterBootRecovers` (E6) — `rclone-mount` fermato manualmente mentre `superdufs`
  resta su (stato tipico post-boot): il manager avvia la recovery dopo `startup_grace`.

Garanzia I-6: tutti gli e2e (root e non-root) usano solo un remote `alias` locale generato in una work
dir isolata (`test/e2e/.work/<test>-<timestamp>`), con contenuti fittizi (`canary.txt`,
`dir1/file.txt`); la cartella `./config` del repository (remoti rclone reali) non viene mai letta né
montata.

## 9. Limiti noti

- **`--pull never` richiede immagini locali**: ogni `up` eseguito dalla recovery usa `--pull never`
  (D7, I-5); se le immagini `rclone/rclone:latest` o `fabiop85/superdufs:v1.5.0` non sono già presenti
  sullo host, la recovery fallisce e ritenta all'infinito con backoff crescente finché le immagini non
  vengono scaricate manualmente.
- **`down` manuale durante backoff viene ripristinato**: in backoff lo stack può essere fermo per
  colpa della recovery stessa (`down` riuscito, `up` fallito), quindi lo stato idle non è distinguibile
  da un arresto manuale e il tentativo successivo esegue comunque `down`/`up` (salvo docker non
  raggiungibile, vedi §2). Per fermare davvero lo stack, fermare prima il servizio `mount-manager`.
  In verifying invece l'`up` è riuscito: uno stack idle è stato fermato a mano e viene rispettato.
- **Variabili di `compose.yml` (`DUFS_UID`, `DUFS_GID`)**: `docker compose` dà precedenza alle
  variabili della shell rispetto a `.env`, e la recovery gira con l'ambiente del servizio systemd, non
  con quello della shell. Definirle in `.env` accanto a `compose.yml` (letto tramite
  `--project-directory`) e non esportarle nella shell, altrimenti uno stack avviato a mano e uno
  ricreato dalla recovery possono girare con UID/GID diversi.
- **Lo stato della recovery non sopravvive al riavvio del manager**: nessuno stato viene salvato su
  disco (scelta voluta, per non avere marker spuri che riavvino uno stack fermato apposta). Se il
  manager viene fermato durante una recovery dopo un `down` riuscito e un `up` fallito, lo stack resta
  fermo; al riavvio il manager lo vede idle e, per I-1, non lo rialza. L'evento `shutdown` lo segnala
  (`detail` "signal received during recovery …"); in quel caso avviare lo stack a mano
  (`docker compose up -d`).
- **Upload pendenti nella cache VFS persi al down/up**: i dati non ancora caricati nella cache VFS di
  rclone (`--vfs-cache-mode=full`, `--cache-dir=/tmp/rclone` nel container, non montata come volume)
  vengono persi quando la recovery esegue `down`/`up`, perché il container viene ricreato. È una
  conseguenza intrinseca del ciclo down/up richiesto per la recovery.
- **`superdufs` con `restart: always`**: dopo un riavvio host, `superdufs` può ripartire da solo mentre
  `rclone-mount` (`restart: no`) resta fermo; questo stato viene gestito dal `startup_grace` in waiting,
  non da logica dedicata al boot.
- **Regola sudoers**: la regola `NOPASSWD` dell'utente per gli e2e root copre qualunque file eseguibile
  nella root del repository (`.../mount-manager/*`, i wildcard non attraversano `/`); mantenere la
  root del repository non scrivibile da altri utenti, per evitare che un file aggiunto lì diventi
  eseguibile come root senza password.
- **Esecuzione non-root**: se `mount-manager run` non gira come root, l'unmount (`MNT_DETACH`) e
  l'abort della connessione FUSE falliscono (viene loggato un warning all'avvio); la supervisione resta
  utile per monitoraggio/notifiche ma non può eseguire la recovery completa.
