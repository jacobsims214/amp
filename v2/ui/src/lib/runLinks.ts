import type { Run } from '../types'

/**
 * Where a run's commit can be opened, derived from its PR's repository:
 * GitHub (`…/owner/repo/pull/N` → `…/owner/repo/commit/<sha>`) and GitLab
 * (`…/-/merge_requests/N` → `…/-/commit/<sha>`). Null when the run has no
 * commit, or no PR to tell which repository it's in.
 */
export function commitURL(run: Pick<Run, 'commit' | 'pr_url'>): string | null {
  const sha = run.commit?.trim()
  if (!sha || !/^[0-9a-f]{7,40}$/i.test(sha) || !run.pr_url) return null
  const gitlab = /^(https?:\/\/[^?#]+?)\/-\/merge_requests\/\d+/i.exec(run.pr_url)
  if (gitlab) return `${gitlab[1]}/-/commit/${sha}`
  const github = /^(https?:\/\/[^/?#]+\/[^/?#]+\/[^/?#]+)\/pull\/\d+/i.exec(run.pr_url)
  if (github) return `${github[1]}/commit/${sha}`
  return null
}

/** The remote session a run happened in, when its harness recorded one. */
export function sessionPath(run: Pick<Run, 'session_id'>): string | null {
  const id = run.session_id?.trim()
  return id && /^[A-Za-z0-9._-]+$/.test(id) ? `/sessions/${encodeURIComponent(id)}` : null
}
