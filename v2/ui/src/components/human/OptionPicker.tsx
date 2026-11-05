/**
 * OptionPicker: a judgment ticket's options as one radio group, fed by the
 * decision doc's current version (its stored options, never parsed from the
 * markdown). The recommended option only carries a badge: choosing is always
 * the person's click (docs/specs/human-decision-docs.md D8). "Something
 * else" is for a decision none of the options covers.
 */
import type { OptionChoice } from '../../lib/decisions'
import type { DecisionAsk } from '../../types'

export function OptionPicker({ ask, value, onChange, disabled, name }: {
  ask: DecisionAsk
  value: OptionChoice
  onChange: (v: OptionChoice) => void
  disabled?: boolean
  /** the radio group's name, unique per ticket */
  name: string
}) {
  const row = 'flex items-start gap-2 px-2.5 py-2 border border-line bg-surface cursor-pointer has-[:checked]:border-ink has-[:disabled]:cursor-default'
  return (
    <div role="radiogroup" aria-label="Options" className="grid gap-1.5" data-testid="option-picker">
      {ask.options.map((o, i) => (
        <label key={i} className={row} style={{ borderRadius: 'var(--radius)' }} data-testid="option">
          <input type="radio" name={name} className="mt-[3px]" checked={value === i} disabled={disabled}
            onChange={() => onChange(i)} />
          <span className="min-w-0 flex-1 text-[13px] leading-relaxed">
            <b className="text-ink">{o.label}</b>
            <span className="text-ink-2"> — {o.consequence}</span>
          </span>
          {i === ask.recommended && (
            <span className="amp-label whitespace-nowrap px-1.5 py-[3px] border border-line text-ink" data-testid="recommended-badge">Recommended</span>
          )}
        </label>
      ))}
      <label className={row} style={{ borderRadius: 'var(--radius)' }}>
        <input type="radio" name={name} className="mt-[3px]" checked={value === 'other'} disabled={disabled}
          onChange={() => onChange('other')} />
        <span className="text-[13px] text-ink-2">Something else</span>
      </label>
    </div>
  )
}
