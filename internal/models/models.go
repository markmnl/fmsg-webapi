package models

// Attachment represents a file attachment associated with a message.
type Attachment struct {
	Size     int    `json:"size"`
	Filename string `json:"filename"`
}

// RecipientDelivery is the delivery status of one recipient of a message or
// add-to batch, sourced from msg_to/msg_add_to.time_delivered and response_code.
type RecipientDelivery struct {
	Addr          string  `json:"addr"`
	TimeDelivered *string `json:"time_delivered"` // RFC3339 UTC; nil if not yet delivered
	ResponseCode  *int    `json:"response_code"`  // nil unless a delivery attempt failed
}

// AddToBatch represents a single add-to delivery: its stable batch identifier,
// the recipients added in one POST /fmsg/:id/add-to call, who added them
// (add_to_from), and when (time).
type AddToBatch struct {
	BatchID    int64               `json:"batch_id"`
	AddToFrom  string              `json:"add_to_from"`
	To         []string            `json:"to"`
	ToDelivery []RecipientDelivery `json:"to_delivery"`
	Time       float64             `json:"time"`
}

// Message represents a fmsg message as exchanged over the HTTP API.
type Message struct {
	Version     int                 `json:"version"`
	HasPid      bool                `json:"has_pid"`
	HasAddTo    bool                `json:"has_add_to"`
	Important   bool                `json:"important"`
	NoReply     bool                `json:"no_reply"`
	Deflate     bool                `json:"deflate"`
	Terminal    bool                `json:"terminal"`
	PID         *int64              `json:"pid"`
	From        string              `json:"from"`
	To          []string            `json:"to"`
	ToDelivery  []RecipientDelivery `json:"to_delivery"`
	AddTo       []AddToBatch        `json:"add_to"`
	Time        *float64            `json:"time"`
	Topic       string              `json:"topic"`
	Type        string              `json:"type"`
	Size        int                 `json:"size"`
	ShortText   string              `json:"short_text,omitempty"`
	Read        bool                `json:"read"`
	TimeRead    *float64            `json:"time_read"`
	Attachments []Attachment        `json:"attachments"`
	Reaction    *string             `json:"reaction"`  // FMSG-005: the emoji this message carries when it is a reaction ("" clears); nil otherwise
	Reactions   []Reaction          `json:"reactions"` // FMSG-005: effective reactions on this message
}

// Reaction is one emoji currently reacted on a message and the participants
// whose effective reaction it is (FMSG-005).
type Reaction struct {
	Emoji string   `json:"emoji"`
	From  []string `json:"from"`
}
