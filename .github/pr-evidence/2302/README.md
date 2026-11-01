# PR evidence: harness on the public home page (AMP #2302)

Before/after captures for the PR that takes `feat/harness-home-page` into `main`.

| File | What it shows |
|---|---|
| `01-home-before.jpg` | `/home/` on `main` (83c1398): the "Any harness" block, first fold |
| `02-home-after.jpg` | `/home/` on `feat/harness-home-page` (e947f9d): same block, with the "AMP Harness" chip and the new "AMP Harness" section below it |
| `03-harness-page-hero.jpg` | new page `/home/harness/`, hero |
| `04-harness-page-features.jpg` | new page `/home/harness/`, "Everything a crew needs" feature grid |

Captured with headless Chrome (CDP) at 1440 px wide, 2x device scale, full page; the
starfield canvas is hidden so the capture is deterministic. Served from the static files as
`git archive` produced them for each commit, so no app build is involved.

This branch exists only to host the images: GitHub's API cannot attach images to a pull request
body, and these files must not be merged.
