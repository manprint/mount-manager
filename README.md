# mount-manager

## Scopo

`mount-manager` supervisiona lo stack Docker Compose composto dal mount rclone (`rclone-mount`) e da
`superdufs`, che espone i file del mount via web. L'applicativo attende che il mount configurato
(`mount_path`) diventi disponibile, poi lo controlla periodicamente. Se il mount smette di rispondere,
cade o presenta un problema di trasporto, l'applicativo smonta la cartella, riavvia il compose
(down/up) e verifica che mount e stack tornino disponibili prima di rimettersi in attesa.

## Stato

In sviluppo — piano `docs/plans/001_plan-MountManager/`. Disponibili: `check` (suite di controlli
C1-C5: mount, probe, container, HTTP opzionale, exec opzionale) e `run` (supervisione continua con
attesa, monitoraggio, recovery down/up con backoff e notifiche webhook). Verificati dagli e2e non-root
E1/E2; mancano ancora i file di deploy systemd e gli e2e di recovery come root.

## Build

```
go build -o bin/mount-manager ./cmd/mount-manager
```

## Test

Test unitari (tutti i package, race detector attivo):

```
go test -count=1 -race ./...
```

Test e2e non-root (scenari E1 "attesa→monitoraggio" ed E2 "down manuale senza recovery"), che avviano
davvero `mount-manager run` e il compose stack reale (`rclone-mount` + `superdufs`):

```
go build -o bin/mount-manager ./cmd/mount-manager && MM_BIN=$PWD/bin/mount-manager go test -count=1 -tags e2e -v -timeout 20m -run '^TestE2E_(WaitThenMonitor|ManualDownNoRecovery)$' ./test/e2e/
```

Precondizioni per gli e2e:

- lo stack di produzione è fermo: nessun container `rclone-mount` o `superdufs` esistente (`docker ps -a`);
- la porta `127.0.0.1:5000` è libera;
- le immagini `rclone/rclone:latest` e `fabiop85/superdufs:v1.5.0` sono presenti (o scaricabili: il test le
  scarica automaticamente se mancano).

Garanzia I-6: gli e2e usano solo un remote `alias` locale generato in una work dir isolata
(`test/e2e/.work/<test>-<timestamp>`), con contenuti fittizi (`canary.txt`, `dir1/file.txt`); la cartella
`./config` del repository (remoti rclone reali) non viene mai letta né montata.
