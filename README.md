# mount-manager

## Scopo

`mount-manager` supervisiona lo stack Docker Compose composto dal mount rclone (`rclone-mount`) e da
`superdufs`, che espone i file del mount via web. L'applicativo attende che il mount configurato
(`mount_path`) diventi disponibile, poi lo controlla periodicamente. Se il mount smette di rispondere,
cade o presenta un problema di trasporto, l'applicativo smonta la cartella, riavvia il compose
(down/up) e verifica che mount e stack tornino disponibili prima di rimettersi in attesa.

## Stato

In sviluppo — piano `docs/plans/001_plan-MountManager/`.

## Build

```
go build -o bin/mount-manager ./cmd/mount-manager
```
