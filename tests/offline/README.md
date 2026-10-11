# Offline application acceptance

Run against a locally built VOXMail image containing its native runtime:

```sh
python3 tests/offline/smoke.py voxmail-local:review
```

For development, `--binary /absolute/path/to/voxmail` mounts a current Go
executable read-only into that image. Make it executable by the image user
(`chmod 755`). The result identifies both the image ID and executable SHA-256;
it is acceptance of that combination, not proof of a published image digest.

The harness creates and removes its own container and data volume, uses
Docker's `none` network, and publishes no ports. It tests initial setup, local
configuration writes, and a restart with persisted enabled mail/SIP/alert
settings. Forced SIP and model provisioning are also enabled deliberately.
Offline mode must preserve local data while suppressing all provider workers,
return HTTP 503 for manual provider operations, create no sync/voice/recovery
jobs, expose its mode in API/readiness, and shut down cleanly. HTTP calls run
inside the container; failed connectivity alone cannot satisfy the assertions.

This fixture complements the restored-backup drill and the active-workload
shutdown test; it does not test live mail providers or speech model inference.
