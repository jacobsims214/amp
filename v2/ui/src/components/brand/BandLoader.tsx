/** The countdown loader: AMP's bands light in launch order while a page waits on data. */
export function BandLoader({ label, className = '' }: { label?: string; className?: string }) {
  return (
    <div role="status" aria-label={label ?? 'Loading'} className={`amp-band-loader ${className}`}>
      <span className="amp-band-loader-seg" aria-hidden="true"><i /><i /><i /><i /></span>
      {label && <span className="amp-band-loader-cap">{label}</span>}
    </div>
  )
}
