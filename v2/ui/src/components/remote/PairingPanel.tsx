/**
 * The pairing screen (protocol 7): shown on /sessions/:id when the page was
 * opened from the laptop's pairing link and this browser isn't paired yet.
 */
import { ShieldAlert, ShieldCheck } from 'lucide-react'
import { usePairing } from '../../remote/usePairing'
import type { PairingLink } from '../../remote/pairingLink'
import type { SessionDetail } from '../../remote/types'
import { BandLoader } from '../brand/BandLoader'

export function PairingPanel({ detail, link, onPaired }: { detail: SessionDetail; link: PairingLink; onPaired: () => void }) {
  const { state, untrusted, tryAgain } = usePairing(detail, link, onPaired)

  return (
    <div className="p-5 sm:p-8 max-w-[560px] w-full" aria-live="polite">
      <span className="amp-label">Remote control · pairing</span>
      {state.kind === 'preparing' && (
        <>
          <h2 className="amp-heading m-0 mt-2 text-[20px] text-ink">Pair this device with {detail.host}</h2>
          <div className="py-8"><BandLoader label="Preparing a secure pairing" /></div>
        </>
      )}

      {state.kind === 'waiting' && (
        <>
          <h2 className="amp-heading m-0 mt-2 text-[20px] text-ink">Pair this device with {detail.host}</h2>
          <div className="mt-5 border border-line-str bg-surface px-4 py-5 text-center">
            <div className="amp-label">Pairing code</div>
            <div className="amp-num mt-2 text-[34px] sm:text-[40px] leading-none tracking-[0.08em] text-ink" data-testid="pairing-code">{state.code}</div>
          </div>
          <p className="m-0 mt-4 text-[15px] text-ink font-semibold">Check the laptop shows the same code and press y there.</p>
          <div className="text-[13.5px] text-ink-2 mt-2 leading-relaxed grid gap-2">
            <p className="m-0">If the codes differ, press <b>n</b> on the laptop: someone else may have this link.</p>
            <p className="m-0">{state.posted ? 'Waiting for the laptop…' : 'Sending the request to the laptop…'}</p>
            <p className="m-0 text-[12.5px] text-ink-3">
              This browser shows as <b className="text-ink-2">{state.device_name}</b>. Once paired, it can control this laptop's sessions for up to 30 days.
            </p>
            <p className="m-0 amp-num text-[12px] text-ink-3">Laptop key {detail.harness_fp_display}</p>
          </div>
        </>
      )}

      {state.kind === 'paired' && (
        <div className="mt-3 flex items-center gap-2 text-success-ink text-[15px]" role="status">
          <ShieldCheck size={18} aria-hidden="true" />Paired. Opening the session…
        </div>
      )}

      {state.kind === 'failed' && (
        <>
          <h2 className="amp-heading m-0 mt-2 text-[20px] text-ink">{state.title}</h2>
          <p className="m-0 mt-2 text-[13.5px] text-ink-2 leading-relaxed" role="alert">{state.message}</p>
          {state.canRetry && (
            <button type="button" className="amp-btn amp-btn-primary amp-btn-sm mt-4" onClick={tryAgain}>Try again</button>
          )}
        </>
      )}

      {untrusted > 0 && (
        <div className="mt-4 flex items-start gap-2 border border-danger bg-surface px-2.5 py-2 text-[12.5px] text-danger-ink" role="alert">
          <ShieldAlert size={15} className="shrink-0 mt-0.5" aria-hidden="true" />
          <span>Untrusted data from the relay was dropped ({untrusted}). Only messages signed by the laptop in the link are used.</span>
        </div>
      )}
    </div>
  )
}
