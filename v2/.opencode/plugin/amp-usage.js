/**
 * amp_session_usage — lets an agent read its own model and token usage for
 * this session, so it can report them on its AMP ticket (amp_complete_task,
 * amp_block_task, amp_report_run). AMP reports and capitalizes token spend
 * from those numbers. Harnesses that can't tell an agent its usage just don't
 * have this tool; agents then report what they know (at least the model).
 *
 * opencode v2 plugin (default export with id + setup). Tool input/output use
 * zod, which opencode accepts as a Standard Schema.
 */
import { z } from "zod"

export default {
  id: "amp.session-usage",
  setup(api) {
    api.tool.transform((tools) => {
      tools.add({
        name: "amp_session_usage",
        options: { codemode: false },
        description:
          "Your own model and token usage in this session so far: model, input/output/cache tokens, and cost when the provider " +
          "reports it. Call it right before amp_complete_task or amp_block_task and pass the numbers along, so AMP can report " +
          "token spend per ticket.",
        input: z.object({ ticket: z.number().optional().describe("Optional: the AMP ticket you're about to report on.") }),
        output: z.object({ usage: z.string() }),
        execute: async (_input, ctx) => {
          const res = await api.session.get({ sessionID: ctx.sessionID })
          const s = res?.session ?? res
          const t = s?.tokens
          const usage = {
            session_id: ctx.sessionID,
            model: s?.model ? `${s.model.providerID}/${s.model.id}` : "",
            input_tokens: t?.input ?? null,
            output_tokens: t ? (t.output ?? 0) + (t.reasoning ?? 0) : null,
            cache_read_tokens: t?.cache?.read ?? null,
            cache_write_tokens: t?.cache?.write ?? null,
            cost_usd: s?.cost != null ? Math.round(s.cost * 10000) / 10000 : null,
          }
          return { output: { usage: JSON.stringify(usage) } }
        },
      })
    })
  },
}
