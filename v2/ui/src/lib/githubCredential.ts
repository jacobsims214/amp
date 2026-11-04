import type { GitHubCredentialInfo } from '../types'

// Pure decisions for the GitHub token form (AMP #3161), kept out of the
// component file so react-refresh can hot-reload the component.

/**
 * The server's problem class as one sentence a person can act on.
 *
 * `last_error` is a class from a closed list, never a message — that is what
 * keeps a GitHub error body (which can quote a request) off a settings page.
 * So an unknown class is reported as unknown rather than echoed: the string
 * is server vocabulary, not copy, and a future class must not land on the
 * page raw.
 */
export function credentialProblem(info: Pick<GitHubCredentialInfo, 'last_error'>): string | null {
  switch (info.last_error) {
    case undefined:
    case '':
      return null
    case 'no_encryption_key':
      return 'This server has no encryption key, so a GitHub token cannot be stored: an admin sets AMP_SECRETS_KEY before one can be attached, and a token already stored cannot be read until then.'
    default:
      return 'AMP reports a problem with this credential that this page does not recognise. Removing it and attaching a new token may help.'
  }
}

/** The HTTP status a refused request came back with, when the client attached one. */
export function httpStatus(e: unknown): number | undefined {
  const status = (e as { status?: number } | null)?.status
  return typeof status === 'number' ? status : undefined
}
