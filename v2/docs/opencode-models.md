# opencode model roster

Which OpenRouter model each AMP agent runs on in opencode, and why. Checked 2026-10-18 against
OpenRouter's live model list (`https://openrouter.ai/api/v1/models`) and published benchmarks.
Prices move and models ship monthly: re-check before trusting any number here.

The goal: the most capability per dollar, open-weight models first, with each agent's input
types (text, images, video, audio) and thinking level set explicitly.

## The roster

| Agent | Model | Thinking | Sees | Why |
|---|---|---|---|---|
| amp-manager | MiMo-V2.6-Pro | on | text, image, video, audio | Highest-rated open-weight model overall; cheap output; you can paste a screenshot or mockup into the plan |
| amp-researcher | MiMo-V2.6-Pro | on | text, image, video, audio | Best general knowledge and reasoning of the open models |
| amp-worker-backend | DeepSeek V4.1 Flash (1017) | max | text, image | #1 open-weight agentic coder, at a fraction of a cent per ticket |
| amp-worker-frontend | DeepSeek V4.1 Flash (1017) | max | text, image | Same, and it can look at its own screenshots |
| amp-worker-docs | DeepSeek V4.1 Flash (1017) | high | text, image | Mechanical work; high is plenty |
| amp-github-researcher | DeepSeek V4.1 Flash (1017) | high | text, image | Tool-heavy `gh`/`git` loops |
| amp-kb-curator | DeepSeek V4.1 Flash (1017) | high | text, image | |
| amp-reviewer | GLM-5.3 | max | text | Top open model on SWE-bench Verified, and a different model family from the implementers, so a real second opinion |
| amp-github-reviewer | GLM-5.3 | max | text | Same |
| amp-designer (new) | Kimi K3 | high | text, image, video | #1 open model for frontend and design; expensive output, so used only where the look is the deliverable |
| amp-media (new) | MiMo-V2.6-Pro | on | text, image, video, audio | Omni-modal analysis; makes images through the `amp-image-gen` skill |

Image generation (the `amp-image-gen` skill) uses `google/gemini-3.1-flash-image` for drafts
(about $0.07 an image) and `google/gemini-nano-banana-2.1` for finals. There is no open-weight
image generator on OpenRouter.

## The models

Prices are OpenRouter's, US$ per million tokens.

| Model | Open weights | Input / output | Context | Input types | Thinking levels |
|---|---|---|---|---|---|
| `deepseek/deepseek-v4.1-flash-1017` | yes | $0.15 / $0.60 | 1M | text, image | none, low, high, max |
| `z-ai/glm-5.3` | yes | $0.079 / $1.80 | 1M | text | low, high, max |
| `xiaomi/mimo-v2.6-pro` | yes (MIT) | $0.435 / $0.87 | 1.05M | text, image, video, audio | on / off |
| `moonshotai/kimi-k3` | yes | $0.304 / $11.94 | 1M | text, image, video | none, low, high, max |

What the benchmarks say (vendors' own runs unless noted):

- **DeepSeek V4.1 Flash** is the top open-weight model on the Vals Index (57.86%, just ahead of
  Kimi K3) at about $0.30 per test against Kimi's $6.47, and leads every model, closed ones
  included, on Terminal-Bench 2.1 (90.6). The 1017 snapshot improved all eleven of DeepSeek's
  reported benchmarks over the September release, most on agentic tool use (Terminal-Bench 4.0
  31.2 → 42.3), at the same price.
- **GLM-5.3** beats Kimi K3 head to head on SWE-bench Verified (95.4% on Vals' independent harness)
  and Terminal-Bench 2.1, for roughly a third to a seventh of the cost per task. It's text-only.
- **Kimi K3** is still the best open model at frontend and visual design (#1 open model on the
  Arena frontend and Design Arena boards), but its output price is 7–20× the others'. Hence one
  specialist, not the default.
- **MiMo-V2.6-Pro** is the highest open-weight model on the Artificial Analysis Intelligence Index
  (46) and takes every input type. It trails DeepSeek and GLM on the hardest terminal-agent
  benchmarks, so it plans and researches rather than implements.

## How the thinking levels are wired (opencode 2.0.19)

Each model in `opencode.json` declares its input types under `capabilities.input` and its
thinking levels as `variants`, each of which sends OpenRouter's `reasoning` parameter. An agent
picks one with `variant:` in its frontmatter.

Tested against a local capture of the requests opencode sends, then with real calls:

- **Subagents get their variant.** A subagent with `variant: max` thinks hard, and with `off`
  it doesn't think at all. This is how every specialist the manager dispatches runs.
- **A primary agent started with `opencode run --agent` ignores its `variant:`**, and so does a
  `model: x#variant` suffix. The manager then runs at the model's default, which has thinking on.
  In the TUI, pick the level with the variant switcher. `opencode run -m openrouter/<model>#<variant>`
  does work.
- These are silently ignored, so don't rely on them for thinking levels: a `body` on a single model,
  an agent's `options`, and a second provider entry with `canonical: "openrouter"`. A `body` on the
  `openrouter` provider itself does apply, but to every model.

## Changing a model

1. Check the model on OpenRouter: price, `input_modalities`, and whether `supported_parameters`
   includes `reasoning` (and `reasoning_effort` for levels rather than on/off).
2. Add it under `providers.openrouter.models` in `v2/opencode.json` with `capabilities`, `limit`,
   `cost` and `variants`, then point the agent at it.
3. Check: `opencode debug agents` shows the model and variant; a one-line
   `opencode run -m openrouter/<model>#<variant> "Reply OK"` proves the ID works.
4. `make sync-global` to install, `scripts/build-claude-plugin.sh` to regenerate the Claude Code
   plugin (Claude Code agents inherit the session model; the opencode model isn't carried over).

## Sources

- OpenRouter models API, 2026-10-18
- [Vals AI: DeepSeek V4.1 Flash](https://www.vals.ai/models/deepseek_deepseek-v4.1-flash)
- [DeepSeek V4.1 Flash 1017 tested (kingy.ai)](https://kingy.ai/blog/deepseek-v4-1-flash-1017-specs-benchmarks-pricing/)
- [DeepSeek-V4.1-Flash paper](https://arxiv.org/abs/2609.19969)
- [GLM-5.3 vs Kimi K3 (Friendli)](https://friendli.ai/blog/kimi-k3-glm-5.3-coding-agent-benchmark)
- [GLM 5.3 vs DeepSeek V4.1 Flash (Yotta Labs)](https://www.yottalabs.ai/post/glm-5-3-vs-deepseek-v4-1-flash-2026)
- [Artificial Analysis Intelligence Index v4.3](https://artificialanalysis.ai/articles/artificial-analysis-intelligence-index-v4-3)
- [MiMo-V2.6 open weights (The Batch)](https://www.deeplearning.ai/the-batch/an-unexpected-open-weights-leader)
- [Kimi K3 tops Arena's coding leaderboard (The New Stack)](https://thenewstack.io/kimi-k3-open-weight-coding/)
- [Design Arena agentic web dev, October 2026](https://benchlm.ai/benchmarks/designarenaagenticwebdev)
