/**
 * Following a server-minted download URL (AMP #3979, team-inbox attachments).
 *
 * A request attachment's bytes are served by a second route that re-checks the
 * caller, so the URL a mint answers with is an *additional* capability rather
 * than a way around signing in: it is single-use, short-lived, same-origin and
 * private. That is why the panel navigates to it rather than fetching it — the
 * response's own `Content-Disposition: attachment` names the file and keeps the
 * bytes out of the SPA.
 *
 * It lives here, as one function, for two reasons: nothing else in the UI may
 * compose a download URL by hand (there is no route to compose — the token only
 * exists in the mint's answer), and a test can replace this one seam to assert
 * that a mint is followed without fighting jsdom's non-configurable `Location`.
 */
export function goToDownload(url: string): void {
  if (!url) return
  window.location.assign(url)
}
