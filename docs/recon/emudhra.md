# eMudhra compromised-intermediate fixture discovery

The verifier itself identifies the compromised set: pull candidate eMudhra
intermediates from a certificate transparency log, then run the verifier.

## Real fixtures — available locally in `prox` (verified 2026-11-04)

The `prox` sibling project ships the real set in `deploy/emudhra/`:

- `em-ov-tls-ca-g3c.pem` — the compromised `EM OV TLS CA - G3C`
  intermediate (P-384, small scalar k=4 — the incident's named cert).
- `em-ov-tls-ca-g3c-key.pem` — its private key (trivially derivable from
  the public cert, k=4; cross-check only).
- `emsign-chain.pem` — the chain: `emSign Root TLS CA - G3` + the
  self-signed `emSign ECC Root CA - G3` anchor (acts as `root.pem`).
- `emsign-root-tls-ca-g3.pem` — the root, standalone.

Verified against this repo's tooling on 2026-11-04:

- `emudhra-check` reports `SMALL-KEY k=4 subject="EM OV TLS CA - G3C"`.
- The key file matches the cert's public key (`openssl pkey -pubout` diff).
- `openssl verify -CAfile emsign-chain.pem em-ov-tls-ca-g3c.pem` → OK.

For T2 use (runbook 1.4): the intermediate is the issuer (`certgen
-anchor small-key -issuer <g3c.pem>` — certgen reconstructs the signing
key from k=4, so the key file is not needed), and `emsign-chain.pem` is
the certlab root pool. The real files are **not** copied into this repo:
they stay in `prox` (committed there, in production use) and are staged
onto the server on demand (runbook 0.3).

## Fallback: full-set discovery (owner step, needs internet)

Only if the 0.2 chain dump shows the inverter's real chain uses a
different eMudhra intermediate. Pull candidates from a certificate
transparency log, then run the verifier.

1. List candidate CA certs issued by eMudhra (crt.sh JSON API):

   ```bash
   curl -s 'https://crt.sh/?q=%22eMudhra%22&opt=match&output=json' > /tmp/emudhra.json
   ```

2. Filter to rows where the cert is a CA (eMudhra OV TLS CA G3 family — P-384
   ECDSA, subject containing `OV TLS CA`), collect their `id` values.
3. Download each: `curl -s https://crt.sh/cert/<id>.pem -o testdata/emudhra/<id>.pem`
   (one file per cert; keep a `manifest.json` with id → subject → serial).
4. Verify the set:

   ```bash
   foxess-local emudhra-check testdata/emudhra/
   ```

   Exactly the eight compromised intermediates must report `SMALL-KEY`; the
   remaining eMudhra intermediates must report `ok (no small key)`.
5. Also fetch the **root** that issued the compromised intermediates (issuer of
   any matching cert — download via its serial on crt.sh) as
   `testdata/emudhra/root.pem`. certlab (task 4) needs it to validate T2 chains.

If fewer than 8 match (crt.sh index gaps), top up from the Mozilla Oct-2026
incident post listing, and record the source in `manifest.json`.

The G3C intermediate — the cert the Oct-2026 incident named — is the one
prox uses in production and the expected T2 issuer. The other seven
compromised intermediates are not needed unless 0.2 points at one of them.

## Offline synthetic fixtures

`testdata/emudhra/synthetic-*.pem` are **not** real eMudhra certificates.
They are self-signed P-384 CA certificates generated in-repo:

- `synthetic-k<N>.pem` (8 files): signed with the compromised small scalars
  `N ∈ {4, 56, 66, 115, 155, 156, 230, 237}` via `emudhra.KeyFromSmallK`, so
  the verifier must flag each as `SMALL-KEY`.
- `synthetic-strong-p384.pem`: a random full-strength P-384 CA; must report
  `ok (no small key)`.
- `synthetic-p256.pem`: a P-256 CA; must report `ok (no small key)` (curve
  mismatch is rejected).

Verify offline (no internet needed):

```bash
go build -o foxess-local ./cmd/foxess-local
./foxess-local emudhra-check testdata/emudhra/
```

Expected: 8 `SMALL-KEY` lines, 2 `ok (no small key)` lines, and
`scanned; 8 small-key intermediate(s) found (expected 8)`.

Because the real set lives in `prox` (not this repo), the synthetic files
stay in `testdata/emudhra/` as the offline verifier fixture — the package
tests generate their own certs inline and do not scan this directory. If
the fallback discovery above is ever run, scan its output in a separate
directory: `emudhra-check` scans all `*.pem` in a directory, and mixing
real and synthetic small-key fixtures would conflate the counts.
