# Releasing amp-harness

This is the checklist for cutting an `amp-harness` release. Pushing a tag named
`harness-v<version>` is the only thing that publishes. The `Harness release` workflow
(`.github/workflows/harness-release.yml`) then builds, checks and publishes a GitHub Release on
`jacobsims214/amp`. Branch pushes, PRs and manual runs of the workflow stop after packaging, and
their files stay workflow artifacts.

Background, gotchas and the record of the test run are in the AMP project 2 KB:
`how-to/harness-release.md`. The version policy (what counts as a major, minor or patch) is D14
in `decisions/amp-harness-architecture.md`. Packaging and install.sh are covered in
`how-to/amp-harness-packaging-and-install.md`.

Tagging is a person's step. Agents prepare the commit, and a person pushes the tag.

## 1. Before you start

- [ ] The code to release is merged into `main`. The release notes' install line downloads
      `install.sh` from `main` (`raw.githubusercontent.com/<repo>/main/v2/harness/install.sh`),
      so tag a commit on `main`, after the harness branch has merged.
- [ ] `Harness` (`harness.yml`: fmt, clippy, tests, asset drift) and `Harness smoke`
      (`harness-smoke.yml`: the smoke test in modes auto, 2026-07-28 and 2025-11-25) are green
      on that commit. **The release workflow runs neither of them.** It only runs the installer
      tests and the version checks, so check these two yourself:

      ```sh
      gh run list --workflow harness.yml --commit <sha>
      gh run list --workflow harness-smoke.yml --commit <sha>
      ```

- [ ] Choose the version under D14 (semver).
      - From 1.0: a breaking change bumps the major, a new feature the minor, a fix the patch.
      - While the version is 0.x: a breaking change or a new feature bumps the minor
        (`0.1.0` → `0.2.0`), and a fix bumps the patch (`0.1.0` → `0.1.1`).
      - A prerelease has a `-` suffix, for example `0.2.0-rc.1`.
      - A workflow test uses `-test.N`, for example `0.0.0-test.2`. See section 9.

## 2. Bump the version (Cargo.toml and Cargo.lock)

The four workspace crates (`amp-mcp`, `amp-core`, `amp-tui`, `amp-harness`) take their version
from `[workspace.package] version` in `v2/harness/Cargo.toml`. The build uses `--locked`, so
`Cargo.lock` must carry the same version.

- [ ] Set `version = "X.Y.Z"` under `[workspace.package]` in `v2/harness/Cargo.toml`.
- [ ] Update the four `name = "amp-…"` entries in `v2/harness/Cargo.lock` to the same version.
      Either edit them by hand, or run cargo (Rust disk rule: `CARGO_HOME` on the external
      drive):

      ```sh
      cd v2/harness
      CARGO_HOME=/Volumes/external-data/development/.cargo-home cargo update --workspace
      git diff --stat Cargo.lock     # only the 4 workspace entries may change
      ```

- [ ] Optionally build and check: `make -C v2/harness version` prints `amp-harness X.Y.Z`
      on its first line.
- [ ] In `v2/harness/CHANGELOG.md`, replace `unreleased` with the release date and add the
      section for the new version.
- [ ] Commit (e.g. `chore(harness): release X.Y.Z`) and merge to `main` by PR like any change.

The version is already `0.1.0` for the first release, so v0.1.0 needs only the CHANGELOG date.

## 3. Tag and push

The tag must be exactly `harness-v` plus the crate version, for example `harness-v0.1.0`. A
tag that doesn't match fails every build job.

```sh
git fetch origin
git tag harness-vX.Y.Z <sha on main>
git push origin harness-vX.Y.Z
gh run list --workflow harness-release.yml --limit 3     # then: gh run watch <id>
```

A run takes about 15 minutes, mostly the Intel macOS build.

## 4. What CI checks on a tag

The jobs run in order, and the release is created only if all of them pass.

1. **install-test** (ubuntu-latest and macos-latest): shellcheck (Linux), then
   `scripts/test-install.sh`, the offline installer tests. On Linux it also runs
   `scripts/test-install-distros.sh`, which installs in Debian, Ubuntu, glibc 2.31 and Alpine
   containers. Alpine and glibc 2.31 must be refused.
2. **build**: four native targets, no cross-compiling:

   | target | runner |
   |---|---|
   | aarch64-apple-darwin | macos-15 |
   | x86_64-apple-darwin | macos-15-intel |
   | x86_64-unknown-linux-gnu | ubuntu-22.04 (glibc floor 2.35) |
   | aarch64-unknown-linux-gnu | ubuntu-22.04-arm |

   For each target:
   - The runner's host is the target.
   - **The tag matches the crate version**: `harness-v<[workspace.package] version>` must equal
     the tag.
   - `cargo build --release --locked -p amp-harness`, then `scripts/package.sh`.
   - install.sh installs the archive, and the installed binary's `--version` runs.
   - **Version stamp**:
     - The first line of `--version` is `amp-harness <version>`.
     - The `commit` line is a prefix of the tagged commit, and is not `-dirty` or `unknown`.
     - The `assets` line equals the last commit that touched `v2/harness/assets`. This is why
       the checkout uses `fetch-depth: 0`.
     - All six pins are listed: rpi-ai, rpi-agent, rpi-tools, rpi-harness, rpi-tui and rmcp.
3. **package**: `sha256sum -c` on every archive, then writes `SHA256SUMS`. It renders the
   Homebrew formula `amp-harness.rb` from the four checksums (`ruby -c`) and copies `install.sh`.
4. **publish** (`harness-v*` tags only; `contents: write` with `GITHUB_TOKEN`, so no extra
   secrets): it checks `SHA256SUMS` again, expects exactly 4 lines in it, and checks that
   `install.sh` and `amp-harness.rb` are not empty. Then it creates the release.

## 5. What gets published

`gh release create <tag> dist/* --verify-tag --title "amp-harness <version>" --latest=false`,
with release notes giving the install line and the build commit.

| version | GitHub Release | installable how |
|---|---|---|
| `X.Y.Z` | normal release | `install.sh` with no `--version` (the newest non-prerelease `harness-v*`), or `--version X.Y.Z` |
| contains `-` (e.g. `0.2.0-rc.1`) | **prerelease** | only with `--version 0.2.0-rc.1`; the "newest" lookup skips it |
| contains `-test` | **draft** prerelease | not at all: drafts are hidden from the unauthenticated releases API |

- **Never marked latest.** `--latest=false` is always passed, because AMP's own releases in the
  same repo keep GitHub's "Latest" badge. install.sh finds the newest harness release itself.
- **Assets (11 files):**
  - the four `amp-harness-<version>-<target>.tar.gz` archives, each with its `.tar.gz.sha256`;
  - `SHA256SUMS`;
  - `install.sh`;
  - `amp-harness.rb`.
- **Re-running `publish`** on a tag whose release already exists uploads the files again with
  `--clobber`. It does not change the title, the notes or the draft and prerelease flags.
- **No signing or notarization** in v1.

## 6. Verify the install

Run this on at least one machine per OS family. Use a throwaway prefix, not your real install:

```sh
gh release view harness-vX.Y.Z --json isDraft,isPrerelease,url,assets \
  --jq '{isDraft,isPrerelease,url,assets:[.assets[].name]}'     # 11 assets, flags as in section 5

P=$(mktemp -d)        # on the dev Mac: a folder on the external drive instead
curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh \
  | sh -s -- --version X.Y.Z --prefix "$P" --no-setup --no-path-check
"$P/amp-harness" --version      # amp-harness X.Y.Z … commit <tagged sha>
"$P/amp-harness" doctor         # against an AMP you can reach
```

- For a normal release, run the same line again **without** `--version`. It must pick X.Y.Z.
- To check a download by hand: get `SHA256SUMS` and the archive from the release, then run
  `shasum -a 256 -c SHA256SUMS --ignore-missing` on macOS or
  `sha256sum -c SHA256SUMS --ignore-missing` on Linux.
- The post-release check in the plan (M5-VERIFY) installs the published binary and runs
  `make -C v2/harness smoke` against compose with `AMP_SMOKE_ARCHIVE` or `AMP_HARNESS_BIN`
  pointing at it.

## 7. Homebrew (template)

There is no tap repo yet. The `publish` job's last step, "Homebrew tap (template, not wired)",
only prints a message. It is skipped for prereleases. The rendered formula `amp-harness.rb` is
attached to every release. To publish it by hand, or to wire the step up later:

1. A person creates the tap repo (e.g. `homebrew-amp`) and adds a deploy key or fine-grained
   token as a repo secret.
2. Copy the release's `amp-harness.rb` to `Formula/amp-harness.rb` in the tap and push.
3. To automate it, replace the template step with a checkout of the tap, that copy and a push.
   Keep it skipped for versions with a `-`.

The formula template is `packaging/homebrew/`, rendered by `scripts/render-formula.sh`.

## 8. Rollback

**The run failed before `publish`.** Nothing was released. Fix the problem, then delete the tag
and tag again:

```sh
git push origin :refs/tags/harness-vX.Y.Z && git tag -d harness-vX.Y.Z
```

A flaky job can be re-run with `gh run rerun <id> --failed`.

**A bad release was published.** Delete the release and its tag:

```sh
gh release delete harness-vX.Y.Z --cleanup-tag --yes
git push origin :refs/tags/harness-vX.Y.Z 2>/dev/null; git tag -d harness-vX.Y.Z
gh release view harness-vX.Y.Z                         # "release not found"
git ls-remote --tags origin 'harness-vX.Y.Z'           # empty
```

- install.sh's "newest" lookup then falls back to the previous `harness-v*` release.
- Anyone who already installed the bad version can go back with
  `install.sh --version <previous>`, which replaces the binary in place.
- Prefer rolling forward. Don't publish a different commit under a version that people may
  already have installed. Release a fix as the next patch (`X.Y.Z+1`) and add a CHANGELOG
  entry that says what was wrong.
- After a deletion, re-check `CHANGELOG.md` so it doesn't describe a version that no longer
  exists.

## 9. Testing the workflow without releasing

Use a `-test` version on a commit that is on no branch. It is published only as a draft, so it
can't be installed. Then delete it. The exact steps (`git commit-tree`, tag
`harness-v0.0.0-test.N`, `gh release view`, cleanup) are in `how-to/harness-release.md`,
"Prereleases, drafts and test tags". Never push a real version tag as a test.
