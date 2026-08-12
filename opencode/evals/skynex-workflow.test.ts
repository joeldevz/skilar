import test from "node:test"
import assert from "node:assert/strict"
import workflowPlugin, {
  SkynexWorkflow,
  createNotificationPoller,
  eventSession,
  notificationMessage,
  promptSession,
  resolveWorkflowBinary,
  startNotificationPolling,
  startSessionPresence,
} from "../plugins/skynex-workflow.ts"

test("exports the OpenCode v1 server-plugin descriptor", () => {
  assert.equal(workflowPlugin.id, "skynex-workflow")
  assert.equal(workflowPlugin.server, SkynexWorkflow)
})

test("workflow binary must be an explicit absolute path", () => {
  assert.equal(resolveWorkflowBinary("/opt/skynex/bin/skynex"), "/opt/skynex/bin/skynex")
  assert.throws(() => resolveWorkflowBinary(undefined), /SKYNEX_WORKFLOW_BINARY.*absolute/i)
  assert.throws(() => resolveWorkflowBinary("skynex"), /SKYNEX_WORKFLOW_BINARY.*absolute/i)
  assert.throws(() => resolveWorkflowBinary(" /opt/skynex/bin/skynex"), /SKYNEX_WORKFLOW_BINARY.*absolute/i)
})

test("poller recovers pending notifications, retries, and deduplicates concurrent ticks", async () => {
  let claims = 0, prompts = 0, acks = 0
  let failOnce = true
  const notice = { ID:"n1",WorkflowID:"wf",JobID:"j1",TerminalState:"candidate_frozen",JobState:"succeeded",Error:"",ClaimToken:"token",ClaimedBy:"session",CreatedAt:"" }
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
	const client={session:{
		get:async()=>({data:{agent:"workflow-orchestrator",model:{providerID:"openai",id:"gpt-5.6-terra",variant:"high"}}}),
		prompt:async(o:unknown)=>{options=o;return {error:{message:"rejected"}}},
	}}
	const poller=createNotificationPoller({claim:async()=>failed,notify:async()=>{},prompt:(id,n)=>promptSession(client,id,n),ack:async()=>{acks++},release:async()=>{releases++}})
	assert.equal(await poller.poll("s"),false)
	assert.equal((options as {throwOnError?:boolean}).throwOnError,true)
	assert.equal(acks,0);assert.equal(releases,1)
})

test("wake prompt preserves the persisted session agent, model, and variant",async()=>{
	const notice={ID:"n",WorkflowID:"wf",JobID:"j",TerminalState:"candidate_frozen",JobState:"succeeded",Error:"",ClaimToken:"t",ClaimedBy:"s",CreatedAt:""}
	let getOptions:unknown,promptOptions:unknown
	const client={session:{
		get:async(o:unknown)=>{getOptions=o;return {data:{agent:"workflow-orchestrator",model:{providerID:"openai",id:"gpt-5.6-terra",variant:"high"}}}},
		prompt:async(o:unknown)=>{promptOptions=o;return {}},
	}}
	await promptSession(client,"s",notice)
	assert.deepEqual(getOptions,{path:{id:"s"},throwOnError:true})
	const options=promptOptions as {path:{id:string};body:{agent:string;model:{providerID:string;modelID:string};variant?:string};throwOnError:boolean}
	assert.equal(options.path.id,"s")
	assert.equal(options.body.agent,"workflow-orchestrator")
	assert.deepEqual(options.body.model,{providerID:"openai",modelID:"gpt-5.6-terra"})
	assert.equal(options.body.variant,"high")
	assert.equal(options.throwOnError,true)
	const prompt=JSON.stringify(promptOptions).toLowerCase()
	assert.match(prompt,/status and inspect/)
	assert.match(prompt,/automatically continue the next safe managed action/)
	assert.match(prompt,/completed.*genuine blocker.*human gate.*destructive ambiguity.*retries.*exhausted/)
	assert.doesNotMatch(prompt,/then report the result/)
	assert.doesNotMatch(prompt,/do not review or deliver automatically/)
})

test("wake prompt fails closed when the persisted session identity is incomplete",async()=>{
	const notice={ID:"n",WorkflowID:"wf",JobID:"j",TerminalState:"candidate_frozen",JobState:"succeeded",Error:"",ClaimToken:"t",ClaimedBy:"s",CreatedAt:""}
	for(const data of [
		{model:{providerID:"openai",id:"gpt-5.6-terra"}},
		{agent:"workflow-orchestrator"},
		{agent:"workflow-orchestrator",model:{providerID:"openai"}},
		{agent:"workflow-orchestrator",model:{id:"gpt-5.6-terra"}},
	]){
		let prompts=0
		const client={session:{get:async()=>({data}),prompt:async()=>{prompts++;return {}}}}
		await assert.rejects(promptSession(client,"s",notice),/persisted agent\/model identity/i)
		assert.equal(prompts,0)
	}
})

test("a failed toast remains best-effort and does not block prompt or ack",async()=>{
	const notice={ID:"n",WorkflowID:"wf",JobID:"j",TerminalState:"candidate_frozen",JobState:"succeeded",Error:"",ClaimToken:"t",ClaimedBy:"s",CreatedAt:""}
	let prompts=0,acks=0,releases=0
	const poller=createNotificationPoller({
		claim:async()=>notice,
		notify:async()=>{throw new Error("headless TUI")},
		prompt:async()=>{prompts++},
		ack:async()=>{acks++},
		release:async()=>{releases++},
	})
	assert.equal(await poller.poll("s"),true)
	assert.equal(prompts,1);assert.equal(acks,1);assert.equal(releases,0)
})

test("a failed durable ack releases the claimed notification",async()=>{
	const notice={ID:"n",WorkflowID:"wf",JobID:"j",TerminalState:"candidate_frozen",JobState:"succeeded",Error:"",ClaimToken:"t",ClaimedBy:"s",CreatedAt:""}
	let prompts=0,acks=0,releases=0
	const poller=createNotificationPoller({
		claim:async()=>notice,
		notify:async()=>{},
		prompt:async()=>{prompts++},
		ack:async()=>{acks++;throw new Error("ack failed")},
		release:async()=>{releases++},
	})
	assert.equal(await poller.poll("s"),false)
	assert.equal(prompts,1);assert.equal(acks,1);assert.equal(releases,1)
})

test("session event identity supports OpenCode info envelopes",()=>{
	assert.equal(eventSession({properties:{info:{id:"nested-session"}}}),"nested-session")
	assert.equal(eventSession({properties:{sessionID:"direct-session",info:{id:"nested-session"}}}),"direct-session")
	assert.equal(eventSession({properties:{info:{}}}),"")
})

test("session presence heartbeats autonomously until stopped",async()=>{
	let beats=0
	const presence=startSessionPresence(async()=>{beats++},5);presence.add("session")
	await new Promise(resolve=>setTimeout(resolve,18));presence.stop()
	assert.ok(beats>=2,`beats=${beats}`)
})
