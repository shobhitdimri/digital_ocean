# sample_test

`integration_test.sh` is an end-to-end test that drives the real HTTP API with `curl`.

```bash
./sample_test/integration_test.sh        # or: make integration
```

By default it builds the server, starts it on a free port with throwaway data and log
directories, runs every check, and stops it. Exit code is 0 only if every check passed.

To test a server that is already running (local, Docker or deployed):

```bash
BASE_URL=http://localhost:8080 ./sample_test/integration_test.sh
```

Each run uses unique flag names, so it can be repeated against the same server. In this mode
the restart/persistence checks are skipped because the script does not own the process.
Set `KEEP=1` to keep the temp directories (logs, `flags.json`, `audit.jsonl`) for inspection.

Requires `bash`, `curl` and `jq`, plus Go when it starts the server itself.

## What it covers

| Area | Checks |
| --- | --- |
| Health | `/healthz`, `/readyz` |
| Validation | missing `X-Actor`, name with a space, name over 64 chars, empty name, unknown JSON field, malformed JSON |
| CRUD | create (201, `Location`, default disabled), duplicate (409), get, list |
| Evaluation | global off/on, per-user override beats global in both directions, override removal |
| Caching | second evaluation is `source=cache`; update, override change and delete each invalidate it |
| Audit | order, actor, timestamp, before/after values, `limit`, history survives delete |
| 404 never 500 | get, update, delete, evaluate on an unknown flag; unknown route (JSON body); 405 with `Allow` |
| Request id | client id echoed, generated when absent |
| Persistence | graceful SIGTERM (exit 0), restart, flags and audit trail still there, log file written |
