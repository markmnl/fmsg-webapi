package models

// Attachment represents a file attachment associated with a message.
type Attachment struct {
	Size     int    `json:"size"`
	Filename string `json:"filename"`
}

// Message represents a fmsg message as exchanged over the HTTP API.
type Message struct {
	Version     int          `json:"version"`
	HasPid      bool         `json:"has_pid"`
	HasAddTo    bool         `json:"has_add_to"`
	Important   bool         `json:"important"`
	NoReply     bool         `json:"no_reply"`
	Deflate     bool         `json:"deflate"`
	PID         *int64       `json:"pid"`
	From        string       `json:"from"`
	To          []string     `json:"to"`
	AddTo       []string     `json:"add_to"`
	Time        *float64     `json:"time"`
	Topic       string       `json:"topic"`
	Type        string       `json:"type"`
	Size        int          `json:"size"`
	Attachments []Attachment `json:"attachments"`
}
