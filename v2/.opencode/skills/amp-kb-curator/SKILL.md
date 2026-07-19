---
name: amp-kb-curator
description: KB maintenance workflow — pruning stale docs, merging duplicates, compacting annotations into doc content, and reporting KB health
---

# AMP KB Curator Skill

The curator maintains KB health. Run these operations in order.

## Operation 1: Report

Run `amp_kb_status(project_id)`. Review the results. Note total docs, stale docs, annotation-heavy docs.

## Operation 2: Prune

For each stale doc (> 90 days without update):
1. Read the doc
2. Check if the content is still relevant
3. If outdated and no longer relevant: `amp_kb_delete(project_id, path)`
4. If still relevant but just old: leave as-is or update updated_at
5. If partially outdated: annotate with the correction

## Operation 3: Compact

For docs with unresolved annotations:
1. Read the full doc and its annotations
2. Integrate each annotation's correction into the doc content
3. Rewrite the doc with the corrections applied
4. Mark annotations as resolved (set IsResolved=true)

## Operation 4: Merge

For docs with overlapping content:
1. Identify candidates (same tag, similar title)
2. Read both docs
3. Create a merged doc with both contents
4. Delete the source docs
5. Write the new doc at the more appropriate path

## Completion

Always post a summary of what was done (docs pruned, annotations compacted, duplicates merged).
