# Capture runbook (Phase 0 Weeks 3 and 5)

This directory holds the mitmproxy addon that writes HAR files for the golden-test
harness. Capture sprints require a running reference Nextcloud, real clients, and
an operator; they are not automated by CI.

## Bring up the capture stack

```bash
make capture-up
```

Services on the `capture` compose profile:

| Service | Role |
|---|---|
| `mysql` | Alternate reference DB |
| `minio` | S3-compatible object store for later storage work |
| `reference-nextcloud` | Upstream Nextcloud used as the capture origin |
| `mitmproxy` | TLS-intercepting proxy writing `/captures/<scenario>-<ts>.har` |

Tear down with `make capture-down`.

## Week 3 scenarios

Set `CAPTURE_SCENARIO` before each client session:

1. `desktop-login-v2` — official desktop client login flow v2
2. `desktop-capabilities` — OCS capabilities after login
3. `desktop-webdav-propfind` — home `PROPFIND Depth: 0` and `Depth: 1`
4. `status-php` — anonymous `/status.php`
5. `app-password-issue` — `GET /ocs/v2.php/core/getapppassword`

Copy resulting HAR files to `testdata/captures/` and import with `tools/golden-gen`.

## Week 5 scenarios

1. `ios-login` — iOS app login (expect TLS pinning; fall back per Phase 0 risk table)
2. `android-login` — Android app login
3. `desktop-sync-readonly` — browse a folder after login
4. `chunked-upload-v1` — desktop chunked upload
5. `maintenance-mode` — OCS request while the reference instance is in maintenance

Target: ≥50 HAR files archived. Replayable cases should be imported into `testdata/golden/`.

## Addon

`mitmproxy_har.py` names output files from `CAPTURE_SCENARIO` and a UTC timestamp.
