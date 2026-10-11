# Controlled provider integration

CI runs this suite on pushes and pull requests with a real `mbsync` binary:

```sh
# Ubuntu/Debian prerequisite: apt-get install isync
go test -race -count=1 -tags integration -timeout 5m -v ./tests/providers
```

The servers bind ephemeral loopback ports and generate short-lived CA roots.
IMAP uses the real go-imap server and client. Synchronization uses the production
configuration generator and runner with fixture-only credentials/CA substitution.
It checks mapped and unmapped folders, nested and empty folders, attachments,
indexing, mid-FETCH disconnects, and an actual server UIDVALIDITY change.

SMTP exercises authentication and verified TLS, MIME/Bcc delivery, acceptance
with failed QUIT, lost DATA acknowledgment, and untrusted certificates. The
recovery flow seals/decrypts the account password, submits the actual recovery
email through that transport, and checks single-use redemption and invalidation
after uncertain delivery. Only its injected trusted sender enables a private
endpoint and private CA; production account paths expose neither override.

These disposable local servers provide deterministic protocol evidence. External
provider interoperability is recorded separately in the release readiness matrix.
