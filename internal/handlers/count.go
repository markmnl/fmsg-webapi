package handlers

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Count handles GET /fmsg/count: how many threads the authenticated identity
// has, for showing "93 of 8,324 threads" while the list is paged in. A thread
// is counted once for each sent message the identity can see (as its sender,
// a recipient or an added recipient) whose parent it can't also see, which is
// how clients group a thread from listed messages. trash is as for GET /fmsg:
// exclude (default), include or only.
func (h *MessageHandler) Count(c *gin.Context) {
	addrs, err := h.visibleAddrs(c)
	if err != nil {
		log.Printf("count threads: resolve visible addrs: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count threads"})
		return
	}
	trashFilter, ok := trashListFilter(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	hasTrash, err := h.hasTrash(ctx, addrs[0])
	if err != nil {
		log.Printf("count threads: trash: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count threads"})
		return
	}
	var threads int64
	err = h.q().QueryRow(ctx,
		`WITH vis AS (
		     SELECT m.id, m.pid
		       FROM msg m
		       CROSS JOIN LATERAL (SELECT CASE WHEN $2 THEN fmsg_api_trash_state($3, m.id) END AS state) t
		      WHERE m.time_sent IS NOT NULL
		        AND (lower(m.from_addr) = ANY($1)
		          OR EXISTS (SELECT 1 FROM msg_to mt WHERE mt.msg_id = m.id AND lower(mt.addr) = ANY($1))
		          OR EXISTS (SELECT 1 FROM msg_add_to mat WHERE mat.msg_id = m.id AND lower(mat.addr) = ANY($1)))
		        AND `+trashFilter+`
		 )
		 SELECT count(*) FROM vis v
		  WHERE v.pid IS NULL OR NOT EXISTS (SELECT 1 FROM vis p WHERE p.id = v.pid)`,
		addrs, hasTrash, addrs[0],
	).Scan(&threads)
	if err != nil {
		log.Printf("count threads: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count threads"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"threads": threads})
}
