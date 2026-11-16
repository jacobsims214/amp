# Releasing amp-harness

This is the checklist for cutting an `amp-harness` release. A new tag named
`harness-v<version>` is the only thing that publishes. Create it either from the GitHub UI
(Releases → Draft a new release, section 3a) or by pushing it with git (section 3b): both start
the `Harness release` workflow (`.github/workflows/harness-release.yml`), which waits for Harness
CI on the tagged commit, then builds, checks and publishes the GitHub Release on
`jacobsims214/amp`, filling in its files, title and notes. Branch pushes, PRs and manual runs of
the workflow stop after packaging, and their files stay workflow artifacts.

Background, gotchas and the record of the test run are in the AMP project 2 KB:
`how-to/harness-release.md`. The version policy (what counts as a major, minor or patch) is D14
in `decisions/amp-harness-architecture.md`. Packaging and install.sh are covered in
`how-to/amp-harness-packaging-and-install.md`.

Tagging is a person's step. Agents prepare the commit, and a person creates the tag (in the UI
or with git).

## 1. Before you start

- [ ] The code to release is merged into `main`. The release notes' install line downloads
      `install.sh` from `main` (`raw.githubusercontent.com/<repo>/main/v2/harness/install.sh`),
      so tag a commit on `main`, after the harness branch has merged.
- [ ] Nothing on that commit is known to be red. `Harness` (`harness.yml`: fmt, clippy,
      tests, asset drift) and `Harness smoke` (`harness-smoke.yml`: the smoke test in modes
      auto, 2026-07-28 and 2025-11-25) must pass on it before anything is published, but you
      **don't have to wait for them before tagging**. The release workflow's `gate` job waits
      for them (up to 60 minutes) and dispatches them if they have no run on the commit, so
      tagging right after a merge is fine (section 4). A run that already failed does stop the
      gate, so a glance first saves a wasted release run:

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

The tag must be exactly `harness-v` plus the crate version (`[workspace.package] version` in
`v2/harness/Cargo.toml` on the commit you tag), for example `harness-v0.1.0`. A tag that
doesn't match fails every build job. Use one of the two paths below, not both.

### 3a. From the GitHub UI

1. On `jacobsims214/amp`: **Releases → Draft a new release**.
2. **Choose a tag**: type `harness-vX.Y.Z` and pick "Create new tag: harness-vX.Y.Z on publish".
   **Target**: `main` (the merged release commit from section 2 must be its head; otherwise
   tag with git, section 3b).
3. Leave the title and the description empty, and **don't attach any files**: the workflow
   replaces the title with `amp-harness X.Y.Z`, the notes with the generated install notes,
   and uploads the 11 assets (section 5).
4. **Untick "Set as the latest release"** if GitHub ticks it. AMP's own releases keep Latest;
   the workflow tries to unmark a harness release anyway (section 5). For a prerelease version (with a `-`, e.g.
   `0.2.0-rc.1`), tick **"Set as a pre-release"**: the workflow doesn't change that flag on a
   release that already exists. Never use the UI for a `-test` version (section 9).
5. **Publish release.** That creates the tag, which starts the `Harness release` run.

The release is visible at once but has **no files until `publish` finishes** (up to about an
hour and a quarter if the gate has to wait for CI). Meanwhile `install.sh` without `--version`
may pick it and fail to download; that clears when the assets land. Watch the run:

```sh
gh run list --workflow harness-release.yml --limit 3     # then: gh run watch <id>
```

### 3b. Push the tag with git

```sh
git fetch origin
git tag harness-vX.Y.Z <sha on main>
git push origin harness-vX.Y.Z
gh run list --workflow harness-release.yml --limit 3     # then: gh run watch <id>
```

The workflow creates the release itself; nothing exists on the Releases page until `publish`.

A run takes about 15 minutes, mostly the Intel macOS build, plus however long the gate waits for
CI on the tagged commit (section 4).

## 4. What CI checks on a tag

The jobs run in order, and the release is created only if all of them pass. `gate` runs alongside
them; only `publish` waits for it.

0. **gate** (`harness-v*` tags only; `actions: write`): it needs a successful run of **both**
   `harness.yml` and `harness-smoke.yml` on the tagged commit, and **waits** for them, so a tag
   cut seconds after a merge is fine. Every 30 seconds it lists each workflow's runs on the
   commit (`actions/workflows/<file>/runs?head_sha=$GITHUB_SHA`):
   - a successful run: that workflow is done;
   - a run still in progress: it keeps waiting;
   - no run at all after a 2-minute grace (a path filter didn't fire, or the commit is on no
     branch): it dispatches the workflow on the tag (`gh workflow run <file> --ref <tag>`) and
     waits for that run; if none appears within 5 minutes it fails;
   - every run completed and none succeeded: it fails at once.

   It gives up after **60 minutes** (job limit 65) and also fails after 5 API errors in a row.
   Each failure prints an `::error::` with the `gh run rerun <run id> --failed` recovery
   (section 8), and `publish` doesn't run. It is skipped (passes at once) for `-test` versions,
   whose off-branch commits have no CI runs.
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
4. **publish** (`harness-v*` tags only; needs `package` and `gate`; `contents: write` with
   `GITHUB_TOKEN`, so no extra secrets): it checks `SHA256SUMS` again, expects exactly 4 lines in it, and checks that
   `install.sh` and `amp-harness.rb` are not empty. Then it creates the release, or fills in
   the one made in the UI (section 5).

## 5. What gets published

With no release for the tag yet (tag pushed with git):
`gh release create <tag> dist/* --verify-tag --title "amp-harness <version>" --latest=false`,
with release notes giving the install line and the build commit.

With a release already there (made in the GitHub UI, or a re-run): `gh release upload <tag>
dist/* --clobber`, then `gh release edit` sets the title `amp-harness <version>` and the same
generated notes, then `--latest=false`. GitHub refuses `--latest=false` when no other release
could be Latest; that only logs a `::warning::`, so check `gh release view <tag>` and fix the
badge in the UI if it shows as Latest. The draft and prerelease flags of an existing release are
left as they are (section 3a).

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
  `--clobber` and resets the title and notes. It does not change the draft and prerelease
  flags.
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
only prints a message. It runs only when the job's `ver` step sets `prerelease=false`, that is
when the version (the tag minus `harness-v`) has no `-`: it runs for `harness-v0.1.0` and is
skipped for `harness-v0.2.0-rc.1` and `harness-v0.0.0-test.1`. The rendered formula
`amp-harness.rb` is attached to every release. To publish it by hand, or to wire the step up later:

1. A person creates the tap repo (e.g. `homebrew-amp`) and adds a deploy key or fine-grained
   token as a repo secret.
2. Copy the release's `amp-harness.rb` to `Formula/amp-harness.rb` in the tap and push.
3. To automate it, replace the template step with a checkout of the tap, that copy and a push.
   Keep its `if: steps.ver.outputs.prerelease == 'false'`. Never test `github.ref_name` for a
   `-`: the `harness-v` prefix has one, so that skips every release.

The formula template is `packaging/homebrew/`, rendered by `scripts/render-formula.sh`.

## 8. Rollback

**The release run failed** (the gate timed out or saw red CI, a flaky build, `publish`
itself). Re-run the failed jobs of the same run; it rebuilds from the same tag and fills in the
release:

```sh
gh run list --workflow harness-release.yml --limit 5   # the failed run's id
gh run rerun <id> --failed                              # gate, builds and publish as needed
gh release view harness-vX.Y.Z --json assets --jq '[.assets[].name] | length'   # 11 when done
```

- If the gate failed on red CI, fix or re-run that workflow first (its `::error::` names the
  run), let it pass, then re-run the release run.
- **Never delete and recreate a published release** (one made in the UI, or by `publish`) to
  retry. Re-running is enough, and a deleted release leaves its tag behind.
- If a code fix is needed and the run was a git tag push with no release yet, nothing was
  released: delete the tag and tag the fixed commit again:

  ```sh
  git push origin :refs/tags/harness-vX.Y.Z && git tag -d harness-vX.Y.Z
  ```

  If a release already exists for the tag (UI path), don't move the tag: release the fix as the
  next patch, and treat the empty release as below.

**A bad release was published** (its contents are wrong, not just its run). This is the only
case for deleting a release. Delete the release and its tag:

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
