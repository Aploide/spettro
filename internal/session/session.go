package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	metadataFilename = "session.json"
	messagesFilename = "messages.json"
	todosFilename    = "todos.json"
	tasksFilename    = "tasks.json"
	agentsFilename   = "agents.jsonl"
)

type Message struct {
	Role     string    `json:"role"`
	Content  string    `json:"content"`
	Thinking string    `json:"thinking,omitempty"`
	Meta     string    `json:"meta,omitempty"`
	At       time.Time `json:"at"`
}

type Todo struct {
	ID           string    `json:"id"`
	Content      string    `json:"content"`
	Status       string    `json:"status"`
	Owner        string    `json:"owner,omitempty"`
	Source       string    `json:"source,omitempty"`
	Priority     string    `json:"priority,omitempty"`
	Dependencies []string  `json:"dependencies,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type AgentEvent struct {
	At             time.Time `json:"at"`
	Kind           string    `json:"kind,omitempty"` // agent | tool | command
	AgentID        string    `json:"agent_id"`
	AgentType      string    `json:"agent_type,omitempty"`
	ParentAgentID  string    `json:"parent_agent_id,omitempty"`
	Task           string    `json:"task,omitempty"`
	Status         string    `json:"status"`
	Summary        string    `json:"summary,omitempty"`
	ToolName       string    `json:"tool_name,omitempty"`
	ToolArgs       string    `json:"tool_args,omitempty"`
	ToolOutput     string    `json:"tool_output,omitempty"`
	ToolID         string    `json:"tool_id,omitempty"`
	CommandSegment string    `json:"command_segment,omitempty"`
	Decision       string    `json:"decision,omitempty"`
	DecisionSource string    `json:"decision_source,omitempty"`
	Reason         string    `json:"reason,omitempty"`
}

type GoalRecord struct {
	Objective       string    `json:"objective"`
	Iteration       int       `json:"iteration"`
	NoProgress      int       `json:"no_progress"`
	StartedAt       time.Time `json:"started_at"`
	MaxIterations   int       `json:"max_iterations"`
	NoProgressLimit int       `json:"no_progress_limit"`
	Active          bool      `json:"active"` // true == unfinished when last saved
}

type Metadata struct {
	ID          string    `json:"id"`
	ProjectPath string    `json:"project_path"`
	ProjectHash string    `json:"project_hash"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Preview is the first user message snippet, persisted so the resume
	// picker can list sessions without loading every message file.
	Preview string `json:"preview,omitempty"`
	// Goal is the optional goal run record. When Active is true on reload,
	// the user is offered to resume the goal.
	Goal *GoalRecord `json:"goal,omitempty"`
}

type State struct {
	Metadata Metadata
	Messages []Message
	Todos    []Todo
	Tasks    []Todo
	Events   []AgentEvent
}

type Summary struct {
	ID        string
	StartedAt time.Time
	UpdatedAt time.Time
	Path      string
	Preview   string
	Legacy    bool
}

func NewID(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		now := time.Now().UnixNano()
		copy(suffix[:], fmt.Sprintf("%08x", now))
	}
	return fmt.Sprintf("session-%x-%s", sum[:4], hex.EncodeToString(suffix[:]))
}

func ProjectHash(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return fmt.Sprintf("%x", sum[:8])
}
