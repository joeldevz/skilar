/** Skynex detached-workflow notification bridge. */
import { isAbsolute } from "node:path"
import type { Plugin, PluginModule } from "@opencode-ai/plugin"

export type Notice = { ID:string; WorkflowID:string; JobID:string; TerminalState:string; JobState:string; Error:string; ClaimToken:string; ClaimedBy:string; CreatedAt:string }
export type PollerDeps = {
  claim(sessionID:string): Promise<Notice|undefined>
  notify(notice:Notice): Promise<void>
  prompt(sessionID:string,notice:Notice): Promise<void>
  ack(notice:Notice): Promise<void>
  release?(notice:Notice): Promise<void>
}

export function createNotificationPoller(deps:PollerDeps) {
  const inFlight = new Set<string>()
  return { async poll(sessionID:string):Promise<boolean> {
    if (!sessionID || inFlight.has(sessionID)) return false
    inFlight.add(sessionID)
    let notice:Notice|undefined
    try {
      notice = await deps.claim(sessionID)
      if (!notice) return false
      try { await deps.notify(notice) } catch {}
      await deps.prompt(sessionID,notice)
      await deps.ack(notice)
      return true
    } catch {
      if (notice && deps.release) await deps.release(notice).catch(()=>{})
      return false
    } finally { inFlight.delete(sessionID) }
  }}
}

export function startNotificationPolling(poller:{poll(id:string):Promise<boolean>},listIdle:()=>Promise<string[]>,intervalMs=2000) {
  const idleSessions=new Set<string>()
  const tick=async(id:string)=>{idleSessions.delete(id);const handled=await poller.poll(id);if(!handled)idleSessions.add(id)}
  void listIdle().then(ids=>{for(const id of ids){idleSessions.add(id);void tick(id)}}).catch(()=>{})
  const timer=setInterval(()=>{for(const id of [...idleSessions])void tick(id)},intervalMs)
  ;(timer as unknown as {unref?:()=>void}).unref?.()
  return { idle(id:string){if(id){idleSessions.add(id);void tick(id)}}, busy(id:string){idleSessions.delete(id)}, stop(){clearInterval(timer)} }
}

export function startSessionPresence(heartbeat:(id:string)=>Promise<void>,intervalMs=5000) {
	const sessions=new Set<string>()
	const beat=()=>{for(const id of sessions)void heartbeat(id).catch(()=>{})}
	const timer=setInterval(beat,intervalMs);(timer as unknown as {unref?:()=>void}).unref?.()
	return {add(id:string){if(id){sessions.add(id);void heartbeat(id).catch(()=>{})}},remove(id:string){sessions.delete(id)},stop(){clearInterval(timer)}}
}

export function resolveWorkflowBinary(raw:unknown):string {
	const binary=value(raw)
	if(!binary||binary.trim()!==binary||binary.includes("\0")||!isAbsolute(binary))throw new Error("SKYNEX_WORKFLOW_BINARY must be an explicit absolute path")
	return binary
}

function run(binary:string,directory:string,args:string[]):{code:number;stdout:string} {
  const result=Bun.spawnSync([binary,"workflow","notifications",...args],{cwd:directory,stdout:"pipe",stderr:"ignore"})
  return {code:result.exitCode,stdout:result.stdout.toString().trim()}
}
function claim(binary:string,directory:string,sessionID:string,activeSessions:string[]):Notice|undefined {
	const args=["claim","--consumer",sessionID,"--allow-rebind"]
	for(const id of activeSessions)args.push("--active-session",id)
	const result=run(binary,directory,args);if(result.code!==0||!result.stdout||result.stdout==="null")return
  try{return JSON.parse(result.stdout) as Notice}catch{return}
}
function disposition(binary:string,directory:string,action:"ack"|"release",notice:Notice):void {
	const result=run(binary,directory,[action,"--id",notice.ID,"--claim-token",notice.ClaimToken])
	if(result.code!==0)throw new Error(`workflow notification ${action} failed`)
}
function value(v:unknown):string{return typeof v==="string"?v:""}
function record(v:unknown):Record<string,unknown>|undefined{return v!==null&&typeof v==="object"&&!Array.isArray(v)?v as Record<string,unknown>:undefined}
export function eventSession(event:unknown):string {
	const p=record(record(event)?.properties)??{}
	return value(p.sessionID)||value(p.sessionId)||value(p.id)||value(record(p.info)?.id)
}

export function notificationMessage(notice:Notice):string {
	if(notice.JobState==="failed")return `Workflow ${notice.WorkflowID} job failed while workflow is ${notice.TerminalState}: ${notice.Error||"unknown error"}`
	if(notice.JobState==="cancelled"||notice.TerminalState==="aborted")return `Workflow ${notice.WorkflowID} was aborted.`
	return `Workflow ${notice.WorkflowID} reached ${notice.TerminalState}.`
}

type PromptClient={session:{
	get(options:unknown):Promise<{data?:unknown;error?:unknown}>
	prompt(options:unknown):Promise<{error?:unknown}>
}}
export async function promptSession(client:PromptClient,sessionID:string,notice:Notice):Promise<void>{
	const current=await client.session.get({path:{id:sessionID},throwOnError:true})
	const session=record(current.data)
	const model=record(session?.model)
	const agent=value(session?.agent)
	const providerID=value(model?.providerID)
	const modelID=value(model?.id)
	const rawVariant=model?.variant
	const variant=rawVariant==="default"||rawVariant===undefined?undefined:value(rawVariant)
	if(current.error||!agent||agent.trim()!==agent||!providerID||providerID.trim()!==providerID||!modelID||modelID.trim()!==modelID||(rawVariant!==undefined&&rawVariant!=="default"&&!variant))throw new Error("OpenCode session is missing its persisted agent/model identity")
	const response=await client.session.prompt({path:{id:sessionID},body:{agent,model:{providerID,modelID},...(variant?{variant}:{}),parts:[{type:"text",text:`${notificationMessage(notice)} Run read-only workflow status and inspect, validate the persisted evidence, then automatically continue the next safe managed action under the continuous execution policy. Do not pause merely to report status or ask permission. Continue without notifying the user unless the workflow is completed, has a genuine blocker or unresolved human gate, faces destructive ambiguity, or retries are exhausted. Never auto-approve, auto-deliver, commit, push, or create a PR.`}]},throwOnError:true})
	if(response.error)throw new Error("OpenCode rejected workflow wake prompt")
}

export const SkynexWorkflow:Plugin=async({client,directory})=>{
	const binary=resolveWorkflowBinary(process.env.SKYNEX_WORKFLOW_BINARY)
	const knownSessions=new Set<string>()
	const presence=startSessionPresence(async id=>{run(binary,directory,["presence","--session",id])})
	const poller=createNotificationPoller({
		claim:async sessionID=>claim(binary,directory,sessionID,[...knownSessions]),
		notify:async notice=>{await client.tui.showToast({body:{title:"Skynex workflow finished",message:notificationMessage(notice),variant:notice.JobState==="failed"?"error":"info"}})},
		prompt:async(sessionID,notice)=>promptSession(client as unknown as PromptClient,sessionID,notice),
    ack:async notice=>disposition(binary,directory,"ack",notice),
    release:async notice=>disposition(binary,directory,"release",notice),
  })
	const loop=startNotificationPolling(poller,async()=>{
		try {
			const api=client.session as unknown as {status():Promise<{data?:Record<string,{type?:string}>}>}
			const response=await api.status();const entries=Object.entries(response.data??{});for(const [id] of entries){knownSessions.add(id);presence.add(id)}return entries.filter(([,s])=>s.type==="idle").map(([id])=>id)
		} catch { return [] }
	})
  return {
		"shell.env":async(input,output)=>{const id=value((input as {sessionID?:unknown}).sessionID);if(id){knownSessions.add(id);presence.add(id);output.env.SKYNEX_OPENCODE_SESSION_ID=id}},
    event:async({event})=>{
      const id=eventSession(event)
		if(id&&event.type!=="session.deleted"){knownSessions.add(id);presence.add(id)}
		if(event.type==="session.idle"&&id)loop.idle(id)
		if((event.type==="session.status"||event.type==="session.error"||event.type==="session.deleted")&&id)loop.busy(id)
		if(event.type==="session.deleted"&&id){knownSessions.delete(id);presence.remove(id)}
    },
  }
}

const workflowPlugin:PluginModule & {id:string}={id:"skynex-workflow",server:SkynexWorkflow}
export default workflowPlugin
