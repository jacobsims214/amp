import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

/** The shared protocol vectors (v2/testdata/remote-control), relative to v2/ui where vitest runs. */
const dir = resolve(process.cwd(), '../testdata/remote-control')

export function loadVectors<T = unknown>(file: string): T {
  return JSON.parse(readFileSync(resolve(dir, file), 'utf8')) as T
}
