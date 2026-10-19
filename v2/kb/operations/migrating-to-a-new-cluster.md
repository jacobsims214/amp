---
title: Moving AMP to a new cluster
tags: [operations, migration, cluster, postgres, typesense, dex, cutover, runbook]
---

# Moving AMP to a new cluster

Use this runbook to move a running AMP, with everything in it, from one Kubernetes cluster to
another. A typical case is a non-prod cluster you launched on, moving to a prod cluster.
`operations/where-amp-keeps-data.md` explains what each step is protecting. The same restore
steps rebuild AMP from backups after a disaster (`operations/backup-and-disaster-recovery.md`).

**The short answer: dumping Postgres is not enough.** Moving AMP means moving:

1. **Postgres:** two databases, `amp` and `dex`.
2. **Typesense:** the knowledge bases, which have no other copy.
3. **The secrets and values** the old stack runs with.

AMP is read-only for the people using it from the freeze to the cutover. For a small install
that is typically under an hour.

## If you are an agent helping with this

- AMP's own tools go down during the move. **Copy this page somewhere local before the freeze**
  (`amp_guide` topic `operations/migrating-to-a-new-cluster.md`). Work through it with your
  person, one phase at a time.
- **Confirm both kube contexts with your person before running anything.** Write them down as
  `SRC` and `DST`.
  - Never run a command against a context that's only a guess.
  - Never run one against a cluster your person hasn't named for this move.
- **Ask before each step that writes, deletes or scales anything.** That includes every step from
  phase 3 on, and every command against `SRC`.
  - Read-only checks (`get`, `logs`, counts) are fine without asking.
- **Keep the source stack intact until your person signs off** after phase 6. It is the rollback.
- Keep the dumps out of git and out of chat: they hold password hashes, token hashes and
  everything people wrote.

## Phase 0: decide

- **Same domain or new domain?**
  - **Same domain (recommended).** DNS or the gateway moves to the new cluster. If you also pin
    the old secrets and move the `dex` database:
    - existing logins, MCP clients and saved links keep working;
    - API tokens keep working;
    - Claude Code and opencode plugins need no change.
  - **New domain:**
    - Everyone signs in again.
    - Every MCP client needs the new URL: `AMP_MCP_URL` for Claude Code, `--url` / the plugin's
      `url` option for opencode.
    - Links to the old domain in comments, KB pages and bookmarks stop working. Keep a redirect
      from the old domain if you can.
- **Which AMP version.** The new stack starts on the **same image tag** as the old one
  (`global.imageTag`). Upgrade after the move, not during it.
- **When.** Agree a window with the org: no dispatches, no agents running, and nobody writing
  in AMP.

## Phase 1: prepare the new cluster

1. Prerequisites:
   - cert-manager and the CloudNativePG operator installed;
   - a StorageClass;
   - the DNS or gateway setup you'll switch to.
2. **Values.** Start from the old stack's values (`values-production.yaml`) and change only what
   differs: storage classes, `cnpg.instances`, the domain if it changes. Keep the namespace (`amp`)
   if you can. Otherwise see "Different namespace" under Phase 4.
3. **Secrets.** Copy the old stack's secrets into `global.secretOverrides`:

   ```bash
   SRC=<old context>; DST=<new context>; NS=amp
   s() { kubectl --context $SRC -n $NS get secret "$1" -o jsonpath="{.data.$2}" | base64 -d; echo; }
   s amp-oauth2-proxy client-secret      # oauth2ProxyClientSecret
   s amp-oauth2-proxy cookie-secret      # oauth2ProxyCookieSecret: keeps browser sessions
   s amp-valkey-users default            # valkeyPassword
   s amp-bootstrap-admin password        # bootstrapAdminPassword
   ```

   - **`bootstrapAdminPassword` matters.** The bootstrap job writes it into Dex on every
     install and upgrade, so without it the bootstrap admin's password changes after the restore.
   - `typesense.apiKey` may be new. Never use the default `amp-local-dev` in prod.
   - Copy any SSO connector secrets.
   - Put real secrets in your secret store, not in git.
4. **ArgoCD.** If ArgoCD deploys the new stack, turn off auto-sync (self-heal) for it until
   phase 5. Otherwise it undoes the scale-downs below.
5. **Install** the chart on `DST`, then check it:
   - Wait until every pod is ready and both post-install jobs have completed. The pull of the
     Ollama embedding model is the slow one.
   - Sign in once to check the empty stack works, then leave it alone.

## Phase 2: freeze the old stack

1. Tell people the window has started. Stop every agent session and dispatch.
2. Cut traffic, but keep amp-api running so the KB indexing queue can drain:

   ```bash
   kubectl --context $SRC -n $NS scale deploy/envoy --replicas=0
   ```

3. **Drain the KB indexing queue.** `pending`, `active` and `retry` must all be 0:

   ```bash
   VP=$(kubectl --context $SRC -n $NS get secret amp-valkey-users -o jsonpath='{.data.default}' | base64 -d)
   V=$(kubectl --context $SRC -n $NS get pod -o name | grep valkey)
   for k in pending active; do kubectl --context $SRC -n $NS exec $V -c amp-valkey -- sh -c "REDISCLI_AUTH='$VP' valkey-cli llen 'asynq:{kb-index}:$k'"; done
   kubectl --context $SRC -n $NS exec $V -c amp-valkey -- sh -c "REDISCLI_AUTH='$VP' valkey-cli zcard 'asynq:{kb-index}:retry'"
   ```

   Also look at `asynq:{kb-index}:archived` (in asynqmon, at `queues.<domain>`). Those are KB
   writes that failed for good, before the move. Decide with your person whether any need to be
   written again after the move.
4. Stop everything that writes:

   ```bash
   kubectl --context $SRC -n $NS scale deploy/amp-api deploy/amp-mcpoauth deploy/amp-authadmin deploy/dex --replicas=0
   ```

## Phase 3: export

Work in an empty directory with enough disk for the artifacts.

```bash
mkdir -p amp-move/kb && cd amp-move
PG=$(kubectl --context $SRC -n $NS get cluster amp-postgres -o jsonpath='{.status.currentPrimary}')
for db in amp dex; do
  kubectl --context $SRC -n $NS exec $PG -c postgres -- pg_dump -Fc -d $db > $db.dump
done
kubectl --context $SRC -n $NS exec $PG -c postgres -- psql -At -d amp -c \
  "select 'projects',count(*) from projects union all select 'tasks',count(*) from tasks
   union all select 'users',count(*) from users union all select 'artifact_versions',count(*) from artifact_versions" > counts.txt
```

Export every KB except the global one (it rebuilds itself). Run the port-forward in a second
terminal, or background it:

```bash
kubectl --context $SRC -n $NS port-forward svc/typesense 18108:8108 &
TS=http://localhost:18108
TK=$(kubectl --context $SRC -n $NS get deploy amp-api -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="TYPESENSE_API_KEY")].value}')
curl -sf -H "X-TYPESENSE-API-KEY: $TK" $TS/collections | python3 -c '
import json, sys
for c in json.load(sys.stdin):
    if c["name"] != "kb_global_0": print(c["name"], c["num_documents"])' > kb/collections.txt
while read c n; do
  curl -sf -H "X-TYPESENSE-API-KEY: $TK" "$TS/collections/$c" > "kb/$c.schema.json"
  curl -sf -H "X-TYPESENSE-API-KEY: $TK" "$TS/collections/$c/documents/export?exclude_fields=embedding" > "kb/$c.jsonl"
done < kb/collections.txt
kill %1
```

- **Why embeddings are left out:** they're regenerated on import, which keeps the files small and
  avoids depending on the old model's vectors.
- **Check before going on:**
  - `kubectl --context $SRC -n $NS exec -i $PG -c postgres -- pg_restore -l < amp.dump` lists the
    tables. This needs no Postgres tools on your machine.
  - Each `kb/<name>.jsonl` has the number of lines `collections.txt` gives it. The last line has
    no newline, so `wc -l` reads one short.

## Phase 4: restore into the new stack

1. Stop the new stack's writers so nothing touches the databases during the restore:

   ```bash
   kubectl --context $DST -n $NS scale deploy/amp-api deploy/amp-mcpoauth deploy/amp-authadmin deploy/dex --replicas=0
   ```

2. Restore both databases. `--clean --if-exists` replaces what the fresh install created:

   ```bash
   PG=$(kubectl --context $DST -n $NS get cluster amp-postgres -o jsonpath='{.status.currentPrimary}')
   for db in amp dex; do
     kubectl --context $DST -n $NS exec -i $PG -c postgres -- \
       pg_restore --clean --if-exists --no-owner --role=amp -d $db < $db.dump
   done
   ```

   Compare the same counts query against `counts.txt`. They must match.
3. Restore the KBs. The new stack must have Ollama up with its model pulled, since Typesense
   embeds every document as it's imported:

   ```bash
   kubectl --context $DST -n $NS port-forward svc/typesense 18108:8108 &
   TS=http://localhost:18108; TK=<the new stack's typesense.apiKey>
   while read c n; do
     python3 -c 'import json, sys; s = json.load(open(sys.argv[1])); keep = {"name", "fields", "default_sorting_field", "token_separators", "symbols_to_index", "enable_nested_fields"}; print(json.dumps({k: v for k, v in s.items() if k in keep}))' "kb/$c.schema.json" > /tmp/schema.json
     curl -s -H "X-TYPESENSE-API-KEY: $TK" -X DELETE "$TS/collections/$c" > /dev/null
     curl -sf -H "X-TYPESENSE-API-KEY: $TK" -X POST "$TS/collections" -d @/tmp/schema.json > /dev/null
     curl -sf -H "X-TYPESENSE-API-KEY: $TK" -X POST "$TS/collections/$c/documents/import?action=upsert" \
       --data-binary @"kb/$c.jsonl" | grep -vc '"success":true'   # failures: must print 0
   done < kb/collections.txt
   ```

   Then check each collection's `num_documents` (`GET $TS/collections`) against `collections.txt`.
   - **Different namespace:** before creating the collections, change the `url` inside each schema's
     `embedding` field to the new Ollama service (`http://ollama.<namespace>.svc.cluster.local:11434/v1/embeddings`).
   - **If Ollama isn't ready:** create the collections without the `embedding` field and import. Once
     AMP is up, run `amp_kb_reindex` on each KB. It sees a keyword-only KB and rebuilds it with
     semantic search.

## Phase 5: start and verify

1. Scale the new stack back up and turn ArgoCD auto-sync back on:

   ```bash
   kubectl --context $DST -n $NS scale deploy/dex deploy/amp-mcpoauth deploy/amp-authadmin --replicas=1
   kubectl --context $DST -n $NS scale deploy/amp-api --replicas=<your ampApi.replicas>
   ```

   With ArgoCD, a sync puts the replica counts back to what the values say.
2. Before telling anyone, check the new stack. Use port-forward to Envoy, or the new address if
   DNS hasn't moved yet.
   - amp-api logs show migrations up to date and the global KB synced, with no errors.
   - Sign in as yourself and as the bootstrap admin.
   - Open a project. Tickets, comments, history and runs are all there.
   - Open an artifact and an older version of it.
   - The KBs, as an agent:
     - `amp_kb_list` on a project KB, a team KB and an org KB;
     - `amp_kb_search` returns semantic results;
     - `amp_kb_status` reports search mode.
   - Use an API token from before the move. It works.
   - Connect an MCP client (plugin or `/mcp`) and call `amp_whoami`.
   - Spend and day reports show the old numbers.
   - Webhooks: a test delivery succeeds from the new cluster's network.

## Phase 6: cut over

1. Move DNS or the gateway to the new stack.
2. **If the domain changed:**
   - Tell everyone the new URL.
   - Plugin users update `AMP_MCP_URL` (Claude Code) or rerun `setup-opencode.sh --url …`
     (opencode).
   - Everyone signs in again.
3. Watch the first hour: sign-ins, MCP connections, KB writes landing (the queue drains), and
   amp-api errors.
4. Leave the old stack scaled to zero, untouched, until your person signs off. A week is a
   sensible default. Then delete it, with its volumes.

## Rolling back

Until people write to the new stack, rolling back is the freeze undone:

1. Point DNS back at the old stack.
2. Scale its `dex`, `amp-mcpoauth`, `amp-authadmin`, `amp-api` and `envoy` back up.

Once people have written to the new stack, a rollback loses those writes unless you repeat phases
2–4 in the other direction. Decide which, with your person, before going back.

## Things that go wrong

- **Counts don't match after `pg_restore`.** Something wrote during the export. Re-freeze,
  re-export, re-restore.
- **The bootstrap admin can't sign in.** `bootstrapAdminPassword` wasn't pinned, so the
  post-install job set a new one. Read it from `amp-bootstrap-admin` on the new stack, or pin the
  old one and run a helm upgrade.
- **Everyone is signed out, or the UI loops between sign-in and the app.**
  - The cookie secret differs: people sign in once and it's fixed.
  - The oauth2-proxy client secret differs from Dex's static client: pin it and restart `dex`
    and `oauth2-proxy` together.
- **MCP clients get "issuer mismatch" or 401s.** The domain changed. Clients must use the new URL
  and sign in again.
- **A KB import fails on every document.** Typesense can't reach Ollama to embed. Check the
  embedding URL in the schema and that the model is pulled, or use the keyword-only route above.
- **KB search returns nothing for a project.** Its collection name doesn't match its ID. The
  database must come from a dump, never from project imports.
