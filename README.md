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
- **monitoring** — lo stack è sano; i controlli vengono ripetuti ogni `check_interval`. Se lo stack
  torna idle si torna in waiting (evento `stack_stopped`); se un controllo fallisce per
  `failure_threshold` cicli consecutivi si avvia la recovery.
- **verifying** — dopo un tentativo di recovery, si attende fino a `recovery.verify_timeout` che la
  suite di controlli torni tutta positiva; in caso di successo si torna a monitoring, altrimenti si
  entra in backoff.
- **backoff** — attesa prima del prossimo tentativo di recovery, con durata calcolata dalla formula
  sotto; scaduta l'attesa si ritenta la recovery.

### Stack "idle"

Lo stack è considerato **idle** quando zero container del progetto Compose sono in stato `running`,
`restarting` o `paused` — questo include sia lo stack mai avviato sia un `docker compose down`/`stop`
manuale. Con stack idle il supervisore non esegue mai `down`/`up` (invariante I-1); in waiting o
monitoring lo stato idle riporta (o mantiene) il supervisore in waiting.

### Soglie e grace

- `failure_threshold` (default 3): numero di cicli consecutivi con controllo fallito, in monitoring,
  prima di avviare la recovery.
- `startup_grace` (default 120s): tempo massimo, in waiting, che uno stack avviato ma non sano può
  restare tale prima che scatti la recovery.

### Sequenza di recovery (D6)

Ad ogni tentativo di recovery (`internal/recovery.Recoverer.Recover`), in ordine (I-8):

1. **Cleanup pre-down** del mount host: se il guasto rilevato riguarda mount o probe, prima
   dell'unmount si scrive `abort` sulla connessione FUSE (fusectl); in ogni caso si esegue il detach
   lazy (`MNT_DETACH`).
2. **`docker compose down -t <recovery.down_timeout>`**; se fallisce, fallback a `kill` seguito da
   `down -t 0`.
3. **Cleanup post-down** del mount host, sempre con abort prima del detach.
4. **`docker compose up -d --pull never --wait --wait-timeout <recovery.up_wait_timeout>`**.
5. **Verifica**: il supervisore entra in stato verifying e attende fino a `recovery.verify_timeout`
   che l'intera suite di controlli (C1-C5) torni positiva.

Il cleanup del mount rifiuta di agire se il filesystem montato in cima a `mount_path` non ha
`expected_fstype` (I-2): non smonta mai un mount estraneo.

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
`stale_mount_cleaned`. Motivo: un bind di sorgente stale farebbe fallire il successivo `up` manuale.

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
| `mount_path` | stringa (path assoluto) | *(obbligatorio)* | Path host del mount FUSE da supervisionare. |
| `compose_file` | stringa (path assoluto) | *(obbligatorio)* | Path del file `docker compose` dello stack. |
| `project_directory` | stringa (path assoluto) | dir di `compose_file` | Directory di progetto passata a `docker compose --project-directory`. |
| `docker_binary` | stringa | `docker` | Nome o path del binario docker usato per pilotare compose. |
| `expected_fstype` | stringa | `fuse.rclone` | Filesystem atteso in cima a `mount_path` quando sano. |
| `check_interval` | durata | `10s` | Intervallo tra un controllo e il successivo mentre lo stack gira. |
| `check_timeout` | durata | `10s` | Timeout del subprocess di probe per un singolo controllo. |
| `failure_threshold` | intero | `3` | Cicli falliti consecutivi in monitoring prima della recovery. |
| `startup_grace` | durata | `120s` | In waiting, tempo tollerato di stack avviato ma non sano prima della recovery. |
| `log_level` | stringa | `info` | Uno tra `debug`, `info`, `warn`, `error`. |
| `recovery.down_timeout` | durata | `30s` | Timeout di `docker compose down -t` durante la recovery. |
| `recovery.up_wait_timeout` | durata | `120s` | Timeout di `docker compose up --wait-timeout` durante la recovery. |
| `recovery.verify_timeout` | durata | `120s` | Tempo massimo in stato verifying prima di considerare la recovery fallita. |
| `recovery.command_grace` | durata | `60s` | Tempo extra aggiunto ai timeout dei comandi down/up/kill prima di forzarne la terminazione. |
| `recovery.backoff_initial` | durata | `10s` | Backoff iniziale tra tentativi di recovery. |
| `recovery.backoff_max` | durata | `5m` | Backoff massimo tra tentativi di recovery. |
| `checks.canary` | stringa (path relativo) | `""` | Nome file relativo dentro il mount che deve esistere; vuoto disabilita il controllo canary. |
| `checks.http.url` | stringa (URL) | `""` | URL del check HTTP opzionale (C4); vuoto lo disabilita. |
| `checks.http.timeout` | durata | `5s` | Timeout della richiesta HTTP del check. |
| `checks.http.accept_status` | lista di interi | `[200, 401, 403]` | Codici di stato HTTP considerati sani (superdufs anonimo risponde 401, D17). |
| `checks.exec.service` | stringa | `""` | Servizio compose in cui eseguire il check exec (C5); vuoto lo disabilita. |
| `checks.exec.path` | stringa (path assoluto) | `/data` (se `checks.exec.service` è impostato) | Path assoluto dentro il container del servizio exec da controllare. |
| `checks.exec.timeout` | durata | `10s` | Timeout del comando `exec` del check. |
| `webhook.url` | stringa (URL) | `""` | URL che riceve le notifiche JSON; vuoto disabilita il webhook. |
| `webhook.headers` | mappa stringa→stringa | `{}` | Header HTTP aggiuntivi inviati con ogni richiesta webhook. |
| `webhook.timeout` | durata | `5s` | Timeout della richiesta HTTP del webhook. |

Le durate sono stringhe accettate da `time.ParseDuration` (es. `10s`, `2m`, `1h`). Tutte le durate,
`failure_threshold`, `log_level`, i path e gli URL sono validati al caricamento; gli errori vengono
riportati tutti insieme, con prefisso `config: <campo>: <motivo>`.

## 5. Comandi

```
mount-manager run [--config P]
mount-manager check [--config P]
mount-manager probe --path P [--canary C]
mount-manager version
```

- **`run`** — avvia la supervisione continua: attesa, monitoraggio, recovery con backoff e notifiche
  webhook. Gira finché non riceve `SIGINT`/`SIGTERM`, poi emette l'evento `shutdown` ed esce con
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
`webhook.headers`.

Eventi (`internal/notify`):

| Evento | Quando |
|--------|--------|
| `startup` | all'avvio del processo `run`. |
| `waiting` | all'avvio, subito dopo `startup` (il ritorno in waiting da monitoring emette `stack_stopped`). |
| `stack_up` | quando lo stack passa da waiting a monitoring (tutti i controlli positivi). |
| `stack_stopped` | quando, in monitoring, lo stack torna idle (si torna in waiting). |
| `failure_detected` | quando scatta la recovery (soglia fallimenti in monitoring o grace scaduta in waiting). |
| `recovery_started` | a ogni tentativo di recovery avviato. |
| `recovery_succeeded` | quando, in verifying, la suite di controlli torna positiva. |
| `recovery_failed` | quando la recovery fallisce o il timeout di verifica scade. |
| `stale_mount_cleaned` | quando viene ripulito un mount stale mentre lo stack è idle (D14). |
| `shutdown` | alla terminazione del processo `run` (segnale ricevuto). |

Esempio di payload:

```json
{
  "event": "recovery_started",
  "timestamp": "2026-09-24T10:15:32.123456789Z",
  "host": "storage01",
  "mount_path": "/opt/rclone-stack/mymount",
  "detail": "probe: probe timeout after 10s; http: context deadline exceeded",
  "attempt": 2
}
```

Campi: `event` (stringa, uno dei nomi sopra), `timestamp` (RFC3339 nano), `host` (hostname del
processo), `mount_path` (dalla configurazione), `detail` (testo libero, dipende dall'evento),
`attempt` (numero del tentativo di recovery corrente; `0` per gli eventi non legati a un tentativo).

## 7. Installazione come servizio systemd

L'installazione è manuale; non esiste un installer automatico.

1. Compila il binario:
   ```
   go build -o bin/mount-manager ./cmd/mount-manager
   ```
2. Installa il binario:
   ```
   sudo install -m 0755 bin/mount-manager /usr/local/bin/
   ```
3. Crea la configurazione in `/etc/mount-manager/config.yml` (parti da
   [`deploy/config.example.yml`](deploy/config.example.yml) e adatta i path allo host).
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

Test e2e non-root (scenari E1 "attesa→monitoraggio" ed E2 "down manuale senza recovery"), che avviano
davvero `mount-manager run` e il compose stack reale (`rclone-mount` + `superdufs`):

```
go build -o bin/mount-manager ./cmd/mount-manager && MM_BIN=$PWD/bin/mount-manager go test -count=1 -tags e2e -v -timeout 20m -run '^TestE2E_(WaitThenMonitor|ManualDownNoRecovery)$' ./test/e2e/
```

Precondizioni per gli e2e (non-root e root):

- lo stack di produzione è fermo: nessun container `rclone-mount` o `superdufs` esistente (`docker ps -a`);
- la porta `127.0.0.1:5000` è libera;
- le immagini `rclone/rclone:latest` e `fabiop85/superdufs:v1.5.0` sono presenti (o scaricabili: il test le
  scarica automaticamente se mancano).

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
- **`down` manuale durante backoff/verifying viene ripristinato**: in verifying lo stato idle viene
  ignorato (allo scadere di `recovery.verify_timeout` si passa in backoff) e in backoff il tentativo
  successivo di recovery esegue comunque `down`/`up`; uno stack fermato a mano in queste fasi viene
  quindi riavviato. Per fermare davvero lo stack, fermare prima il servizio `mount-manager`.
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
