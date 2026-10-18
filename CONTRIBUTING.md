# Contributing to AMP

Thanks for helping build AMP. The project lives in [`v2/`](./v2/); see the
[README](./README.md) for running it locally.

## Before your first pull request: the CLA

AMP is open source under the [Apache License 2.0](./LICENSE). Every contributor
signs the [Contributor License Agreement](./CLA.md) once. It lets the
maintainer keep licensing AMP, including under other terms in the future,
while you keep the rights to your own work.

When you open your first pull request, a bot comments asking you to sign.
Reply on the pull request with exactly:

> I have read the CLA Document and I hereby sign the CLA

The check turns green and stays green for all your future pull requests. If it
doesn't update, comment `recheck`.

If you contribute as part of your job, make sure your employer allows it
(section 5 of the CLA).

## Making a change

- Branch from `main` and keep each pull request to one change.
- API (`v2/amp-api`): `go vet ./...` and `go test ./...` pass. Integration tests
  need `TEST_DATABASE_URL` (Postgres) and, for the knowledge base,
  `TEST_TYPESENSE_URL`.
- UI (`v2/ui`): `npx tsc -b` and `npm run lint` pass.
- Changes must be backward compatible: database migrations only add, and
  existing MCP tools and REST endpoints keep working for current clients.
- If you change how AMP is used, update the guide in [`v2/kb/`](./v2/kb/).
  It ships with AMP and agents teach users from it.
