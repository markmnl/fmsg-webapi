package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// Search. GET /fmsg/search is an optional endpoint: it finds the caller's
// threads by the words in their messages and returns them whole. This server
// defines the route and the shapes below so clients can rely on one contract
// (see the README), but does not implement it: it answers 501 Not
// Implemented. A deployment that indexes message text can implement it.

type searchMatch struct {
	ID int64 `json:"id"`
	// Field is where the words were found: body, topic, file, from or to.
	Field string `json:"field"`
	Text  string `json:"text"`
}

type searchThread struct {
	Root int64 `json:"root"`
	// Time is when the thread was last active: its newest message's time.
	Time *float64 `json:"time"`
	// Match is the newest message of the thread that has the words; null
	// when there were no words to find.
	Match *searchMatch `json:"match"`
}

type searchResponse struct {
	// Total is how many threads match, over all pages.
	Total int64 `json:"total"`
	// NextOffset is the offset of the next page, null when this is the last.
	NextOffset *int              `json:"next_offset"`
	Terms      []string          `json:"terms"`
	Threads    []searchThread    `json:"threads"`
	Messages   []messageListItem `json:"messages"`
}

// Search handles GET /fmsg/search. Not implemented by this server.
func (h *MessageHandler) Search(c *gin.Context) {
	if middleware.GetIdentity(c) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.JSON(http.StatusNotImplemented, gin.H{"error": "search is not implemented by this host"})
}
