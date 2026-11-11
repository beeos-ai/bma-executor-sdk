import { createHash } from 'node:crypto'
import { RpcError } from './errors.js'
import { JsonRpcErrorCode, type SessionCompactParams, type SessionCompactResult, type SessionCompactCancelParams, type SessionCompactCancelResult } from './generated/executor.js'

const id = (value: unknown): value is string => typeof value === 'string' && /^[a-zA-Z0-9_:-]{1,160}$/.test(value)
const operation = (value: unknown): value is string => typeof value === 'string' && /^[a-zA-Z0-9_-]{1,128}$/.test(value)
const invalid = () => new RpcError(JsonRpcErrorCode.InvalidParams, 'invalid context maintenance request or receipt')

export function validateCompactRequest(value: SessionCompactParams): void {
  if (!value || !id(value.session_id) || !operation(value.operation_id) || !id(value.expected_last_input_id) ||
      !Number.isSafeInteger(value.expected_context_version) || value.expected_context_version < 0 || value.expected_context_version >= Number.MAX_SAFE_INTEGER ||
      !Number.isSafeInteger(value.deadline_ms) || value.deadline_ms < 1 || value.deadline_ms > 300000 ||
      typeof value.model !== 'string' || !value.model.trim() || value.model.length > 256 ||
      !value.metadata || typeof value.metadata !== 'object' || Array.isArray(value.metadata) ||
      Object.values(value.metadata).some(item => typeof item !== 'string' || Buffer.byteLength(item) > 32768) ||
      Buffer.byteLength(JSON.stringify(value.metadata)) > 65536 ||
      Object.keys(value).some(key => !['session_id', 'operation_id', 'expected_context_version', 'expected_last_input_id', 'model', 'deadline_ms', 'metadata'].includes(key))) throw invalid()
}

/** Credentials may rotate on readback; the operation's semantic identity cannot. */
export function compactIdentity(value: SessionCompactParams): string {
  return JSON.stringify([value.session_id, value.operation_id, value.expected_context_version, value.expected_last_input_id, value.model])
}

export function validateCompactReceipt(request: SessionCompactParams, result: SessionCompactResult): void {
  if (!result || result.session_id !== request.session_id || result.operation_id !== request.operation_id ||
      result.previous_context_version !== request.expected_context_version || result.context_version !== request.expected_context_version + 1 ||
      !Number.isSafeInteger(result.context_version) || !Number.isSafeInteger(result.source_event_count) || result.source_event_count < 1 ||
      typeof result.summary !== 'string' || !result.summary.trim() || Buffer.byteLength(result.summary) > 524288 ||
      result.summary_sha256 !== createHash('sha256').update(result.summary).digest('hex') ||
      Object.keys(result).some(key => !['session_id', 'operation_id', 'previous_context_version', 'context_version', 'summary', 'summary_sha256', 'source_event_count'].includes(key))) throw invalid()
}


export function cancelAsCompact(value:SessionCompactCancelParams):SessionCompactParams {
  return {...value,deadline_ms:1,metadata:{}}
}
export function validateCompactCancel(value:SessionCompactCancelParams):void {
  validateCompactRequest(cancelAsCompact(value))
  if(Object.keys(value).some(key=>!['session_id','operation_id','expected_context_version','expected_last_input_id','model'].includes(key)))throw invalid()
}
export function validateCompactCancelReceipt(request:SessionCompactCancelParams,result:SessionCompactCancelResult):void {
  if(!result || result.session_id!==request.session_id || result.operation_id!==request.operation_id || typeof result.canceled!=='boolean' || ('receipt' in result && !result.receipt) || result.canceled===!!result.receipt || Object.keys(result).some(key=>!['session_id','operation_id','canceled','settled','receipt'].includes(key)))throw invalid()
  if(result.receipt)validateCompactReceipt(cancelAsCompact(request),result.receipt)
}
