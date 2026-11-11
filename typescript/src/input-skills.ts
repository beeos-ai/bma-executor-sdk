import { RpcError } from './errors.js'
import { JsonRpcErrorCode, type TurnStartParams, type TurnStartResult } from './generated/executor.js'

/** Shape gate only. An opted-in handler must verify and install the exact bytes. */
export function validateInputSkillRequest(config: unknown): boolean {
  if (config === undefined || config === null) return false
  if (typeof config !== 'object' || Array.isArray(config)) throw invalid()
  const c = config as Record<string, unknown>
  if (Object.hasOwn(c, 'input_skill_receipt')) throw new RpcError(JsonRpcErrorCode.InvalidParams, 'input_skill_receipt is effective-only')
  if (!Object.hasOwn(c, 'input_skills')) return false
  if (!Array.isArray(c.input_skills) || c.input_skills.length < 1 || c.input_skills.length > 10) throw invalid()
  const seen = new Set<string>()
  let total = 0
  for (const doc of c.input_skills) {
    const keys = ['skill_id', 'revision_id', 'name', 'body', 'body_sha256']
    if (!doc || typeof doc !== 'object' || Array.isArray(doc) || Object.keys(doc).length !== keys.length
      || keys.some(key => typeof doc[key] !== 'string')
      || !/^skl_[a-zA-Z0-9_-]{1,124}$/.test(doc.skill_id) || !/^skr_[a-zA-Z0-9_-]{1,124}$/.test(doc.revision_id)
      || !/^[0-9a-f]{64}$/.test(doc.body_sha256) || !doc.name.trim() || Buffer.byteLength(doc.name) > 255
      || !doc.body.trim() || Buffer.byteLength(doc.body) > 65536 || seen.has(doc.skill_id)) throw invalid()
    total += Buffer.byteLength(doc.body)
    if (total > 262144) throw invalid()
    seen.add(doc.skill_id)
  }
  return true
}
function invalid(): RpcError { return new RpcError(JsonRpcErrorCode.InvalidParams, 'invalid input_skills') }


export function validateInputSkillReceipt(params: TurnStartParams, result: TurnStartResult): void {
  const selected = params.config?.input_skills ?? []
  const receipt = result.effective?.input_skill_receipt
  if (!selected.length && receipt === undefined) return
  if (!receipt || receipt.input_id !== params.input_id || !Array.isArray(receipt.skills) || receipt.skills.length !== selected.length || !selected.length) throw invalid()
  if (selected.some((doc,i) => receipt.skills[i]?.skill_id !== doc.skill_id || receipt.skills[i]?.revision_id !== doc.revision_id || receipt.skills[i]?.body_sha256 !== doc.body_sha256)) throw invalid()
}
