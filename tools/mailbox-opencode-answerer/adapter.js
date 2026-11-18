// mailbox-opencode-answerer
//
// EXTERNAL answerer for sub2api's mailbox relay mode (mailbox_handler.go).
// Watches the file spool, uses a REAL OpenCode model (via session.prompt),
// and writes answers carrying: text + reasoning + semantic tool intents.
//
// Protocol (backend/internal/handler/mailbox_handler.go):
//   pending/<qid>.json --atomic rename--> claimed/<qid>.json
//   read mailboxQuestion.prompt (plain text; instructions/tools stripped)
//   OpenCode session.prompt + read assistant message content parts:
//     - "text" parts      -> answer.text
//     - "reasoning" parts -> answer.reasoning
//     - "tool" parts      -> answer.tool_calls (semantic intents)
//   answers/<qid>.json -> gateway reassembles -> client receives response
//
// OpenCode tool name -> mailbox semantic intent (gateway maps intent back to
// whatever tools the original client declared):
//   read / read_file / cat          -> read_file
//   list / list_dir / ls            -> list_dir
//   grep / search / search_text     -> search_text
//   shell / exec / bash / run       -> run_command
//   write / write_file              -> write_file
//   edit / patch / edit_file        -> edit_file
//
// MAILBOX_ANSWER_MODE:
//   prompt (default) - session.prompt + message read (real reasoning + tools)
//   generate         - stateless session.generate (text only, no reasoning/tools)
//   cli              - `opencode run --standalone --format json`

import fs from "node:fs"
import path from "node:path"
import { execFile } from "node:child_process"
import { promisify } from "node:util"

const execFileP = promisify(execFile)
const SPOOL = process.argv[2] || process.env.MAILBOX_SPOOL_DIR || "/tmp/mailbox-spool"
const POLL_MS = Number(process.argv[3] || 500)
const MODE = process.env.MAILBOX_ANSWER_MODE || "prompt"
const AGENT = process.env.MAILBOX_OPENCODE_AGENT || "mailbox-relay"
const OPENCODE_CONFIG = process.env.OPENCODE_CONFIG || ""

const BASE = (process.env.OPENCODE_BASE_URL || "http://127.0.0.1:4097").replace(/\/$/, "")
const PASS = process.env.OPENCODE_SERVER_PASSWORD || ""
const PROVIDER = process.env.OPENCODE_PROVIDER || "opencode"
const OPENTIMEOUT_MS = Number(process.env.MAILBOX_ANSWER_TIMEOUT_MS || 110000)
const WAIT_TIMEOUT_MS = Number(process.env.MAILBOX_MODEL_WAIT_MS || 90000)

const DEFAULT_FREE_MODELS = [
  "big-pickle", "deepseek-v4.1-flash-free", "hy4-flash-free", "iceman-free",
  "inkling-2-small-free", "longcat-2.5-free", "flint-40b-free",
  "nemotron-3.5-super-vl-free", "beam-free", "mimo-v2.6-flash-free",
  "nemotron-3.5-lightning-free",
]
const MODEL_POOL = process.env.OPENCODE_MODELS
  ? process.env.OPENCODE_MODELS.split(",").map((s) => s.trim()).filter(Boolean)
  : process.env.OPENCODE_MODEL ? [process.env.OPENCODE_MODEL.trim()] : DEFAULT_FREE_MODELS

const ALLOWED_INTENTS = new Set(["read_file", "list_dir", "search_text", "run_command", "write_file", "edit_file"])

// Brain-mode instruction: the model must NOT execute tools. It expresses
// the tool call as a semantic intent in the JSON instead; the original
// client executes it after the gateway maps the intent back to its tool.
const STRICT_JSON = `You are a mailbox relay answerer. The message after "---" is a plain-text conversation transcript.
Do NOT call any tools. If the task needs a tool, express it as a semantic intent in the JSON tool_calls field.
Reply with EXACTLY one JSON object and nothing else, no markdown fences, no prose:
{"text":"<your answer>","reasoning":"<brief reasoning>","tool_calls":[{"intent":"<intent>","args":{...}}]}
Allowed intents and args:
- read_file {path, start_line?, end_line?}
- list_dir {path?}
- search_text {pattern, path?, is_regex?}
- run_command {command, cwd?}
- write_file {path, content}
- edit_file {path, old_text, new_text}
If no tool is needed, use "tool_calls":[].
---
`

const d = (name) => path.join(SPOOL, name)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

function claimOne() {
  const names = fs.readdirSync(d("pending")).filter((n) => n.endsWith(".json"))
  if (!names.length) return null
  const name = names[0]
  fs.renameSync(d(`pending/${name}`), d(`claimed/${name}`))
  return { qid: name.replace(/\.json$/, ""), name }
}

function httpHeaders() {
  const h = { "Content-Type": "application/json" }
  if (PASS) h["Authorization"] = "Basic " + Buffer.from("opencode:" + PASS).toString("base64")
  return h
}

// one session per model
const sessions = new Map()
let rr = 0
async function getSession(model) {
  if (sessions.has(model)) return sessions.get(model)
  const r = await fetch(`${BASE}/api/session`, {
    method: "POST", headers: httpHeaders(),
    body: JSON.stringify({ agent: "build", model: { id: model, providerID: PROVIDER } }),
  })
  const j = await r.json()
  if (!r.ok || !j.data?.id) throw new Error(`create session ${model} failed: ` + JSON.stringify(j).slice(0, 200))
  sessions.set(model, j.data.id)
  console.log(`[answerer] session for ${PROVIDER}/${model} = ${j.data.id}`)
  return j.data.id
}

// map an OpenCode tool name to a mailbox semantic intent
function toolToIntent(name) {
  const n = (name || "").toLowerCase()
  if (/^(read|read_file|cat|view_file|file_read|open_file)$/.test(n)) return "read_file"
  if (/^(list|list_dir|ls|list_files|dir)$/.test(n)) return "list_dir"
  if (/^(grep|search|search_text|code_search|find_text)$/.test(n)) return "search_text"
  if (/^(shell|exec|execute|bash|run|run_command|terminal|execute_command|exec_command)$/.test(n)) return "run_command"
  if (/^(write|write_file)$/.test(n)) return "write_file"
  if (/^(edit|patch|edit_file)$/.test(n)) return "edit_file"
  return null
}

// map an OpenCode tool input to semantic intent args
function toolInputToArgs(intent, input) {
  input = input || {}
  switch (intent) {
    case "read_file": return { path: input.path ?? input.filePath ?? "" }
    case "list_dir": return { path: input.path ?? "." }
    case "search_text": return { pattern: input.pattern ?? input.query ?? "", path: input.path }
    case "run_command": return { command: input.command ?? input.cmd ?? "", cwd: input.cwd ?? input.workdir }
    case "write_file": return { path: input.path ?? "", content: input.content ?? "" }
    case "edit_file": return { path: input.path ?? "", old_text: input.oldString ?? input.old_text ?? "", new_text: input.newString ?? input.new_text ?? "" }
    default: return {}
  }
}

// Extract the newest assistant message's text + reasoning after a prompt.
async function readAssistantContent(sessionID) {
  const r = await fetch(`${BASE}/api/session/${sessionID}/message?limit=20&order=desc`, {
    headers: httpHeaders(), signal: AbortSignal.timeout(15000),
  })
  const j = await r.json()
  if (!r.ok || !j.data) throw new Error("message list failed: " + JSON.stringify(j).slice(0, 200))
  const assistants = j.data.filter((m) => m.type === "assistant")
  if (!assistants.length) throw new Error("no assistant message yet")
  let text = "", reasoning = ""
  for (const m of assistants) {
    for (const c of m.content || []) {
      if (c.type === "text" && !text) text = c.text || ""
      else if (c.type === "reasoning" && !reasoning) reasoning = c.text || ""
    }
  }
  return { text: text.trim(), reasoning: reasoning.trim() }
}

// Parse the strict JSON the model was instructed to emit.
function parseBrainJSON(raw, fallbackReasoning) {
  let obj
  try { obj = JSON.parse(raw) } catch {
    const m = raw.match(/\{[\s\S]*\}/)
    if (!m) throw new Error("answer is not JSON: " + raw.slice(0, 200))
    obj = JSON.parse(m[0])
  }
  const text = typeof obj.text === "string" ? obj.text : raw
  const reasoning = typeof obj.reasoning === "string" && obj.reasoning ? obj.reasoning : (fallbackReasoning || "")
  const tool_calls = (Array.isArray(obj.tool_calls) ? obj.tool_calls : []).filter(
    (c) => c && ALLOWED_INTENTS.has(c.intent),
  )
  return { text, reasoning, tool_calls }
}

async function tryPromptModel(model, transcript) {
  const sid = await getSession(model)
  // Brain mode: instruct the model NOT to execute tools; express intents in JSON.
  const pr = await fetch(`${BASE}/api/session/${sid}/prompt`, {
    method: "POST", headers: httpHeaders(),
    body: JSON.stringify({ text: STRICT_JSON + transcript }),
    signal: AbortSignal.timeout(15000),
  })
  if (!pr.ok) { sessions.delete(model); throw new Error(`${model}: prompt failed ${pr.status}`) }
  try {
    await fetch(`${BASE}/api/experimental/session/${sid}/wait`, {
      method: "POST", headers: httpHeaders(), body: "{}",
      signal: AbortSignal.timeout(WAIT_TIMEOUT_MS),
    })
  } catch { /* wait may time out; still try to read */ }
  const out = await readAssistantContent(sid)
  const parsed = parseBrainJSON(out.text, out.reasoning)
  console.log(`[answerer] model ${model} -> text=${parsed.text.length} reasoning=${parsed.reasoning.length} tools=${parsed.tool_calls.length}`)
  return parsed
}

async function askPrompt(prompt) {
  const n = MODEL_POOL.length
  let lastErr
  for (let k = 0; k < n; k++) {
    const model = MODEL_POOL[(rr + k) % n]
    try {
      const out = await tryPromptModel(model, prompt)
      if (!out.text && !out.tool_calls.length) throw new Error("empty answer")
      rr = (rr + k + 1) % n
      return { ...out, model }
    } catch (e) {
      lastErr = e
      console.warn(`[answerer] model ${model} failed: ${e.message}`)
    }
  }
  throw new Error("all models failed; last=" + (lastErr?.message || "unknown"))
}

async function askGenerate(prompt) {
  const n = MODEL_POOL.length
  let lastErr
  for (let k = 0; k < n; k++) {
    const model = MODEL_POOL[(rr + k) % n]
    try {
      const sid = await getSession(model)
      const r = await fetch(`${BASE}/api/session/${sid}/generate`, {
        method: "POST", headers: httpHeaders(),
        body: JSON.stringify({ prompt: SYSTEM_PROMPT + "\n---\n" + prompt }),
        signal: AbortSignal.timeout(OPENTIMEOUT_MS),
      })
      const j = await r.json()
      if (!r.ok || j._tag || !j.data?.text) throw new Error(`${model}: ` + (j.message || j._tag || "empty"))
      rr = (rr + k + 1) % n
      return { text: j.data.text.trim(), reasoning: "", tool_calls: [], model }
    } catch (e) { lastErr = e; console.warn(`[answerer] model ${model} failed: ${e.message}`) }
  }
  throw new Error("all models failed; last=" + (lastErr?.message || "unknown"))
}

async function askCli(prompt) {
  const env = { ...process.env }
  if (OPENCODE_CONFIG) {
    env.OPENCODE_CONFIG = OPENCODE_CONFIG
    delete env.OPENCODE_CONFIG_DIR
    env.OPENCODE_CONFIG_CONTENT = ""
  }
  const { stdout } = await execFileP("opencode",
    ["run", "--standalone", "--agent", AGENT, "--format", "json", prompt],
    { env, maxBuffer: 16 * 1024 * 1024, timeout: OPENTIMEOUT_MS })
  let text = ""
  for (const line of stdout.split("\n")) {
    try { const ev = JSON.parse(line); if (ev.type === "text" && ev.part?.text) text += ev.part.text } catch {}
  }
  if (!text.trim()) throw new Error("opencode produced no text")
  return { text: text.trim(), reasoning: "", tool_calls: [], model: "cli" }
}

function writeAnswer(ans) {
  const final = d(`answers/${ans.id}.json`)
  const tmp = final + ".tmp"
  fs.writeFileSync(tmp, JSON.stringify(ans))
  fs.renameSync(tmp, final)
}

async function main() {
  for (const dir of ["pending", "claimed", "answers", "sessions", "done"]) fs.mkdirSync(d(dir), { recursive: true })
  console.log(`[answerer] spool=${SPOOL} poll=${POLL_MS}ms mode=${MODE}`)
  console.log(`[answerer] model pool (${MODEL_POOL.length}): ${MODEL_POOL.join(", ")}`)
  if (MODE === "prompt" && !PASS) console.warn("[answerer] WARNING: no OPENCODE_SERVER_PASSWORD set")
  while (true) {
    const job = claimOne()
    if (!job) { await sleep(POLL_MS); continue }
    console.log(`[answerer] claimed ${job.qid}`)
    try {
      const q = JSON.parse(fs.readFileSync(d(`claimed/${job.name}`), "utf8"))
      const out = MODE === "prompt" ? await askPrompt(q.prompt || q.latest_user || "")
        : MODE === "generate" ? await askGenerate(q.prompt || q.latest_user || "")
        : await askCli(q.prompt || q.latest_user || "")
      const ans = {
        id: job.qid,
        text: out.text,
        reasoning: out.reasoning,
        tool_calls: (out.tool_calls || []).filter((c) => ALLOWED_INTENTS.has(c.intent)),
        answered_by: `opencode:${MODE}:${out.model}`,
        answered_at: new Date().toISOString(),
      }
      if (!ans.text && !ans.tool_calls.length) throw new Error("empty answer")
      writeAnswer(ans)
      console.log(`[answerer] answered ${job.qid} by ${ans.answered_by}: ${ans.text.slice(0, 80)}`)
    } catch (err) {
      console.error(`[answerer] failed ${job.qid}: ${err.message}`)
      try { fs.renameSync(d(`claimed/${job.name}`), d(`pending/${job.name}`)) } catch {}
    }
  }
}
main()
