# Maintainer release and APT repository guide

This document is for repository administrators. End-user installation, configuration, and operation remain in the project [`README.md`](../../README.md).

## Release workflow

[`.github/workflows/release.yml`](../../.github/workflows/release.yml) runs only when a version tag matching `v[0-9]*` is pushed. Ordinary commits and branch pushes do not build or publish releases.

A tagged workflow:

1. checks out the tagged commit and tags;
2. runs all Go tests, `go vet`, selected race-enabled tests, and CGO-disabled ARMv7/ARM64 cross-build checks;
3. builds `amd64`, `arm64`, `armv7`/Debian `armhf`, and `386`/Debian `i386` packages;
4. publishes binaries, archives, Debian packages, checksums, and licenses in a GitHub Release;
5. generates Debian `Packages` and `Release` indexes;
6. verifies the configured private signing key against the pinned public fingerprint;
7. generates and verifies `Release.gpg` and `InRelease`;
8. deploys the signed APT repository through GitHub Pages.

Stable tags become Debian versions without the leading `v`: tag `v1.2.3` produces package version `1.2.3`. Both packages receive the same version. Untagged local packages use a development version beginning with `0.0.0+git` and are not published by this workflow.

## APT signing key

The committed public key is [`packaging/apt/homeautomationmonitorgw-archive-key.asc`](../../packaging/apt/homeautomationmonitorgw-archive-key.asc). The private key must never be committed.

Pinned fingerprint:

```text
FBB4 E68D ABB4 A25F 6E73  C81D D4EA 266E 8BC8 043F
```

Keep the private ASCII-armored export in an offline backup. GitHub Actions secrets can be replaced but cannot be read back after they are saved. Loss of the private key prevents publication of updates trusted by clients that installed the current public key.

### Store the private key in GitHub Actions

1. Open the repository and select **Settings → Secrets and variables → Actions**.
2. In **Repository secrets**—not **Environment secrets**, **Environment variables**, or **Repository variables**—select **New repository secret**.
3. Set **Name** to exactly `APT_SIGNING_PRIVATE_KEY`.
4. Open `homeautomationmonitorgw-archive-private-key.asc` as text.
5. Copy the complete ASCII-armored content, including `-----BEGIN PGP PRIVATE KEY BLOCK-----` and `-----END PGP PRIVATE KEY BLOCK-----`, into **Secret**.
6. Select **Add secret** and confirm that `APT_SIGNING_PRIVATE_KEY` appears under **Repository secrets**.

Do not use an environment-scoped secret. The signing job does not target an environment and reads only the repository secret. The later `github-pages` deployment environment does not need the private key.

The signing job imports the secret into a temporary mode-`0700` GnuPG home, verifies the complete fingerprint, signs only generated repository metadata, verifies both signatures, and removes the temporary keyring. It never prints the secret. Repository administrators who can modify the release workflow remain inside the signing trust boundary.

## Configure GitHub Pages

1. Open **Settings → Pages → Build and deployment**.
2. Set **Source** to **GitHub Actions**.
3. If **GitHub Actions** is already selected, make no further change.
4. Do not select the suggested Jekyll or Static HTML **Configure** buttons and do not create another Pages workflow.
5. Leave **Custom domain** empty unless the repository is intentionally assigned one.

The message that workflow details will appear after the first deployment is expected. The tagged release workflow supplies the Pages artifact.

## Publish a release

Before tagging, require a clean `main`, a configured repository secret, and GitHub Pages set to GitHub Actions:

```bash
git switch main
git status --short
git pull --ff-only

git tag -a v1.2.3 -m "Release v1.2.3"
git push origin v1.2.3
```

Do not move or recreate a successfully published tag. If the workflow fails before publication, diagnose the failed boundary before deciding whether a new patch tag is required.

## Release verification

Require every job in **Tagged release** to succeed:

- `Verify tagged source`
- all architecture package jobs
- `Publish GitHub release`
- `Build signed APT repository`
- `Publish signed APT repository`

Then verify:

```bash
curl --fail --silent --show-error --location \
  https://tseiman.github.io/HomeAutomationMonitorGW/apt/repository-key.asc \
  --output /tmp/homeautomationmonitorgw-archive-key.asc

gpg --batch --show-keys --fingerprint \
  /tmp/homeautomationmonitorgw-archive-key.asc

curl --fail --silent --show-error --location \
  https://tseiman.github.io/HomeAutomationMonitorGW/apt/dists/stable/InRelease \
  --output /tmp/homeautomationmonitorgw-InRelease
```

The public fingerprint must match the pinned value above. Complete client-side verification with the isolated APT or RevPi installation procedure from the primary README. Remove temporary verification files afterward.
