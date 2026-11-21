import assert from 'node:assert/strict'
import { test } from 'node:test'
import { AgentRuntime, type AgentHost, type AgentHandler } from '../src/agent.js'

// Dispatch starts session.close synchronously up to its wait on turn.done.
test('receipt refusal releases a session.close already waiting on preparation', {timeout:2000}, async () => {
  let release!:()=>void, began!:()=>void
  const held=new Promise<void>(r=>{release=r}),started=new Promise<void>(r=>{began=r})
  let ran=false
  const host:AgentHost={call:async()=>({accepted:true}) as never,notify(){},stopSignal:new AbortController().signal,connected:()=>true,nextRegistration:async()=>{},addSession(){},removeSession(){},sessionEnded:async()=>({accepted:true}),log(){}}
  const handler:AgentHandler={openTurn:async p=>{began();await held;return {accepted:true,turn_id:p.turn_id,executor_session_ref:'s',resumed:false,effective:{}}},runTurn:async()=>{ran=true},closeSession:async()=>{}}
  const runtime=new AgentRuntime(host,handler,{enabled:true,input_skills:true})
  const opening=runtime.dispatch('turn.start',{session_id:'s',turn_id:'t',input_id:'i',input:[{type:'text',text:'x'}],config:{input_skills:[{skill_id:'skl_one',revision_id:'skr_one',name:'One',body:'skill',body_sha256:'a'.repeat(64)}]}})!
  await started
  const closing=runtime.dispatch('session.close',{session_id:'s',reason:'test'})!
  release()
  await assert.rejects(opening)
  assert.deepEqual(await closing,{closed:true})
  assert.equal(ran,false)
})
