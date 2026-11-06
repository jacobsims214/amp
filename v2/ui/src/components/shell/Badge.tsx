/** A "waiting on you" count: white on the accent, read as "N waiting on you". */
export function Badge({ n }: { n?: number }) {
  if (!n) return null
  return <span className="ml-auto amp-num text-[11px] leading-none text-white bg-accent px-[5px] py-[2px] shrink-0" aria-hidden="true">{n}</span>
}
