import test from "node:test"
import assert from "node:assert/strict"
import { createNotificationPoller, startNotificationPolling, startSessionPresence, notificationMessage, promptSession } from "../plugins/skynex-workflow.ts"

test("poller recovers pending notifications, retries, and deduplicates concurrent ticks", async () => {
  let claims = 0, prompts = 0, acks = 0
  let failOnce = true
  const notice = { ID:"n1",WorkflowID:"wf",JobID:"j1",TerminalState:"candidate_frozen",ClaimToken:"token",ClaimedBy:"session",CreatedAt:"" }
  const poller = createNotificationPoller({
    claim: async () => { claims++; return notice },
    notify: async () => {},
    prompt: async () => { prompts++; if (failOnce) { failOnce=false; throw new Error("retry") } },
    ack: async () => { acks++ },
  })
  await Promise.all([poller.poll("session"),poller.poll("session")])
  assert.equal(prompts,1);assert.equal(acks,0)
  await poller.poll("session")
  assert.equal(prompts,2);assert.equal(acks,1);assert.equal(claims,2)
})

test("autonomous loop polls an idle session recovered at plugin load",async()=>{
	let polls=0
	const loop=startNotificationPolling({poll:async()=>{polls++;return false}},async()=>["recovered-session"],5)
	await new Promise(resolve=>setTimeout(resolve,25));loop.stop()
	assert.ok(polls>=2,`polls=${polls}`)
})

test("failure message includes job error and prompt response.error is not acknowledged",async()=>{
	const failed={ID:"n",WorkflowID:"wf",JobID:"j",TerminalState:"executing",JobState:"failed",Error:"compile broke",ClaimToken:"t",ClaimedBy:"s",CreatedAt:""}
	assert.match(notificationMessage(failed),/failed.*compile broke/i)
	let options:unknown,acks=0,releases=0
	const client={session:{prompt:async(o:unknown)=>{options=o;return {error:{message:"rejected"}}}}}
	const poller=createNotificationPoller({claim:async()=>failed,notify:async()=>{},prompt:(id,n)=>promptSession(client,id,n),ack:async()=>{acks++},release:async()=>{releases++}})
	assert.equal(await poller.poll("s"),false)
	assert.equal((options as {throwOnError?:boolean}).throwOnError,true)
	assert.equal(acks,0);assert.equal(releases,1)
})

test("session presence heartbeats autonomously until stopped",async()=>{
	let beats=0
	const presence=startSessionPresence(async()=>{beats++},5);presence.add("session")
	await new Promise(resolve=>setTimeout(resolve,18));presence.stop()
	assert.ok(beats>=2,`beats=${beats}`)
})
