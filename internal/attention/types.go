package attention

import "time"

const SchemaVersion = 1

type Priority string

const (
	PriorityCorrection   Priority = "human_correction"
	PriorityDirect       Priority = "direct"
	PriorityReview       Priority = "task_review"
	PrioritySubscription Priority = "subscription"
)

type InboxState string

const (
	Pending InboxState = "pending"
	Claimed InboxState = "claimed"
	Done    InboxState = "done"
)

type Delivery struct {
	AgentID  string   `json:"agent_id"`
	Priority Priority `json:"priority"`
}

type Message struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Seq            int64     `json:"seq"`
	AuthorID       string    `json:"author_id"`
	Kind           string    `json:"kind"`
	Content        string    `json:"content"`
	CreatedAt      time.Time `json:"created_at"`
}

type Conversation struct {
	ID       string    `json:"id"`
	LastSeq  int64     `json:"last_seq"`
	Messages []Message `json:"messages"`
}

type InboxItem struct {
	ID             string     `json:"id"`
	AgentID        string     `json:"agent_id"`
	ConversationID string     `json:"conversation_id"`
	FromSeq        int64      `json:"from_seq"`
	ToSeq          int64      `json:"to_seq"`
	Priority       Priority   `json:"priority"`
	State          InboxState `json:"state"`
	LeaseID        string     `json:"lease_id,omitempty"`
	LeaseExpiresAt time.Time  `json:"lease_expires_at,omitempty"`
	ClaimReadSeq   int64      `json:"claim_read_seq,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type WorkMark struct {
	AgentID        string    `json:"agent_id"`
	ConversationID string    `json:"conversation_id"`
	TaskID         string    `json:"task_id,omitempty"`
	Reason         string    `json:"reason"`
	Active         bool      `json:"active"`
	Revision       int64     `json:"revision"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Draft struct {
	OperationID    string    `json:"operation_id"`
	AgentID        string    `json:"agent_id"`
	ConversationID string    `json:"conversation_id"`
	ItemID         string    `json:"item_id"`
	Content        string    `json:"content"`
	BaseReadSeq    int64     `json:"base_read_seq"`
	LatestSeq      int64     `json:"latest_seq"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Operation struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	Fingerprint string    `json:"fingerprint"`
	Outcome     string    `json:"outcome"`
	EntityID    string    `json:"entity_id,omitempty"`
	Revision    int64     `json:"revision"`
	CreatedAt   time.Time `json:"created_at"`
}

type State struct {
	SchemaVersion int                     `json:"schema_version"`
	Revision      int64                   `json:"revision"`
	Conversations map[string]Conversation `json:"conversations"`
	Inbox         map[string]InboxItem    `json:"inbox"`
	ReadCursors   map[string]int64        `json:"read_cursors"`
	WorkMarks     map[string]WorkMark     `json:"work_marks"`
	Drafts        map[string]Draft        `json:"drafts"`
	Operations    []Operation             `json:"operations"`
	UpdatedAt     time.Time               `json:"updated_at"`
}

type PublishInput struct {
	OperationID    string     `json:"operation_id"`
	MessageID      string     `json:"message_id"`
	ConversationID string     `json:"conversation_id"`
	AuthorID       string     `json:"author_id"`
	Kind           string     `json:"kind"`
	Content        string     `json:"content"`
	Deliveries     []Delivery `json:"deliveries,omitempty"`
}

type Claim struct {
	Item     InboxItem `json:"item"`
	Messages []Message `json:"messages"`
}

type WorkMarkInput struct {
	OperationID    string `json:"operation_id"`
	AgentID        string `json:"agent_id"`
	ConversationID string `json:"conversation_id"`
	TaskID         string `json:"task_id,omitempty"`
	Reason         string `json:"reason"`
}

type FreshReplyInput struct {
	OperationID string     `json:"operation_id"`
	MessageID   string     `json:"message_id"`
	AgentID     string     `json:"agent_id"`
	ItemID      string     `json:"item_id"`
	LeaseID     string     `json:"lease_id"`
	ReadSeq     int64      `json:"read_seq"`
	Content     string     `json:"content"`
	Deliveries  []Delivery `json:"deliveries,omitempty"`
}

type FreshReplyResult struct {
	Sent   *Message  `json:"sent,omitempty"`
	Draft  *Draft    `json:"draft,omitempty"`
	Missed []Message `json:"missed,omitempty"`
}
