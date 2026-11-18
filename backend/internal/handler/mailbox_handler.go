package handler

// Mailbox relay mode (prototype, mailbox-relay PLAN §12).
//
// An API key whose group platform is "mailbox" is answered from a file
// spool instead of a live upstream account. The relay side implemented
// here does three things and nothing else:
//
//  1. Strip: instructions/system/developer content and tool definitions
//     are stored in a server-side session record and never enter the
//     question that leaves this process.
//  2. Queue: the plain-text prompt (history rendered as text) is written
//     to spool/pending as a file. An external answerer claims it by
//     atomic rename (pending -> claimed), thinks off-channel, and drops
//     the answer into spool/answers.
//  3. Assemble: the client connection is held open (SSE keepalive
//     comments while waiting, surfaced to the client as an event-level
//     heartbeat — a reasoning item whose summary grows on the Responses
//     side, reasoning_content chunks on the Chat side — because clients
//     that count only output items as activity otherwise sit at
//     "Thinking 0s" for the whole wait); once the answer file appears,
//     the reply is assembled back into the original API shape and
//     streamed out.
//     Tool calls take the outbound half of the same loop: the answerer
//     writes semantic intents (never client tool names — tool
//     definitions stay in the session record, PLAN §9), and assembly
//     maps each intent onto a tool the client actually declared,
//     shaping the arguments to that tool's schema. An intent that maps
//     onto nothing declared is not dropped: it is appended to the
//     answer text in plain sight and recorded in the session record.
//
// Existing gateway modes are untouched: the routes layer only diverts
// requests here when the key's group platform is "mailbox" and the
// mailbox config section is enabled.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const mailboxPlatform = service.PlatformMailbox

// MailboxModelID is the single model a mailbox group advertises. The
// answerer behind the spool is model-agnostic, but the service is
// offered under exactly this name, so /v1/models for a mailbox group
// returns just this one entry instead of falling back to a stock
// catalog (see GatewayHandler.Models).
const MailboxModelID = "meta/muse-spark-1.3-contributor"

// MailboxHandler serves /v1/responses and /v1/chat/completions for
// mailbox-platform groups.
type MailboxHandler struct {
	cfg *config.Config
	mu  sync.Mutex // serializes session record read-modify-write in this process
}

func NewMailboxHandler(cfg *config.Config) *MailboxHandler {
	h := &MailboxHandler{cfg: cfg}
	_ = h.ensureDirs()
	return h
}

// ---------- configuration ----------

func (h *MailboxHandler) spoolDir() string {
	if d := strings.TrimSpace(h.cfg.Mailbox.SpoolDir); d != "" {
		return d
	}
	return "mailbox-spool"
}

func (h *MailboxHandler) dir(name string) string { return filepath.Join(h.spoolDir(), name) }

func (h *MailboxHandler) ensureDirs() error {
	for _, d := range []string{"pending", "claimed", "answers", "sessions", "done"} {
		if err := os.MkdirAll(h.dir(d), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (h *MailboxHandler) pendingTimeout() time.Duration {
	if h.cfg.Mailbox.PendingTimeoutSeconds > 0 {
		return time.Duration(h.cfg.Mailbox.PendingTimeoutSeconds) * time.Second
	}
	return 600 * time.Second
}

func (h *MailboxHandler) keepaliveInterval() time.Duration {
	if h.cfg.Mailbox.KeepaliveSeconds > 0 {
		return time.Duration(h.cfg.Mailbox.KeepaliveSeconds) * time.Second
	}
	return 15 * time.Second
}

func (h *MailboxHandler) pollInterval() time.Duration {
	if h.cfg.Mailbox.PollIntervalMillis > 0 {
		return time.Duration(h.cfg.Mailbox.PollIntervalMillis) * time.Millisecond
	}
	return 500 * time.Millisecond
}

// ---------- spool records ----------

// mailboxTurn is one rendered conversation turn. Tool interactions are
// stored as Role "tool" with a self-describing Text.
type mailboxTurn struct {
	Role string `json:"role"` // user | assistant | tool
	Text string `json:"text"`
}

// mailboxSession is the relay-side session record. Instructions and
// tools live ONLY here (server-side); they are never written into a
// question file.
type mailboxSession struct {
	ID             string                `json:"id"`
	ParentID       string                `json:"parent_id,omitempty"`
	Model          string                `json:"model"`
	Instructions   string                `json:"instructions,omitempty"`
	Tools          json.RawMessage       `json:"tools,omitempty"`
	Turns          []mailboxTurn         `json:"turns"`
	APIKeyID       int64                 `json:"api_key_id"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
	LastQuestionID string                `json:"last_question_id,omitempty"`
	LastStatus     string                `json:"last_status,omitempty"` // pending | answered | timeout | client_gone
	Rejected       []mailboxRejectedCall `json:"rejected,omitempty"`
}

// mailboxRejectedCall is the session-record trace of one answerer tool
// intent that could not be mapped onto any tool the client declared
// for this session (PLAN §9 assembly validation).
type mailboxRejectedCall struct {
	QuestionID string    `json:"question_id"`
	Intent     string    `json:"intent"`
	Args       string    `json:"args,omitempty"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
}

// mailboxQuestion is the only artifact an answerer ever sees: plain
// prompt text, no instructions, no tool definitions.
type mailboxQuestion struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"session_id"`
	ParentID   string    `json:"parent_id,omitempty"`
	Model      string    `json:"model"`
	CreatedAt  time.Time `json:"created_at"`
	Prompt     string    `json:"prompt"`      // full rendered transcript, plain text
	LatestUser string    `json:"latest_user"` // newest user ask, plain text
	APIKeyID   int64     `json:"api_key_id"`
}

// mailboxToolIntent is one semantic tool intent in an answer file.
// The answerer names a semantic verb and natural arguments; it never
// sees (and never names) the client's actual tools. The relay maps the
// intent onto a declared tool at assembly time (see resolveAnswer).
type mailboxToolIntent struct {
	Intent string          `json:"intent"` // read_file | list_dir | search_text | run_command | write_file | edit_file
	Args   json.RawMessage `json:"args,omitempty"`
}

type mailboxAnswer struct {
	ID         string              `json:"id"` // == question id
	Text       string              `json:"text"`
	ToolCalls  []mailboxToolIntent `json:"tool_calls,omitempty"`
	AnsweredBy string              `json:"answered_by,omitempty"`
	AnsweredAt time.Time           `json:"answered_at"`
}

// mailboxDone archives a finished question for audit.
type mailboxDone struct {
	Question   mailboxQuestion `json:"question"`
	Answer     *mailboxAnswer  `json:"answer,omitempty"`
	Status     string          `json:"status"` // answered | timeout | client_gone
	WaitMillis int64           `json:"wait_millis"`
	FinishedAt time.Time       `json:"finished_at"`
}

// ---------- file helpers ----------

func mailboxNewID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func validMailboxID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for _, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

func (h *MailboxHandler) sessionPath(id string) string {
	return filepath.Join(h.dir("sessions"), id+".json")
}

func (h *MailboxHandler) loadSession(id string) (*mailboxSession, error) {
	data, err := os.ReadFile(h.sessionPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s mailboxSession
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (h *MailboxHandler) saveSession(s *mailboxSession) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(h.sessionPath(s.ID), data)
}

// ---------- extraction: strip + render ----------

// mailboxExtract is what one request contributes: rendered turns plus
// the material stripped out of the question path.
type mailboxExtract struct {
	Turns           []mailboxTurn
	LatestUser      string
	Instructions    string
	HasInstructions bool
	Tools           json.RawMessage
}

func mailboxContentText(content gjson.Result) string {
	switch {
	case content.Type == gjson.String:
		return content.String()
	case content.IsArray():
		var sb strings.Builder
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "input_text", "output_text", "text":
				sb.WriteString(part.Get("text").String())
			case "input_image", "image":
				sb.WriteString("[image]")
			}
		}
		return sb.String()
	}
	return ""
}

// extractResponses parses a Responses API request body.
func extractResponses(body []byte) mailboxExtract {
	var ex mailboxExtract
	if ins := gjson.GetBytes(body, "instructions"); ins.Exists() && ins.Type == gjson.String {
		ex.Instructions = ins.String()
		ex.HasInstructions = true
	}
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		ex.Tools = json.RawMessage(tools.Raw)
	}
	input := gjson.GetBytes(body, "input")
	switch {
	case input.Type == gjson.String:
		ex.Turns = append(ex.Turns, mailboxTurn{Role: "user", Text: input.String()})
	case input.IsArray():
		for _, item := range input.Array() {
			// Message items are typed ("type":"message") in the strict
			// Responses shape, but some clients send bare role/content
			// items; treat an untyped item carrying a role as a message.
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").String() != "" {
				itemType = "message"
			}
			switch itemType {
			case "message":
				role := item.Get("role").String()
				text := mailboxContentText(item.Get("content"))
				switch role {
				case "user":
					ex.Turns = append(ex.Turns, mailboxTurn{Role: "user", Text: text})
				case "assistant":
					ex.Turns = append(ex.Turns, mailboxTurn{Role: "assistant", Text: text})
				case "system", "developer":
					if ex.HasInstructions && ex.Instructions != "" {
						ex.Instructions += "\n"
					}
					ex.Instructions += text
					ex.HasInstructions = true
				}
			case "function_call":
				name := item.Get("name").String()
				args := item.Get("arguments").String()
				ex.Turns = append(ex.Turns, mailboxTurn{Role: "tool",
					Text: fmt.Sprintf("Tool call: %s(%s)", name, args)})
			case "function_call_output":
				out := mailboxContentText(item.Get("output"))
				if out == "" {
					out = item.Get("output").String()
				}
				ex.Turns = append(ex.Turns, mailboxTurn{Role: "tool",
					Text: fmt.Sprintf("Tool result: %s", out)})
			default:
				// reasoning / item_reference / etc.: not part of the plain-text prompt.
			}
		}
	}
	for i := len(ex.Turns) - 1; i >= 0; i-- {
		if ex.Turns[i].Role == "user" {
			ex.LatestUser = ex.Turns[i].Text
			break
		}
	}
	return ex
}

// extractChat parses a Chat Completions request body.
func extractChat(body []byte) mailboxExtract {
	var ex mailboxExtract
	msgs := gjson.GetBytes(body, "messages")
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		ex.Tools = json.RawMessage(tools.Raw)
	}
	if msgs.IsArray() {
		for _, m := range msgs.Array() {
			role := m.Get("role").String()
			switch role {
			case "system", "developer":
				text := mailboxContentText(m.Get("content"))
				if ex.HasInstructions && ex.Instructions != "" {
					ex.Instructions += "\n"
				}
				ex.Instructions += text
				ex.HasInstructions = true
			case "user":
				ex.Turns = append(ex.Turns, mailboxTurn{Role: "user", Text: mailboxContentText(m.Get("content"))})
			case "assistant":
				if tc := m.Get("tool_calls"); tc.IsArray() {
					for _, call := range tc.Array() {
						name := call.Get("function.name").String()
						args := call.Get("function.arguments").String()
						ex.Turns = append(ex.Turns, mailboxTurn{Role: "tool",
							Text: fmt.Sprintf("Tool call: %s(%s)", name, args)})
					}
				}
				if text := mailboxContentText(m.Get("content")); text != "" {
					ex.Turns = append(ex.Turns, mailboxTurn{Role: "assistant", Text: text})
				}
			case "tool":
				ex.Turns = append(ex.Turns, mailboxTurn{Role: "tool",
					Text: fmt.Sprintf("Tool result: %s", mailboxContentText(m.Get("content")))})
			}
		}
	}
	for i := len(ex.Turns) - 1; i >= 0; i-- {
		if ex.Turns[i].Role == "user" {
			ex.LatestUser = ex.Turns[i].Text
			break
		}
	}
	return ex
}

func renderTranscript(turns []mailboxTurn) string {
	var sb strings.Builder
	for _, t := range turns {
		switch t.Role {
		case "user":
			sb.WriteString("User: ")
			sb.WriteString(t.Text)
		case "assistant":
			sb.WriteString("Assistant: ")
			sb.WriteString(t.Text)
		default: // tool turns are self-describing
			sb.WriteString(t.Text)
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func turnsEqual(a, b []mailboxTurn) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mergeTurns combines a session's stored turns with the turns carried
// by the current request. Clients chaining via previous_response_id
// send only the new items; clients resending full history are detected
// by the prefix check so nothing is duplicated.
func mergeTurns(prior, cur []mailboxTurn) []mailboxTurn {
	if len(prior) == 0 {
		return cur
	}
	if len(cur) >= len(prior) && turnsEqual(cur[:len(prior)], prior) {
		return cur
	}
	merged := make([]mailboxTurn, 0, len(prior)+len(cur))
	merged = append(merged, prior...)
	merged = append(merged, cur...)
	return merged
}

func estimateTokens(text string) int {
	n := (len([]rune(text)) + 3) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// ---------- tool intent mapping (outbound half of the tool loop) ----------
//
// The answerer writes semantic intents; the client declared concrete
// tools. Mapping happens here, at assembly, against the session's
// stored declarations — in both directions the conversion lives in the
// relay (inbound function_call/function_call_output are rendered to
// plain transcript text by the extract functions above).
//
// Intent vocabulary (answerer-facing, fixed):
//
//	read_file    {path, start_line?, end_line?}
//	list_dir     {path?}                     (default ".")
//	search_text  {pattern, path?, is_regex?}
//	run_command  {command, cwd?}
//	write_file   {path, content}
//	edit_file    {path, old_text, new_text}
//
// Mapping tiers, tried in order per intent (first tier with a
// candidate wins; if that candidate's arguments cannot be shaped to
// its schema the intent is rejected, loudly — no silent fallthrough):
//
//  1. action tools: a declared tool with an "action" parameter whose
//     action enum contains the intent's action name (grep_search for
//     search_text), or whose name+description documents it. Shaped as
//     {action: <name>, ...natural args}.
//  2. name aliases: a tool literally named like the intent
//     (read_file/read/view_file, grep_search/grep, shell/exec, ...).
//  3. shell tools: a tool whose only shape is a "command" parameter
//     (and no "action" parameter). The intent is compiled to a shell
//     command line (cat/sed, ls, grep, or the command itself).
//     write_file/edit_file deliberately never take this tier.
//  4. property cover: a tool whose parameters directly cover the
//     intent's natural arguments.
//
// Shaping keeps only properties the target tool declares and then
// requires every schema-required property to be present. run_command
// has one extra form between tiers 2 and 3: an action tool documenting
// a run_host action is shaped as
// {action:"run_host", exe:"bash", args:["-lc", command]}.

// mailboxDeclaredTool is one client-declared tool, normalized from
// either the Responses shape ({name, description, parameters}) or the
// Chat shape ({function: {name, description, parameters}}).
type mailboxDeclaredTool struct {
	name       string
	matchText  string // lowercased name + description, for matching
	props      map[string]gjson.Result
	required   []string
	actionEnum []string
}

func parseDeclaredTools(raw json.RawMessage) []mailboxDeclaredTool {
	if len(raw) == 0 {
		return nil
	}
	arr := gjson.ParseBytes(raw)
	if !arr.IsArray() {
		return nil
	}
	var out []mailboxDeclaredTool
	for _, t := range arr.Array() {
		name := t.Get("name").String()
		desc := t.Get("description").String()
		params := t.Get("parameters")
		if fn := t.Get("function"); name == "" && fn.Exists() {
			name = fn.Get("name").String()
			desc = fn.Get("description").String()
			params = fn.Get("parameters")
		}
		if name == "" {
			continue
		}
		tool := mailboxDeclaredTool{
			name:      name,
			matchText: strings.ToLower(name + " " + desc),
			props:     map[string]gjson.Result{},
		}
		if props := params.Get("properties"); props.Exists() && props.IsObject() {
			props.ForEach(func(k, v gjson.Result) bool {
				tool.props[k.String()] = v
				return true
			})
		}
		if req := params.Get("required"); req.IsArray() {
			for _, r := range req.Array() {
				tool.required = append(tool.required, r.String())
			}
		}
		if act, ok := tool.props["action"]; ok {
			if en := act.Get("enum"); en.IsArray() {
				for _, e := range en.Array() {
					tool.actionEnum = append(tool.actionEnum, e.String())
				}
			}
		}
		out = append(out, tool)
	}
	return out
}

func (t mailboxDeclaredTool) hasProp(p string) bool {
	_, ok := t.props[p]
	return ok
}

func (t mailboxDeclaredTool) isActionTool() bool { return t.hasProp("action") }

// isShellTool reports whether the tool's only calling shape is a
// command string/array. Action tools also carrying a "command"
// property are excluded: they are driven through their action form.
func (t mailboxDeclaredTool) isShellTool() bool {
	return t.hasProp("command") && !t.isActionTool()
}

func (t mailboxDeclaredTool) mentions(s string) bool {
	return strings.Contains(t.matchText, strings.ToLower(s))
}

func (t mailboxDeclaredTool) enumHas(s string) bool {
	for _, e := range t.actionEnum {
		if e == s {
			return true
		}
	}
	return false
}

// shapeArgs keeps only the properties tool declares and verifies that
// every schema-required property ended up filled. The second return
// value is the rejection reason ("" = shaped fine).
func shapeArgs(tool mailboxDeclaredTool, picked map[string]json.RawMessage) (map[string]json.RawMessage, string) {
	out := map[string]json.RawMessage{}
	for k, v := range picked {
		if tool.hasProp(k) {
			out[k] = v
		}
	}
	for _, r := range tool.required {
		if _, ok := out[r]; !ok {
			return nil, fmt.Sprintf("required parameter %q of tool %q cannot be filled from the intent", r, tool.name)
		}
	}
	return out, ""
}

func pickArgs(args gjson.Result, keys ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, k := range keys {
		if v := args.Get(k); v.Exists() {
			out[k] = json.RawMessage(v.Raw)
		}
	}
	return out
}

func rawString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellShaped builds the argument object for a shell tool: the command
// as an array (["bash","-lc",line]) when the schema types it as an
// array, else as a plain string; cwd lands in cwd/workdir if declared.
func shellShaped(tool mailboxDeclaredTool, cmdline string, args gjson.Result) (map[string]json.RawMessage, string) {
	picked := map[string]json.RawMessage{}
	if tool.props["command"].Get("type").String() == "array" {
		b, _ := json.Marshal([]string{"bash", "-lc", cmdline})
		picked["command"] = b
	} else {
		picked["command"] = rawString(cmdline)
	}
	if cwd := args.Get("cwd"); cwd.Exists() {
		for _, k := range []string{"cwd", "workdir", "working_dir"} {
			if tool.hasProp(k) {
				picked[k] = json.RawMessage(cwd.Raw)
				break
			}
		}
	}
	return shapeArgs(tool, picked)
}

func pickActionTool(tools []mailboxDeclaredTool, action string) (mailboxDeclaredTool, bool) {
	for _, t := range tools {
		if t.isActionTool() && t.enumHas(action) {
			return t, true
		}
	}
	for _, t := range tools {
		if t.isActionTool() && t.mentions(action) {
			return t, true
		}
	}
	return mailboxDeclaredTool{}, false
}

func pickNamedTool(tools []mailboxDeclaredTool, names ...string) (mailboxDeclaredTool, bool) {
	for _, t := range tools {
		for _, n := range names {
			if t.name == n {
				return t, true
			}
		}
	}
	return mailboxDeclaredTool{}, false
}

func pickShellTool(tools []mailboxDeclaredTool) (mailboxDeclaredTool, bool) {
	for _, t := range tools {
		if t.isShellTool() {
			return t, true
		}
	}
	return mailboxDeclaredTool{}, false
}

func mapFail(reason string) (mailboxDeclaredTool, map[string]json.RawMessage, string) {
	return mailboxDeclaredTool{}, nil, reason
}

// mapIntent maps one semantic intent onto a declared tool and shapes
// its arguments. A non-empty third return value is the rejection
// reason; nothing is assembled in that case.
func mapIntent(tools []mailboxDeclaredTool, intent string, args gjson.Result) (mailboxDeclaredTool, map[string]json.RawMessage, string) {
	argStr := func(k string) string { return args.Get(k).String() }
	withAction := func(action string, picked map[string]json.RawMessage) map[string]json.RawMessage {
		picked["action"] = rawString(action)
		return picked
	}

	switch intent {
	case "read_file":
		path := argStr("path")
		if path == "" {
			return mapFail("intent read_file requires args.path")
		}
		if t, ok := pickActionTool(tools, "read_file"); ok {
			shaped, why := shapeArgs(t, withAction("read_file", pickArgs(args, "path", "start_line", "end_line")))
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		if t, ok := pickNamedTool(tools, "read_file", "read", "view_file", "file_read", "open_file", "cat"); ok && t.hasProp("path") {
			shaped, why := shapeArgs(t, pickArgs(args, "path", "start_line", "end_line"))
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		if t, ok := pickShellTool(tools); ok {
			cmdline := "cat -- " + shellQuote(path)
			if s, e := argStr("start_line"), argStr("end_line"); s != "" && e != "" {
				cmdline = fmt.Sprintf("sed -n '%s,%sp' -- %s", s, e, shellQuote(path))
			}
			shaped, why := shellShaped(t, cmdline, args)
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		for _, t := range tools {
			if !t.isActionTool() && t.hasProp("path") {
				shaped, why := shapeArgs(t, pickArgs(args, "path", "start_line", "end_line"))
				if why != "" {
					return mapFail(why)
				}
				return t, shaped, ""
			}
		}
		return mapFail("no tool declared in this session can read a file")

	case "list_dir":
		path := argStr("path")
		if path == "" {
			path = "."
		}
		if t, ok := pickActionTool(tools, "list_dir"); ok {
			picked := pickArgs(args, "path", "depth", "max_entries")
			if _, has := picked["path"]; !has {
				picked["path"] = rawString(path)
			}
			shaped, why := shapeArgs(t, withAction("list_dir", picked))
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		if t, ok := pickNamedTool(tools, "list_dir", "list_files", "ls", "list", "dir"); ok {
			shaped, why := shapeArgs(t, pickArgs(args, "path"))
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		if t, ok := pickShellTool(tools); ok {
			shaped, why := shellShaped(t, "ls -la -- "+shellQuote(path), args)
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		for _, t := range tools {
			if t.isActionTool() {
				continue
			}
			picked := map[string]json.RawMessage{}
			switch {
			case t.hasProp("path"):
				picked["path"] = rawString(path)
			case t.hasProp("dir"):
				picked["dir"] = rawString(path)
			default:
				continue
			}
			shaped, why := shapeArgs(t, picked)
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		return mapFail("no tool declared in this session can list a directory")

	case "search_text":
		pattern := argStr("pattern")
		if pattern == "" {
			pattern = argStr("query")
		}
		if pattern == "" {
			return mapFail("intent search_text requires args.pattern")
		}
		path := argStr("path")
		if t, ok := pickActionTool(tools, "grep_search"); ok {
			picked := pickArgs(args, "pattern", "path", "is_regex")
			if _, has := picked["pattern"]; !has {
				picked["pattern"] = rawString(pattern)
			}
			shaped, why := shapeArgs(t, withAction("grep_search", picked))
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		if t, ok := pickNamedTool(tools, "grep_search", "grep", "search_text", "search", "code_search", "find_text"); ok {
			shaped, why := shapeArgs(t, pickArgs(args, "pattern", "path", "is_regex"))
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		if t, ok := pickShellTool(tools); ok {
			where := path
			if where == "" {
				where = "."
			}
			op := "grep -rn"
			if args.Get("is_regex").Bool() {
				op = "grep -rnE"
			}
			shaped, why := shellShaped(t, fmt.Sprintf("%s -- %s %s", op, shellQuote(pattern), shellQuote(where)), args)
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		for _, t := range tools {
			if t.isActionTool() {
				continue
			}
			picked := map[string]json.RawMessage{}
			switch {
			case t.hasProp("pattern"):
				picked["pattern"] = rawString(pattern)
			case t.hasProp("query"):
				picked["query"] = rawString(pattern)
			default:
				continue
			}
			if t.hasProp("path") && path != "" {
				picked["path"] = rawString(path)
			}
			shaped, why := shapeArgs(t, picked)
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		return mapFail("no tool declared in this session can search text")

	case "run_command":
		command := argStr("command")
		if command == "" {
			return mapFail("intent run_command requires args.command")
		}
		if t, ok := pickShellTool(tools); ok {
			shaped, why := shellShaped(t, command, args)
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		for _, t := range tools {
			if t.isActionTool() && (t.enumHas("run_host") || t.mentions("run_host")) {
				b, _ := json.Marshal([]string{"bash", "-lc", command})
				picked := map[string]json.RawMessage{
					"action": rawString("run_host"),
					"exe":    rawString("bash"),
					"args":   b,
				}
				if cwd := args.Get("cwd"); cwd.Exists() && t.hasProp("cwd") {
					picked["cwd"] = json.RawMessage(cwd.Raw)
				}
				shaped, why := shapeArgs(t, picked)
				if why != "" {
					return mapFail(why)
				}
				return t, shaped, ""
			}
		}
		if t, ok := pickActionTool(tools, "run_command"); ok {
			picked := pickArgs(args, "command", "cwd")
			shaped, why := shapeArgs(t, withAction("run_command", picked))
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		if t, ok := pickNamedTool(tools, "shell", "exec", "run_command", "bash", "terminal", "execute_command"); ok && t.hasProp("command") {
			shaped, why := shellShaped(t, command, args)
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		return mapFail("no tool declared in this session can run a command")

	case "write_file":
		if argStr("path") == "" || !args.Get("content").Exists() {
			return mapFail("intent write_file requires args.path and args.content")
		}
		if t, ok := pickActionTool(tools, "write_file"); ok {
			shaped, why := shapeArgs(t, withAction("write_file", pickArgs(args, "path", "content")))
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		for _, t := range tools {
			if !t.isActionTool() && t.hasProp("path") && t.hasProp("content") {
				shaped, why := shapeArgs(t, pickArgs(args, "path", "content"))
				if why != "" {
					return mapFail(why)
				}
				return t, shaped, ""
			}
		}
		return mapFail("no tool declared in this session can write a file (shell synthesis is deliberately not used for writes)")

	case "edit_file":
		if argStr("path") == "" || !args.Get("old_text").Exists() || !args.Get("new_text").Exists() {
			return mapFail("intent edit_file requires args.path, args.old_text and args.new_text")
		}
		if t, ok := pickActionTool(tools, "edit_file"); ok {
			shaped, why := shapeArgs(t, withAction("edit_file", pickArgs(args, "path", "old_text", "new_text")))
			if why != "" {
				return mapFail(why)
			}
			return t, shaped, ""
		}
		for _, t := range tools {
			if !t.isActionTool() && t.hasProp("old_text") && t.hasProp("new_text") {
				shaped, why := shapeArgs(t, pickArgs(args, "path", "old_text", "new_text"))
				if why != "" {
					return mapFail(why)
				}
				return t, shaped, ""
			}
		}
		return mapFail("no tool declared in this session can edit a file (shell synthesis is deliberately not used for edits)")
	}
	return mapFail(fmt.Sprintf("unknown tool intent %q", intent))
}

// mailboxMappedCall is one intent after successful mapping: the real
// client tool name plus its schema-shaped arguments (JSON object,
// serialized — the APIs carry function arguments as a JSON string).
type mailboxMappedCall struct {
	Index     int // index into the answer's tool_calls list
	Intent    string
	Name      string
	CallID    string // call_<qid>_<index>
	ItemID    string // fc_<qid>_<index>
	Arguments string
}

// mailboxResolved is an answer after assembly validation: the text
// the client will see (rejected intents appended in plain sight) and
// the mapped calls it will receive.
type mailboxResolved struct {
	Text     string
	Calls    []mailboxMappedCall
	Rejected []mailboxRejectedCall
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// resolveAnswer validates an answer's tool intents against the
// session's declared tools and maps the ones that fit. Rejected
// intents are appended to the visible text and recorded for the
// session record; they are never silently dropped (PLAN §9).
func resolveAnswer(session *mailboxSession, q *mailboxQuestion, ans *mailboxAnswer) *mailboxResolved {
	res := &mailboxResolved{Text: ans.Text}
	if len(ans.ToolCalls) == 0 {
		return res
	}
	tools := parseDeclaredTools(session.Tools)
	for i, tc := range ans.ToolCalls {
		var args gjson.Result
		if len(tc.Args) > 0 {
			args = gjson.ParseBytes(tc.Args)
		}
		tool, shaped, why := mapIntent(tools, tc.Intent, args)
		if why != "" {
			rec := mailboxRejectedCall{
				QuestionID: q.ID,
				Intent:     tc.Intent,
				Args:       truncateStr(string(tc.Args), 300),
				Reason:     why,
				At:         time.Now(),
			}
			res.Rejected = append(res.Rejected, rec)
			if res.Text != "" {
				res.Text += "\n\n"
			}
			res.Text += fmt.Sprintf("[mailbox relay: tool intent %q (%s) was not sent to the client: %s.]", tc.Intent, rec.Args, why)
			continue
		}
		rawArgs, _ := json.Marshal(shaped)
		res.Calls = append(res.Calls, mailboxMappedCall{
			Index:     i,
			Intent:    tc.Intent,
			Name:      tool.name,
			CallID:    fmt.Sprintf("call_%s_%d", q.ID, i),
			ItemID:    fmt.Sprintf("fc_%s_%d", q.ID, i),
			Arguments: string(rawArgs),
		})
	}
	return res
}

// ---------- queue + wait ----------

type mailboxWaitOutcome int

const (
	mailboxAnswered mailboxWaitOutcome = iota
	mailboxTimedOut
	mailboxClientGone
)

// enqueue writes the question into spool/pending atomically and marks
// the session as awaiting an answer.
func (h *MailboxHandler) enqueue(q *mailboxQuestion, session *mailboxSession) error {
	if err := h.ensureDirs(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(h.dir("pending"), q.ID+".json"), data); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	session.LastQuestionID = q.ID
	session.LastStatus = "pending"
	session.UpdatedAt = time.Now()
	return h.saveSession(session)
}

// pollAnswer waits for spool/answers/<qid>.json to appear. While
// waiting it invokes onKeepalive at the keepalive interval (used by
// streaming responses to hold the connection). It also watches the
// client connection.
func (h *MailboxHandler) pollAnswer(c *gin.Context, qid string, onKeepalive func()) (*mailboxAnswer, mailboxWaitOutcome) {
	answerPath := filepath.Join(h.dir("answers"), qid+".json")
	deadline := time.NewTimer(h.pendingTimeout())
	defer deadline.Stop()
	poll := time.NewTicker(h.pollInterval())
	defer poll.Stop()
	var keepalive *time.Ticker
	var keepaliveC <-chan time.Time
	if onKeepalive != nil {
		keepalive = time.NewTicker(h.keepaliveInterval())
		defer keepalive.Stop()
		keepaliveC = keepalive.C
	}
	ctx := c.Request.Context()
	for {
		if data, err := os.ReadFile(answerPath); err == nil {
			var ans mailboxAnswer
			if err := json.Unmarshal(data, &ans); err == nil && (ans.Text != "" || len(ans.ToolCalls) > 0) {
				return &ans, mailboxAnswered
			}
		}
		select {
		case <-deadline.C:
			return nil, mailboxTimedOut
		case <-ctx.Done():
			return nil, mailboxClientGone
		case <-keepaliveC:
			onKeepalive()
		case <-poll.C:
		}
	}
}

// settle records the outcome: session settlement (append the
// assembled turns and any rejected-intent records on success),
// archive the question under spool/done, and remove the live
// queue/answer files. ans is the raw answer as written by the
// answerer (archived verbatim); res is the assembled form the client
// actually saw.
func (h *MailboxHandler) settle(session *mailboxSession, q *mailboxQuestion, ans *mailboxAnswer, res *mailboxResolved, outcome mailboxWaitOutcome, started time.Time) {
	status := "answered"
	if outcome == mailboxTimedOut {
		status = "timeout"
	} else if outcome == mailboxClientGone {
		status = "client_gone"
	}
	h.mu.Lock()
	if cur, err := h.loadSession(session.ID); err == nil && cur != nil {
		session = cur
	}
	session.UpdatedAt = time.Now()
	session.LastStatus = status
	if outcome == mailboxAnswered && res != nil {
		if res.Text != "" {
			session.Turns = append(session.Turns, mailboxTurn{Role: "assistant", Text: res.Text})
		}
		for _, call := range res.Calls {
			session.Turns = append(session.Turns, mailboxTurn{Role: "tool",
				Text: fmt.Sprintf("Tool call: %s(%s)", call.Name, call.Arguments)})
		}
		session.Rejected = append(session.Rejected, res.Rejected...)
	}
	_ = h.saveSession(session)
	h.mu.Unlock()

	done := mailboxDone{
		Question:   *q,
		Answer:     ans,
		Status:     status,
		WaitMillis: time.Since(started).Milliseconds(),
		FinishedAt: time.Now(),
	}
	if data, err := json.MarshalIndent(done, "", "  "); err == nil {
		_ = writeFileAtomic(filepath.Join(h.dir("done"), q.ID+".json"), data)
	}
	// A still-pending question nobody will answer now is withdrawn; a
	// claimed one is left for the answerer (its answer, if written,
	// simply finds no waiter and stays as an orphan file).
	if outcome != mailboxAnswered {
		_ = os.Remove(filepath.Join(h.dir("pending"), q.ID+".json"))
	} else {
		_ = os.Remove(filepath.Join(h.dir("pending"), q.ID+".json"))
		_ = os.Remove(filepath.Join(h.dir("claimed"), q.ID+".json"))
		_ = os.Remove(filepath.Join(h.dir("answers"), q.ID+".json"))
	}
}

// prepare runs the shared front half of both endpoints: read + parse
// the request, resolve the session chain, strip, render, enqueue.
// On error it writes the HTTP error itself and returns ok=false.
func (h *MailboxHandler) prepare(c *gin.Context, chat bool) (
	body []byte, model string, stream bool, session *mailboxSession, q *mailboxQuestion, started time.Time, ok bool,
) {
	started = time.Now()
	apiKey, hasKey := middleware.GetAPIKeyFromContext(c)
	if !hasKey || apiKey == nil {
		h.errorJSON(c, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "Invalid API key")
		return nil, "", false, nil, nil, started, false
	}
	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil || len(body) == 0 {
		h.errorJSON(c, http.StatusBadRequest, "invalid_request_error", "invalid_body", "Failed to read request body")
		return nil, "", false, nil, nil, started, false
	}
	if !gjson.ValidBytes(body) {
		h.errorJSON(c, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Failed to parse request body")
		return nil, "", false, nil, nil, started, false
	}
	modelRes := gjson.GetBytes(body, "model")
	if !modelRes.Exists() || modelRes.String() == "" {
		h.errorJSON(c, http.StatusBadRequest, "invalid_request_error", "model_required", "model is required")
		return nil, "", false, nil, nil, started, false
	}
	model = modelRes.String()
	stream = gjson.GetBytes(body, "stream").Bool()

	var ex mailboxExtract
	if chat {
		ex = extractChat(body)
	} else {
		ex = extractResponses(body)
	}
	if len(ex.Turns) == 0 {
		h.errorJSON(c, http.StatusBadRequest, "invalid_request_error", "empty_prompt", "No user prompt found in request")
		return nil, "", false, nil, nil, started, false
	}

	prevID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	var parent *mailboxSession
	if prevID != "" && !chat {
		if !validMailboxID(prevID) {
			h.errorJSON(c, http.StatusBadRequest, "invalid_request_error", "invalid_previous_response_id", "previous_response_id is malformed")
			return nil, "", false, nil, nil, started, false
		}
		h.mu.Lock()
		parent, err = h.loadSession(prevID)
		h.mu.Unlock()
		if err != nil {
			h.errorJSON(c, http.StatusInternalServerError, "api_error", "session_load_failed", "Failed to load session")
			return nil, "", false, nil, nil, started, false
		}
		if parent == nil {
			h.errorJSON(c, http.StatusBadRequest, "invalid_request_error", "previous_response_not_found",
				"previous_response_id does not match any mailbox session")
			return nil, "", false, nil, nil, started, false
		}
	}

	respID := mailboxNewID("resp_")
	if chat {
		respID = mailboxNewID("chatcmpl_")
	}
	merged := ex.Turns
	instructions := ex.Instructions
	hasInstructions := ex.HasInstructions
	var tools json.RawMessage = ex.Tools
	if parent != nil {
		merged = mergeTurns(parent.Turns, ex.Turns)
		if !hasInstructions && parent.Instructions != "" {
			instructions = parent.Instructions
			hasInstructions = true
		}
		if len(tools) == 0 && len(parent.Tools) > 0 {
			tools = parent.Tools
		}
	}
	_ = hasInstructions

	session = &mailboxSession{
		ID:           respID,
		ParentID:     prevID,
		Model:        model,
		Instructions: instructions,
		Tools:        tools,
		Turns:        merged,
		APIKeyID:     apiKey.ID,
		CreatedAt:    started,
		UpdatedAt:    started,
		LastStatus:   "pending",
	}
	q = &mailboxQuestion{
		ID:         mailboxNewID("mbq_"),
		SessionID:  respID,
		ParentID:   prevID,
		Model:      model,
		CreatedAt:  started,
		Prompt:     renderTranscript(merged),
		LatestUser: ex.LatestUser,
		APIKeyID:   apiKey.ID,
	}
	// Persist the session record before enqueueing so the chain head
	// exists even if this process dies while the question is pending.
	h.mu.Lock()
	err = h.saveSession(session)
	h.mu.Unlock()
	if err != nil {
		h.errorJSON(c, http.StatusInternalServerError, "api_error", "session_save_failed", "Failed to persist session")
		return nil, "", false, nil, nil, started, false
	}
	if err := h.enqueue(q, session); err != nil {
		h.errorJSON(c, http.StatusInternalServerError, "api_error", "enqueue_failed", "Failed to queue question")
		return nil, "", false, nil, nil, started, false
	}
	return body, model, stream, session, q, started, true
}

func (h *MailboxHandler) errorJSON(c *gin.Context, status int, errType, code, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"code":    code,
			"message": message,
		},
	})
}

// ---------- Responses API ----------

// responsesObject renders the Responses API response object. For a
// completed response, res carries the assembled answer: a message
// item when there is text, then one function_call item per mapped
// tool call.
func (h *MailboxHandler) responsesObject(session *mailboxSession, status string, res *mailboxResolved, created time.Time) map[string]any {
	obj := map[string]any{
		"id":         session.ID,
		"object":     "response",
		"created_at": created.Unix(),
		"status":     status,
		"model":      session.Model,
		"output":     []any{},
	}
	if session.ParentID != "" {
		obj["previous_response_id"] = session.ParentID
	} else {
		obj["previous_response_id"] = nil
	}
	// Stripped material is echoed back in the response object exactly
	// as the client uploaded it; it just never travels to the answerer.
	if session.Instructions != "" {
		obj["instructions"] = session.Instructions
	} else {
		obj["instructions"] = nil
	}
	if len(session.Tools) > 0 {
		obj["tools"] = json.RawMessage(session.Tools)
	} else {
		obj["tools"] = []any{}
	}
	if status == "completed" && res != nil {
		var items []any
		if res.Text != "" {
			items = append(items, map[string]any{
				"id":      "msg_" + session.LastQuestionID,
				"type":    "message",
				"status":  "completed",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": res.Text, "annotations": []any{}}},
			})
		}
		outText := res.Text
		for _, call := range res.Calls {
			items = append(items, map[string]any{
				"id":        call.ItemID,
				"type":      "function_call",
				"status":    "completed",
				"call_id":   call.CallID,
				"name":      call.Name,
				"arguments": call.Arguments,
			})
			outText += call.Arguments
		}
		obj["output"] = items
		obj["usage"] = map[string]any{
			"input_tokens":  estimateTokens(renderTranscript(session.Turns)),
			"output_tokens": estimateTokens(outText),
			"total_tokens":  estimateTokens(renderTranscript(session.Turns)) + estimateTokens(outText),
		}
	}
	return obj
}

func writeSSE(c *gin.Context, event string, payload map[string]any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if event != "" {
		_, _ = fmt.Fprintf(c.Writer, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", data)
	c.Writer.Flush()
}

func writeSSEComment(c *gin.Context, text string) {
	_, _ = fmt.Fprintf(c.Writer, ": %s\n\n", text)
	c.Writer.Flush()
}

// Responses serves POST /v1/responses for mailbox-platform keys.
func (h *MailboxHandler) Responses(c *gin.Context) {
	_, model, stream, session, q, started, ok := h.prepare(c, false)
	if !ok {
		return
	}
	_ = model

	if !stream {
		ans, outcome := h.pollAnswer(c, q.ID, nil)
		var res *mailboxResolved
		if outcome == mailboxAnswered {
			res = resolveAnswer(session, q, ans)
		}
		h.settle(session, q, ans, res, outcome, started)
		switch outcome {
		case mailboxAnswered:
			c.JSON(http.StatusOK, h.responsesObject(session, "completed", res, started))
		case mailboxTimedOut:
			h.errorJSON(c, http.StatusGatewayTimeout, "api_error", "mailbox_answer_timeout",
				"No answer arrived within the mailbox pending window")
		case mailboxClientGone:
			// Nothing sensible left to send.
		}
		return
	}

	// Streaming: open the stream immediately, send response.created,
	// then hold with keepalive comments until the answer lands.
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	seq := 0
	emit := func(event string, payload map[string]any) {
		payload["type"] = event
		payload["sequence_number"] = seq
		seq++
		writeSSE(c, event, payload)
	}
	createdObj := h.responsesObject(session, "in_progress", nil, started)
	emit("response.created", map[string]any{"response": createdObj})
	emit("response.in_progress", map[string]any{"response": createdObj})

	// Event-level wait heartbeat (MAILBOX-PACE): an SSE comment alone
	// is invisible to clients that only count output items as activity
	// (CXU's Thinking timer sat at 0s for the whole wait, because the
	// first output item used to be the answer itself). Open a reasoning
	// item at output 0 immediately and grow its summary by one ellipsis
	// per keepalive tick until the answer lands; the item is closed
	// before the real output is assembled. The accumulated summary is
	// pure wait filler ("..."), never answer content, and the reasoning
	// item also heads the completed object's output so streamed indices
	// and the final object agree. The ": ping" comment stays as the
	// transport-level keepalive underneath.
	rsID := "rs_" + q.ID
	rsTicks := 0
	rsSummary := func() string { return strings.Repeat("…", rsTicks) }
	rsItem := func(status string) map[string]any {
		return map[string]any{
			"id": rsID, "type": "reasoning", "status": status,
			"summary": []any{map[string]any{"type": "summary_text", "text": rsSummary()}},
		}
	}
	emit("response.output_item.added", map[string]any{
		"output_index": 0,
		"item":         map[string]any{"id": rsID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
	})
	emit("response.reasoning_summary_part.added", map[string]any{
		"item_id": rsID, "output_index": 0, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})
	rsBeat := func() {
		rsTicks++
		emit("response.reasoning_summary_text.delta", map[string]any{
			"item_id": rsID, "output_index": 0, "summary_index": 0, "delta": "…",
		})
	}
	rsBeat() // first beat at t=0, not after the first keepalive interval
	ans, outcome := h.pollAnswer(c, q.ID, func() { writeSSEComment(c, "ping"); rsBeat() })
	if outcome != mailboxClientGone {
		emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": rsID, "output_index": 0, "summary_index": 0, "text": rsSummary(),
		})
		emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": rsID, "output_index": 0, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": rsSummary()},
		})
		emit("response.output_item.done", map[string]any{
			"output_index": 0, "item": rsItem("completed"),
		})
	}
	var res *mailboxResolved
	if outcome == mailboxAnswered {
		res = resolveAnswer(session, q, ans)
	}
	h.settle(session, q, ans, res, outcome, started)
	switch outcome {
	case mailboxTimedOut:
		emit("error", map[string]any{
			"code":    "mailbox_answer_timeout",
			"message": "No answer arrived within the mailbox pending window",
			"param":   nil,
		})
		return
	case mailboxClientGone:
		return
	}

	outIdx := 1 // output 0 is the wait-heartbeat reasoning item
	if res.Text != "" {
		itemID := "msg_" + q.ID
		emit("response.output_item.added", map[string]any{
			"output_index": outIdx,
			"item": map[string]any{
				"id": itemID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		})
		emit("response.content_part.added", map[string]any{
			"item_id": itemID, "output_index": outIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
		for _, chunk := range chunkText(res.Text, 120) {
			emit("response.output_text.delta", map[string]any{
				"item_id": itemID, "output_index": outIdx, "content_index": 0, "delta": chunk,
			})
		}
		emit("response.output_text.done", map[string]any{
			"item_id": itemID, "output_index": outIdx, "content_index": 0, "text": res.Text,
		})
		emit("response.content_part.done", map[string]any{
			"item_id": itemID, "output_index": outIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": res.Text, "annotations": []any{}},
		})
		emit("response.output_item.done", map[string]any{
			"output_index": outIdx,
			"item": map[string]any{
				"id": itemID, "type": "message", "status": "completed",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": res.Text, "annotations": []any{}}},
			},
		})
		outIdx++
	}
	for _, call := range res.Calls {
		emit("response.output_item.added", map[string]any{
			"output_index": outIdx,
			"item": map[string]any{
				"id": call.ItemID, "type": "function_call", "status": "in_progress",
				"call_id": call.CallID, "name": call.Name, "arguments": call.Arguments,
			},
		})
		emit("response.function_call_arguments.done", map[string]any{
			"item_id": call.ItemID, "output_index": outIdx, "arguments": call.Arguments,
		})
		emit("response.output_item.done", map[string]any{
			"output_index": outIdx,
			"item": map[string]any{
				"id": call.ItemID, "type": "function_call", "status": "completed",
				"call_id": call.CallID, "name": call.Name, "arguments": call.Arguments,
			},
		})
		outIdx++
	}
	completedObj := h.responsesObject(session, "completed", res, started)
	// Keep the completed object's output aligned with the streamed
	// indices: the wait-heartbeat reasoning item is output 0.
	if items, _ := completedObj["output"].([]any); items != nil {
		completedObj["output"] = append([]any{rsItem("completed")}, items...)
	} else {
		completedObj["output"] = []any{rsItem("completed")}
	}
	emit("response.completed", map[string]any{"response": completedObj})
}

func chunkText(s string, size int) []string {
	r := []rune(s)
	if len(r) == 0 {
		return []string{""}
	}
	var chunks []string
	for i := 0; i < len(r); i += size {
		end := i + size
		if end > len(r) {
			end = len(r)
		}
		chunks = append(chunks, string(r[i:end]))
	}
	return chunks
}

// ---------- Chat Completions API ----------

// ChatCompletions serves POST /v1/chat/completions for mailbox-platform keys.
func (h *MailboxHandler) ChatCompletions(c *gin.Context) {
	_, model, stream, session, q, started, ok := h.prepare(c, true)
	if !ok {
		return
	}
	usage := func(ansText string) map[string]any {
		in := estimateTokens(q.Prompt)
		out := estimateTokens(ansText)
		return map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
	}

	if !stream {
		ans, outcome := h.pollAnswer(c, q.ID, nil)
		var res *mailboxResolved
		if outcome == mailboxAnswered {
			res = resolveAnswer(session, q, ans)
		}
		h.settle(session, q, ans, res, outcome, started)
		switch outcome {
		case mailboxAnswered:
			msg := map[string]any{"role": "assistant", "content": res.Text}
			finishReason := "stop"
			if len(res.Calls) > 0 {
				var tcs []any
				for _, call := range res.Calls {
					tcs = append(tcs, map[string]any{
						"id":   call.CallID,
						"type": "function",
						"function": map[string]any{
							"name":      call.Name,
							"arguments": call.Arguments,
						},
					})
				}
				msg["tool_calls"] = tcs
				finishReason = "tool_calls"
			}
			c.JSON(http.StatusOK, map[string]any{
				"id": session.ID, "object": "chat.completion", "created": started.Unix(), "model": model,
				"choices": []any{map[string]any{
					"index":         0,
					"message":       msg,
					"finish_reason": finishReason,
				}},
				"usage": usage(res.Text),
			})
		case mailboxTimedOut:
			h.errorJSON(c, http.StatusGatewayTimeout, "api_error", "mailbox_answer_timeout",
				"No answer arrived within the mailbox pending window")
		case mailboxClientGone:
		}
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	chunk := func(delta map[string]any, finish any) {
		writeSSE(c, "", map[string]any{
			"id": session.ID, "object": "chat.completion.chunk", "created": started.Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
	}
	chunk(map[string]any{"role": "assistant", "content": ""}, nil)

	// Event-level wait heartbeat (MAILBOX-PACE): as on the Responses
	// side, a bare SSE comment is invisible to clients. Periodic
	// reasoning_content chunks keep the stream visibly active while
	// waiting without ever touching content; the first beat goes out
	// immediately. The ": ping" comment stays underneath as the
	// transport-level keepalive.
	chunk(map[string]any{"reasoning_content": "…"}, nil)
	ans, outcome := h.pollAnswer(c, q.ID, func() {
		writeSSEComment(c, "ping")
		chunk(map[string]any{"reasoning_content": "…"}, nil)
	})
	var res *mailboxResolved
	if outcome == mailboxAnswered {
		res = resolveAnswer(session, q, ans)
	}
	h.settle(session, q, ans, res, outcome, started)
	switch outcome {
	case mailboxTimedOut:
		writeSSE(c, "", map[string]any{
			"error": map[string]any{
				"type": "api_error", "code": "mailbox_answer_timeout",
				"message": "No answer arrived within the mailbox pending window",
			},
		})
		return
	case mailboxClientGone:
		return
	}
	if res.Text != "" {
		for _, piece := range chunkText(res.Text, 120) {
			chunk(map[string]any{"content": piece}, nil)
		}
	}
	for j, call := range res.Calls {
		chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": j,
			"id":    call.CallID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		}}}, nil)
	}
	finishReason := "stop"
	if len(res.Calls) > 0 {
		finishReason = "tool_calls"
	}
	chunk(map[string]any{}, finishReason)
	_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	c.Writer.Flush()
}
