// A local agent served over the executor protocol, with an in-memory platform
// in the same process: turn.start, a permission interaction, durable events
// with sequences and ACKs, and session.close. Run: node agent-minimal/main.mjs
import { ExecutorClient, Harness } from '@beeos-ai/bma-sdk/executor'

// --- the platform: persists events, answers interactions ---
const events = []
const harness = new Harness({
  agent: {
    ackedSequence: async (sessionId) => events.filter((event) => event.sessionId === sessionId).length,
    agentEvent: async (_executor, event) => {
      events.push(event)
      console.log(`event #${event.sequence} ${event.type}`)
    },
    agentDelta: (_executor, delta) => console.log(`  preview: ${delta.text}`),
    interactionRequested: async (executor, request) => {
      console.log(`interaction ${request.kind} for ${request.tool_name}: allowing`)
      setImmediate(() => void executor.respondInteraction({ session_id: request.session_id, interaction_id: request.interaction_id, outcome: 'allow' }))
      return `pint_${request.interaction_id}`
    },
    interactionCanceled: async () => {},
  },
})

// --- the agent executor: a scripted "model" ---
const client = new ExecutorClient({
  environmentId: '',
  executor: { name: 'agent-minimal', os: process.platform, arch: process.arch, version: '0.1.0' },
  dialer: harness.inProcessDialer(),
})
client.serveAgent(
  {
    openTurn: async (params) => ({ accepted: true, turn_id: params.turn_id, executor_session_ref: `conv_${params.session_id}`, resumed: false, effective: { model: 'scripted' } }),
    runTurn: async (turn, signal) => {
      const prompt = turn.params.input[0]?.text ?? ''
      const toolUse = turn.emit({ type: 'agent.tool_use', name: 'bash', input: { command: 'date' } })
      const answer = await turn.requestInteraction({ kind: 'permission', tool_use_id: toolUse, tool_name: 'bash' }, signal)
      turn.emit({ type: 'agent.tool_result', tool_use_id: toolUse, content: [{ type: 'text', text: answer.outcome === 'allow' ? new Date().toISOString() : 'denied' }] })
      const messageId = turn.newEventId()
      turn.delta(messageId, 'agent.message', 0, 'Writing the answer…')
      turn.emitWithId(messageId, { type: 'agent.message', content: [{ type: 'text', text: `You said: ${prompt}` }] })
    },
    closeSession: async () => {},
  },
  { models: [{ id: 'scripted' }], interaction_kinds: ['permission'] },
)
await client.start()

const executor = await harness.waitExecutor()
await executor.startTurn({ session_id: 'sess_agent', turn_id: 'turn_1', input_id: 'in_1', input: [{ type: 'text', text: 'what time is it?' }] })
while (!events.some((event) => event.type === 'session.status_idle')) await new Promise((resolve) => setTimeout(resolve, 10))
await executor.closeAgentSession({ session_id: 'sess_agent', reason: 'done' })
client.stop('done')
