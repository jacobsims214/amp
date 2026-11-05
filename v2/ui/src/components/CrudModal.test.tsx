// Dialog keys (#128): Enter in a one-line field saves, Escape closes, and
// closing with something typed asks first, in the dialog (no confirm()).
import { fireEvent, render, screen, within } from '@testing-library/react'
import { StrictMode, useState } from 'react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { CrudModal } from './CrudModal'
import { ConfirmDeleteModal } from './ConfirmDeleteModal'

function renderDialog(props: { onSave?: () => void; onClose?: () => void } = {}) {
  const onSave = props.onSave ?? vi.fn()
  const onClose = props.onClose ?? vi.fn()
  render(
    <CrudModal title="New Thing" onClose={onClose} onSave={onSave}>
      <input aria-label="Name" />
      <textarea aria-label="Notes" />
    </CrudModal>,
  )
  return { onSave, onClose }
}

describe('CrudModal keys', () => {
  it('is a labelled dialog', () => {
    renderDialog()
    expect(screen.getByRole('dialog', { name: 'New Thing' })).toBeInTheDocument()
  })

  it('Enter in a one-line field saves; Enter in a textarea is a new line', async () => {
    const { onSave } = renderDialog()
    await userEvent.type(screen.getByLabelText('Notes'), 'one{Enter}two')
    expect(onSave).not.toHaveBeenCalled()
    expect(screen.getByLabelText('Notes')).toHaveValue('one\ntwo')
    await userEvent.type(screen.getByLabelText('Name'), 'Launch{Enter}')
    expect(onSave).toHaveBeenCalledTimes(1)
  })

  it('Escape closes an untouched dialog at once', async () => {
    const { onClose } = renderDialog()
    await userEvent.keyboard('{Escape}')
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('with something typed, Escape asks first; Keep editing stays, Discard closes', async () => {
    const { onClose } = renderDialog()
    await userEvent.type(screen.getByLabelText('Name'), 'Launch')
    await userEvent.keyboard('{Escape}')
    expect(onClose).not.toHaveBeenCalled()
    const ask = screen.getByRole('alertdialog', { name: 'Discard changes?' })
    expect(ask).toHaveTextContent("Discard what you've entered?")
    expect(screen.getByRole('button', { name: 'Keep editing' })).toHaveFocus()
    await userEvent.keyboard('{Escape}') // Escape again: back to editing
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(screen.getByLabelText('Name')).toHaveValue('Launch')

    await userEvent.keyboard('{Escape}')
    await userEvent.click(screen.getByRole('button', { name: 'Discard' }))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('never uses the browser confirm', async () => {
    const confirm = vi.spyOn(window, 'confirm')
    renderDialog()
    await userEvent.type(screen.getByLabelText('Name'), 'x')
    await userEvent.keyboard('{Escape}')
    expect(confirm).not.toHaveBeenCalled()
    confirm.mockRestore()
  })

  it('a dialog without Save (changes apply as made) closes on Escape without asking', async () => {
    const onClose = vi.fn()
    render(<CrudModal title="Team access" onClose={onClose}><input aria-label="Team" /></CrudModal>)
    await userEvent.type(screen.getByLabelText('Team'), 'Core{Enter}')
    await userEvent.keyboard('{Escape}')
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('IME: Escape or Enter that belongs to a composition does nothing', () => {
    const { onSave, onClose } = renderDialog()
    const name = screen.getByLabelText('Name')
    fireEvent.keyDown(name, { key: 'Enter', isComposing: true })
    fireEvent.keyDown(name, { key: 'Enter', keyCode: 229 }) // Safari's confirming Enter
    fireEvent.keyDown(document, { key: 'Escape', isComposing: true })
    expect(onSave).not.toHaveBeenCalled()
    expect(onClose).not.toHaveBeenCalled()
  })

  it('changes that apply at once (data-applies-now) are nothing to discard', async () => {
    const onClose = vi.fn()
    render(
      <CrudModal title="Edit Epic" onClose={onClose} onSave={vi.fn()}>
        <div data-applies-now><select aria-label="Owner"><option>a</option><option>b</option></select></div>
      </CrudModal>,
    )
    await userEvent.selectOptions(screen.getByLabelText('Owner'), 'b')
    await userEvent.keyboard('{Escape}')
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('Enter in a field with suggestions (a datalist) picks, it does not save', async () => {
    const onSave = vi.fn()
    render(<CrudModal title="T" onClose={vi.fn()} onSave={onSave}><input aria-label="Key" list="keys" /><datalist id="keys" /></CrudModal>)
    await userEvent.type(screen.getByLabelText('Key'), 'jira{Enter}')
    expect(onSave).not.toHaveBeenCalled()
  })
})

// The opener, the dialog, and focus between them (#128 fix round 1).
function Opener({ autoFocus = true }: { autoFocus?: boolean }) {
  const [open, setOpen] = useState(false)
  return <>
    <button type="button" onClick={() => setOpen(true)}>+ Epic</button>
    <button type="button">Behind</button>
    {open && (
      <CrudModal title="New Epic" onClose={() => setOpen(false)} onSave={vi.fn()}>
        <input aria-label="Name" autoFocus={autoFocus} />
        <textarea aria-label="Notes" />
      </CrudModal>
    )}
  </>
}

describe('CrudModal focus', () => {
  it('gives focus back to the button that opened it, though a field took autoFocus', async () => {
    render(<Opener />)
    await userEvent.click(screen.getByRole('button', { name: '+ Epic' }))
    expect(screen.getByLabelText('Name')).toHaveFocus()
    await userEvent.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.getByRole('button', { name: '+ Epic' })).toHaveFocus()
  })

  it('opened from the keyboard: Enter on the opener, Escape, focus is back on it', async () => {
    render(<Opener />)
    screen.getByRole('button', { name: '+ Epic' }).focus()
    await userEvent.keyboard('{Enter}')
    await userEvent.keyboard('{Escape}')
    expect(screen.getByRole('button', { name: '+ Epic' })).toHaveFocus()
  })

  it('Tab and Shift+Tab stay inside the dialog', async () => {
    render(<Opener />)
    await userEvent.click(screen.getByRole('button', { name: '+ Epic' }))
    const dialog = screen.getByRole('dialog', { name: 'New Epic' })
    for (let i = 0; i < 8; i++) {
      await userEvent.tab()
      expect(dialog).toContainElement(document.activeElement as HTMLElement)
    }
    screen.getByRole('button', { name: 'Close' }).focus() // the first focusable
    await userEvent.tab({ shift: true })
    expect(screen.getByRole('button', { name: 'Save' })).toHaveFocus() // wrapped to the last
    await userEvent.tab()
    expect(screen.getByRole('button', { name: 'Close' })).toHaveFocus()
    expect(screen.getByRole('button', { name: 'Behind' })).not.toHaveFocus()
  })
})

describe('ConfirmDeleteModal focus', () => {
  function DeleteOpener() {
    const [open, setOpen] = useState(false)
    return <>
      <button type="button" onClick={() => setOpen(true)}>Delete epic</button>
      {open && <ConfirmDeleteModal title="Delete E" description="Gone." onClose={() => setOpen(false)} onConfirm={async () => {}} />}
    </>
  }

  it('starts on Cancel, keeps Tab inside, and closes back to the opener on Escape', async () => {
    render(<DeleteOpener />)
    await userEvent.click(screen.getByRole('button', { name: 'Delete epic' }))
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus()
    const dialog = screen.getByRole('alertdialog', { name: 'Delete E' })
    for (let i = 0; i < 5; i++) {
      await userEvent.tab()
      expect(dialog).toContainElement(document.activeElement as HTMLElement)
    }
    await userEvent.keyboard('{Escape}')
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(screen.getByRole('button', { name: 'Delete epic' })).toHaveFocus()
  })
})

describe('CrudModal focus under StrictMode and edge cases (#128 fix round 2)', () => {
  it('in StrictMode the autoFocus field keeps focus, and Escape still returns to the opener', async () => {
    render(<StrictMode><Opener /></StrictMode>)
    await userEvent.click(screen.getByRole('button', { name: '+ Epic' }))
    expect(screen.getByLabelText('Name')).toHaveFocus()
    await userEvent.keyboard('Launch')
    expect(screen.getByLabelText('Name')).toHaveValue('Launch')
    await userEvent.keyboard('{Escape}') // typed: asks first
    await userEvent.click(screen.getByRole('button', { name: 'Discard' }))
    expect(screen.getByRole('button', { name: '+ Epic' })).toHaveFocus()
  })

  it('when the opener is gone (an empty state\'s button), focus goes to the main content', async () => {
    function EmptyState() {
      const [open, setOpen] = useState(false)
      const [saved, setSaved] = useState(false)
      return <main id="main-content" tabIndex={-1}>
        {!saved && <button type="button" onClick={() => setOpen(true)}>Create your first epic</button>}
        {open && <CrudModal title="New Epic" onClose={() => setOpen(false)} onSave={() => { setSaved(true); setOpen(false) }}>
          <input aria-label="Name" autoFocus />
        </CrudModal>}
      </main>
    }
    render(<StrictMode><EmptyState /></StrictMode>)
    await userEvent.click(screen.getByRole('button', { name: 'Create your first epic' }))
    await userEvent.type(screen.getByLabelText('Name'), 'Launch{Enter}')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.getByRole('main')).toHaveFocus()
  })

  it('with two dialogs open, only the top one keeps Tab', async () => {
    function Stacked() {
      const [inner, setInner] = useState(false)
      return (
        <>
          <CrudModal title="Outer" onClose={vi.fn()}>
            <button type="button" onClick={() => setInner(true)}>Open inner</button>
          </CrudModal>
          {/* A sibling, as a portal would be: outside the outer dialog's panel. */}
          {inner && <ConfirmDeleteModal title="Inner" description="x" onClose={() => setInner(false)} onConfirm={async () => {}} />}
        </>
      )
    }
    render(<StrictMode><Stacked /></StrictMode>)
    await userEvent.click(screen.getByRole('button', { name: 'Open inner' }))
    const inner = screen.getByRole('alertdialog', { name: 'Inner' })
    expect(within(inner).getByRole('button', { name: 'Cancel' })).toHaveFocus()
    for (let i = 0; i < 6; i++) {
      await userEvent.tab()
      expect(inner).toContainElement(document.activeElement as HTMLElement)
    }
  })
})
