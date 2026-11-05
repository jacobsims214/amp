# URL state in the AMP UI

What a person sees should survive a reload, a Back press and a pasted link. So
the parts of a view that decide *what* is shown live in the query string, and
the parts that only decide *how it's arranged right now* (zoom, scroll, an open
menu) stay in component state. This page is the convention the UI code cites
(`ui/src/hooks/useSearchParamState.ts`, `useFocusParam.ts`, `lib/projectLink.ts`).
It replaces the missing `decisions/human-work-ux.md §3` that older comments cite.

## Rules

1. **One param, one meaning.** Use a name from the table below. If you need a
   new one, add it here in the same PR.
2. **Defaults are left out.** A value equal to the view's default is removed
   from the URL, so `/project/1/board` stays clean. Write a param only when it
   changes what's shown.
3. **Push for selections, replace for adjustments.** Choosing a ticket, node,
   pill, epic or query pushes a history entry, so Back undoes it. Typing,
   toggles (hide done, focus chain, mine only) and dismissing replace the entry.
   Typed text is written once typing pauses (Board search: 250 ms).
4. **Leave other params alone.** Every write merges into the current query
   string. `usePatchSearchParams` does this and writes several keys in one
   navigation; never call `setSearchParams({ … })` with a fresh object.
5. **Parse defensively.** A missing or unusable value reads as the default. It
   never throws and never shows an error.
6. **Switching views carries `focus` and nothing else.** The sidebar and the
   project view tabs build their links with `viewLink()`, which keeps `?focus=`
   on views that honor it (Mission, My work, Board, Flight plan, Graph).
   Filters belong to the view they're on.
7. **A focus link must not fail silently.** When `?focus=` names a ticket that
   isn't in the project, the view looks it up (`useFocusFallback`). A ticket in
   another project redirects (replace) to the same view there. A ticket that
   doesn't exist or isn't visible shows "Ticket #N was not found, or you can't
   see it" with a Dismiss button that clears `focus`.

Use `useSearchParamState(key, codec)` for a single value, with the shared codecs
(`idParam`, `textParam`, `flagParam(default, on, off)`, `idListParam`). For a
group of keys that change together, such as the Board filter, write a parse
and serialize pair (`lib/boardFilter.ts`) and apply it with
`usePatchSearchParams`.

## Canonical links

Two short links don't name a project or a view, so they can be pasted from
anywhere (Trace, Day reports, an agent's message, a commit):

- **`/t/<ref>`**, a ticket. `<ref>` is the ticket's human ref, `CODE-<id>`
  (`/t/BA-34`), or just the id (`/t/34`, `/t/ba-34` works too). Ticket ids are
  global, so the trailing number decides; the code is only for people reading
  the link. A code that doesn't match still opens the ticket, and the drawer
  says so in one line (`?ref=`). It opens the ticket's
  project Board with the drawer open (replace, so Back skips the hop) and
  keeps every other param: `/t/BA-34?tab=details`, `/t/BA-34?run=12`. A ticket
  that doesn't exist or that you can't see says so.
- **`/r/<run id>`**, a run. It opens the run's ticket with `?run=<id>`
  (`GET /api/runs/:id` says which ticket).

The server links people to tickets this way (`TaskURL` in amp-api, and the
`url` field of `amp_get_task` and `amp_claim_task`). Old
`/project/:id/board?task=<id>` links keep working (`task` is an alias of
`focus`). The drawer's **Copy link**, the ref in its header, the Trace ref and
"Open ticket", Day reports refs and GitHub delivery refs all use `/t/`.

## Params in use

| Param | Where | Values | Default (left out) | History |
|---|---|---|---|---|
| `focus` | every project view that shows tickets | ticket id; opens its drawer (My work outlines the item) | none | push to open, replace to close |
| `ticket`, `task` | old links | aliases of `focus`, rewritten to it on arrival | none | replace |
| `tab` | ticket drawer | `details`, `history` (`runs` reads as `details`) | the ticket's own: Details for a ticket not started and an open human ticket, History otherwise | replace |
| `run` | ticket drawer | run id: opens Details, scrolls to that run and highlights it | none | link only |
| `ref` | ticket drawer | the ref a `/t/` link was typed with, when its code isn't the ticket's project's: shown as a one-line notice | none | link only (set by `/t/`) |
| `event` | ticket drawer | activity id: opens History, scrolls to that entry and highlights it | none | link only |
| `approve` | My work | `<epic id>[,<epic id>…]` or `all` | none | push |
| `user` | project My work | user id, whose My work to show | the viewer | link only |
| `status` | Board | `ready`, `in_progress`, `done`, `waiting` | all states | push |
| `q` | Board search; Trace query | text; on Trace a SHA, branch or PR URL, run on load | empty | Board: replace after a pause. Trace: push on submit |
| `epic` | Board, Graph | epic id | all epics | push |
| `owner` | Board | `mine`, `unowned`, `all`, `<user id>` | `mine` | push |
| `owner` | Graph | `mine` | everyone | replace |
| `done` | Board | `show` (done epics are hidden by default) | `hide` | replace |
| `done` | Graph | `hide` | `show` | replace |
| `sel` | Graph | ticket id; selected node with its chain lit | none | push to select, replace to clear |
| `chain` | Graph | `1`: show only the selected node's chain | off | replace |
| `open` | Trace | `<task id>[,<task id>…]`: expanded stories | none | push |
| `doc` | Library (KB) | doc path: the open doc | none (the list) | push |
| `q` | Library (KB) | search text, run on load | empty | replace |
| `tag` | Library (KB) | tag: filters the list | all tags | push |
| `range` | Report | `1d`, `7d`, `30d`, `all` | `7d` | replace |
| `range` | Portfolio | `7d`, `30d`, `90d`, `365d` | `30d` | push |
| `user` | Day reports | user id: whose reports | you | push |
| `day` | Day reports | `YYYY-MM-DD`: the day your report is built for | today | replace |
| `open` | Day reports | `YYYY-MM-DD`: the saved report that's expanded, or `none` | the newest | replace |
| `v` | Artifact | version number | latest | replace |
| `org` | Portfolio | org id | last viewed, else your first | replace |
| `launch` | any page (debug) | `full` or `short`: forces the launch animation | by timing | read only |

`owner` and `done` mean the same thing on Board and Graph but have different
defaults, because the views do. Each view documents its own defaults. Values
are only compared within a view, never across views.

## Copy link

A "Copy link" button copies the absolute URL that reproduces what's on screen,
built from the same params (Trace: `/trace?q=<q>&open=<task id>`; a ticket:
`/t/<CODE>-<id>`, with `?tab=` only when it isn't the ticket's default; a run:
`/r/<run id>`). Copy links
don't yet resolve viewer-relative values: a Board link with the default
`owner=mine` shows the *recipient's* tickets. Sharing a specific person's view
means choosing that person (or Everyone) in the owner filter first.

## Not in the URL (yet)

- Board collapsed epics and stories, and Graph expanded stories: arrangement,
  not selection. They stay per-view state for now. So do Portfolio's
  split and hidden columns (per-browser preferences).
- Graph search box, zoom and pan, and the "What changed" panel.
- Modals (plan check, epic and story edit, team access) are not addressable.
