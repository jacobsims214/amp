import type { GitHubHookInfo } from '../types'

/** The address GitHub posts to. The API sends an absolute URL when the server
 *  knows its public address, and a path otherwise; a path is completed with
 *  the origin this page was loaded from. */
export function hookURL(info: Pick<GitHubHookInfo, 'url' | 'setup'>, origin = window.location.origin): string {
  const u = info.setup?.payload_url || info.url
  if (/^https?:\/\//i.test(u)) return u
  return `${origin.replace(/\/+$/, '')}${u.startsWith('/') ? '' : '/'}${u}`
}
