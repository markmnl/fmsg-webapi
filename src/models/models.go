package models

// Attachment represents a file attachment associated with a message.
type Attachment struct {
	Size     int    `json:"size"`
	Filename string `json:"filename"`
}

// Message represents a fmsg message as exchanged over the HTTP API.
type Message struct {
	Version     int          `json:"version"`
	Flags       int          `json:"flags"`
	PID         *int64       `json:"pid"`
	From        string       `json:"from"`
	To          []string     `json:"to"`
	Time        *float64     `json:"time"`
	Topic       string       `json:"topic"`
	Type        string       `json:"type"`
	Size        int          `json:"size"`
	Attachments []Attachment `json:"attachments"`
}
