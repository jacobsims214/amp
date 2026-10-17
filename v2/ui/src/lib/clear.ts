// The API treats an empty field in an update as "keep", so an edit form
// that blanks a field has to name it in `clear` for it to be removed.
export function withClears<T extends Record<string, unknown>>(form: T, clearable: (keyof T & string)[]): T & { clear: string[] } {
  return { ...form, clear: clearable.filter(f => typeof form[f] === 'string' && (form[f] as string).trim() === '') }
}
