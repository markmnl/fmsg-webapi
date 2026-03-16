package models

// Attachment represents a file attachment associated with a message.
type Attachment struct {
	Size     int    `json:"size"`
	Filename string `json:"filename"`
}

// Flag bitmask constants for the msg.flags column.
const (
	FlagHasPid        uint8 = 1
	FlagImportant     uint8 = 1 << 1
	FlagNoReply       uint8 = 1 << 3
	FlagSkipChallenge uint8 = 1 << 4
	FlagDeflate       uint8 = 1 << 5
)

// Message represents a fmsg message as exchanged over the HTTP API.
type Message struct {
	Version       int          `json:"version"`
	HasPid        bool         `json:"has_pid"`
	Important     bool         `json:"important"`
	NoReply       bool         `json:"no_reply"`
	SkipChallenge bool         `json:"skip_challenge"`
	Deflate       bool         `json:"deflate"`
	PID           *int64       `json:"pid"`
	From          string       `json:"from"`
	To            []string     `json:"to"`
	Time          *float64     `json:"time"`
	Topic         string       `json:"topic"`
	Type          string       `json:"type"`
	Size          int          `json:"size"`
	Attachments   []Attachment `json:"attachments"`
}

// EncodeFlags packs the boolean flag fields into a uint8 bitmask.
func (m *Message) EncodeFlags() uint8 {
	var f uint8
	if m.HasPid {
		f |= FlagHasPid
	}
	if m.Important {
		f |= FlagImportant
	}
	if m.NoReply {
		f |= FlagNoReply
	}
	if m.SkipChallenge {
		f |= FlagSkipChallenge
	}
	if m.Deflate {
		f |= FlagDeflate
	}
	return f
}

// DecodeFlags unpacks a uint8 bitmask into the boolean flag fields.
func (m *Message) DecodeFlags(f uint8) {
	m.HasPid = f&FlagHasPid != 0
	m.Important = f&FlagImportant != 0
	m.NoReply = f&FlagNoReply != 0
	m.SkipChallenge = f&FlagSkipChallenge != 0
	m.Deflate = f&FlagDeflate != 0
}
