// Package wire defines the Helm v1 protocol between helmd and helm-server.
// The single source of truth is protocol/README.md plus protocol/fixtures/*.json;
// these types must round-trip every fixture (see wire_test.go).
package wire

import (
	"encoding/json"
	"time"
)

const Version = 1

// ---------- Run states ----------

type RunState string

const (
	RunPending   RunState = "PENDING"
	RunPreparing RunState = "PREPARING"
	RunRunning   RunState = "RUNNING"
	RunGated     RunState = "GATED"
	RunSuspended RunState = "SUSPENDED"
	RunVerifying RunState = "VERIFYING"
	RunSucceeded RunState = "SUCCEEDED"
	RunFailed    RunState = "FAILED"
	RunAborted   RunState = "ABORTED"
	RunOrphaned  RunState = "ORPHANED"
)

// ---------- WebSocket envelope ----------

type MsgType string

const (
	MsgWake      MsgType = "wake"
	MsgHeartbeat MsgType = "heartbeat"
	MsgRunEvents MsgType = "run_events"
)

type Envelope struct {
	V       int             `json:"v"`
	Type    MsgType         `json:"type"`
	ID      string          `json:"id"`
	TS      time.Time       `json:"ts"`
	Payload json.RawMessage `json:"payload"`
}

type Wake struct {
	Reason string `json:"reason"` // run_queued | directive_queued | skills_updated
}

type GPULoad struct {
	Index      int     `json:"index"`
	UtilPct    float64 `json:"util_pct"`
	MemUsedMB  int     `json:"mem_used_mb"`
	MemTotalMB int     `json:"mem_total_mb"`
}

type Load struct {
	CPUPct float64   `json:"cpu_pct"`
	GPUs   []GPULoad `json:"gpus,omitempty"`
}

type Heartbeat struct {
	RunningRuns []string `json:"running_runs"`
	Load        Load     `json:"load"`
}

// ---------- L1 events ----------

type EventType string

const (
	EvStateChange  EventType = "state_change"
	EvAgentMessage EventType = "agent_message"
	EvToolCall     EventType = "tool_call"
	EvCmdExit      EventType = "cmd_exit"
	EvFSChange     EventType = "fs_change"
	EvError        EventType = "error"
	EvEvidence     EventType = "evidence"
	EvClaim        EventType = "claim"
)

type Event struct {
	Seq  int64           `json:"seq"`
	TS   time.Time       `json:"ts"`
	Type EventType       `json:"type"`
	Data json.RawMessage `json:"data"`
}

type RunEvents struct {
	RunID  string  `json:"run_id"`
	Events []Event `json:"events"`
}

// Typed event payloads (encode into Event.Data with MustData).
type StateChange struct {
	From   RunState `json:"from"`
	To     RunState `json:"to"`
	Reason string   `json:"reason,omitempty"`
}

type AgentMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type ToolCall struct {
	Tool        string `json:"tool"`
	InputDigest string `json:"input_digest"`
	Decision    string `json:"decision"` // allow | deny | hold
	Reason      string `json:"reason,omitempty"`
}

type CmdExit struct {
	CmdDigest  string `json:"cmd_digest"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
}

type FSChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	InScope bool   `json:"in_scope"`
}

type ErrorSig struct {
	Signature string `json:"signature"`
	Message   string `json:"message"`
	Count     int    `json:"count"`
}

type Evidence struct {
	URI   string `json:"uri"`
	Kind  string `json:"kind"`
	Bytes int64  `json:"bytes"`
}

type Claim struct {
	Kind string `json:"kind"` // done | milestone | question
	Text string `json:"text"`
}

func MustData(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------- Run assignment / contract ----------

type Scope struct {
	WritePaths    []string `json:"write_paths"`
	DenyCommands  []string `json:"deny_commands,omitempty"`
	ReadonlyPaths []string `json:"readonly_paths,omitempty"`
}

type Budgets struct {
	Tokens      int64 `json:"tokens,omitempty"`
	WallSeconds int64 `json:"wall_seconds,omitempty"`
	GPUHours    int64 `json:"gpu_hours,omitempty"`
}

type Contract struct {
	Version     int      `json:"version"`
	Intent      string   `json:"intent"`
	Prompt      string   `json:"prompt"`
	Repo        string   `json:"repo,omitempty"`
	BaseRef     string   `json:"base_ref,omitempty"`
	Workdir     string   `json:"workdir,omitempty"` // non-git tasks
	Scope       Scope    `json:"scope"`
	Checkpoints []string `json:"checkpoints,omitempty"`
	Acceptance  []string `json:"acceptance,omitempty"`
	Budgets     Budgets  `json:"budgets"`
	Skills      []string `json:"skills,omitempty"`
}

type RunAssignment struct {
	RunID      string    `json:"run_id"`
	TaskID     string    `json:"task_id"`
	Epoch      int64     `json:"epoch"`
	LeaseUntil time.Time `json:"lease_until"`
	Executor   string    `json:"executor"` // codex | claude | shell | job
	Contract   Contract  `json:"contract"`
}

// ---------- Directives ----------

type DirectiveKind string

const (
	DirContinue DirectiveKind = "CONTINUE"
	DirSteer    DirectiveKind = "STEER"
	DirAmend    DirectiveKind = "AMEND"
	DirPause    DirectiveKind = "PAUSE"
	DirRollback DirectiveKind = "ROLLBACK"
	DirAbort    DirectiveKind = "ABORT"
)

type Directive struct {
	ID              string          `json:"id"`
	RunID           string          `json:"run_id"`
	Kind            DirectiveKind   `json:"kind"`
	ContractVersion int             `json:"contract_version"`
	Args            json.RawMessage `json:"args,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
}

type AckStatus string

const (
	AckApplied         AckStatus = "applied"
	AckRejectedStale   AckStatus = "rejected_stale"
	AckRejectedInvalid AckStatus = "rejected_invalid"
	AckFailed          AckStatus = "failed"
)

type DirectiveAck struct {
	DirectiveID string    `json:"directive_id"`
	Status      AckStatus `json:"status"`
	Detail      string    `json:"detail,omitempty"`
	AckedAt     time.Time `json:"acked_at"`
}

// ---------- Capabilities ----------

type AgentCLI struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
}

type GPU struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	MemTotalMB int    `json:"mem_total_mb"`
}

type Capabilities struct {
	MachineName       string     `json:"machine_name"`
	DaemonVersion     string     `json:"daemon_version"`
	OS                string     `json:"os"`
	Arch              string     `json:"arch"`
	Labels            []string   `json:"labels"`
	Agents            []AgentCLI `json:"agents"`
	GPUs              []GPU      `json:"gpus"`
	MaxConcurrentRuns int        `json:"max_concurrent_runs"`
}
