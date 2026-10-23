package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// Trash. Each address has its own: deleting a sent message only hides it from
// that address, and replies keep arriving. A thread mark hides everything
// below the message it is on, including replies that arrive later. Deleting
// for good (purging) hides the message and everything that replies to it from
// that address for ever, gives back the storage it counted against that
// address, and removes it from the host once no local participant still
// holds it; a reply to it is then rejected as parent not found. State lives
// in fmsg_api_trash and is read through fmsg_api_trash_state (dd.sql).

const (
	trashMessage = "message" // deleted itself
	trashThread  = "thread"  // in a deleted thread
	trashPurged  = "purged"  // deleted for good
)

// trashStateQ is where msgID stands for addr: "", trashMessage, trashThread
// or trashPurged. An address that has never deleted anything costs one
// indexed lookup.
func trashStateQ(ctx context.Context, q queries, addr string, msgID int64) (string, error) {
	var state *string
	err := q.QueryRow(ctx,
		`SELECT CASE WHEN EXISTS (SELECT 1 FROM fmsg_api_trash WHERE addr = $1)
		             THEN fmsg_api_trash_state($1, $2) END`,
		strings.ToLower(addr), msgID).Scan(&state)
	if err != nil || state == nil {
		return "", err
	}
	return *state, nil
}

// purgedFor reports whether msgID is deleted for good by addr. Lookup errors
// are logged and treated as not purged.
func purgedFor(ctx context.Context, q queries, addr string, msgID int64) bool {
	state, err := trashStateQ(ctx, q, addr, msgID)
	if err != nil {
		log.Printf("trash state of %d for %s: %v", msgID, addr, err)
	}
	return state == trashPurged
}

// hasTrash reports whether addr has deleted anything.
func (h *MessageHandler) hasTrash(ctx context.Context, addr string) (bool, error) {
	var has bool
	err := h.q().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fmsg_api_trash WHERE addr = $1)`, strings.ToLower(addr)).Scan(&has)
	return has, err
}

// trashListFilter is the list condition for a ?trash= mode on a row whose
// state is in the column state. Purged messages are never listed.
func trashListFilter(c *gin.Context) (string, bool) {
	switch mode := c.DefaultQuery("trash", "exclude"); mode {
	case "exclude":
		return "t.state IS NULL", true
	case "include":
		return "t.state IS DISTINCT FROM 'purged'", true
	case "only":
		return "t.state IN ('message', 'thread')", true
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "trash must be exclude, include or only"})
		return "", false
	}
}

// localDomains are the domains whose addresses hold their messages on this
// host.
func (h *MessageHandler) localDomains() []string {
	if h.LocalDomain == "" {
		return nil
	}
	return []string{strings.ToLower(h.LocalDomain)}
}

// boolQuery reads a ?name=true flag.
func boolQuery(c *gin.Context, name string) bool {
	v := strings.ToLower(c.Query(name))
	return v == "true" || v == "1"
}

// participates reports whether addr sent msgID or is one of its recipients.
func (h *MessageHandler) participates(ctx context.Context, addr string, msgID int64) (bool, error) {
	var ok bool
	err := h.q().QueryRow(ctx, `SELECT
		    EXISTS (SELECT 1 FROM msg WHERE id = $1 AND lower(from_addr) = $2)
		 OR EXISTS (SELECT 1 FROM msg_to WHERE msg_id = $1 AND lower(addr) = $2)
		 OR EXISTS (SELECT 1 FROM msg_add_to WHERE msg_id = $1 AND lower(addr) = $2)`,
		msgID, strings.ToLower(addr)).Scan(&ok)
	return ok, err
}

// trashSubtree lists msgID and the sent messages below it that addr takes
// part in, deepest first.
func (h *MessageHandler) trashSubtree(ctx context.Context, addr string, msgID int64) ([]int64, error) {
	rows, err := h.q().Query(ctx, `
		WITH RECURSIVE sub(id, depth) AS (
			SELECT $1::bigint, 0
			UNION ALL
			SELECT m.id, s.depth + 1 FROM msg m JOIN sub s ON m.pid = s.id WHERE s.depth < 1000
		)
		SELECT s.id FROM sub s JOIN msg m ON m.id = s.id
		WHERE m.time_sent IS NOT NULL
		  AND (lower(m.from_addr) = $2
		       OR EXISTS (SELECT 1 FROM msg_to t WHERE t.msg_id = s.id AND lower(t.addr) = $2)
		       OR EXISTS (SELECT 1 FROM msg_add_to a WHERE a.msg_id = s.id AND lower(a.addr) = $2))
		GROUP BY s.id ORDER BY max(s.depth) DESC, s.id DESC`,
		msgID, strings.ToLower(addr))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// notifyTrash tells addr's connections that ids moved in or out of the trash.
// The notifications are sent when the transaction commits.
func (h *MessageHandler) notifyTrash(ctx context.Context, addr string, ids []int64) {
	for _, id := range ids {
		if _, err := h.q().Exec(ctx, `SELECT pg_notify('msg_trash', $1)`, fmt.Sprintf("%d,%s", id, addr)); err != nil {
			log.Printf("trash %d: notify: %v", id, err)
		}
	}
}

// trashSent handles DELETE /fmsg/:id for a sent message: it moves it (or with
// ?thread=true its thread) to the caller's trash, or with ?permanent=true
// deletes it for good. Runs in the request's transaction.
func (h *MessageHandler) trashSent(c *gin.Context, msgID int64) {
	identity := strings.ToLower(middleware.GetIdentity(c))
	ctx := c.Request.Context()

	ok, err := h.participates(ctx, identity, msgID)
	if err != nil {
		log.Printf("trash %d: participation: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
		return
	}
	state, err := trashStateQ(ctx, h.q(), identity, msgID)
	if err != nil {
		log.Printf("trash %d: state: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
		return
	}
	if !ok || state == trashPurged {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}

	thread := boolQuery(c, "thread")
	if boolQuery(c, "permanent") {
		h.purge(c, identity, msgID, thread)
		return
	}

	now := float64(time.Now().UnixMicro()) / 1e6
	if _, err := h.q().Exec(ctx, `
		INSERT INTO fmsg_api_trash (addr, msg_id, thread, time_deleted) VALUES ($1, $2, $3, $4)
		ON CONFLICT (addr, msg_id) DO UPDATE SET thread = fmsg_api_trash.thread OR EXCLUDED.thread`,
		identity, msgID, thread, now); err != nil {
		log.Printf("trash %d: mark: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
		return
	}
	ids := []int64{msgID}
	if thread {
		if ids, err = h.trashSubtree(ctx, identity, msgID); err != nil {
			log.Printf("trash %d: thread: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
			return
		}
	}
	h.notifyTrash(ctx, identity, ids)
	if thread {
		state = trashThread
	} else if state, err = trashStateQ(ctx, h.q(), identity, msgID); err != nil {
		log.Printf("trash %d: state: %v", msgID, err)
		state = trashMessage
	}
	c.JSON(http.StatusOK, gin.H{"id": msgID, "trash": state})
}

// Restore handles POST /fmsg/:id/restore — takes a message (or with
// ?thread=true the thread below it) out of the caller's trash. Messages
// deleted for good can't be restored.
func (h *MessageHandler) Restore(c *gin.Context) {
	identity := strings.ToLower(middleware.GetIdentity(c))
	msgID, ok := resolveID(c, h.q())
	if !ok {
		return
	}
	ctx := c.Request.Context()

	ok, err := h.participates(ctx, identity, msgID)
	if err != nil {
		log.Printf("restore %d: participation: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to restore message"})
		return
	}
	if !ok || purgedFor(ctx, h.q(), identity, msgID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}

	ids := []int64{msgID}
	if boolQuery(c, "thread") {
		if ids, err = h.trashSubtree(ctx, identity, msgID); err != nil {
			log.Printf("restore %d: thread: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to restore message"})
			return
		}
	}
	if _, err := h.q().Exec(ctx,
		`DELETE FROM fmsg_api_trash WHERE addr = $1 AND msg_id = ANY($2) AND time_purged IS NULL`,
		identity, ids); err != nil {
		log.Printf("restore %d: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to restore message"})
		return
	}
	h.notifyTrash(ctx, identity, ids)
	state, err := trashStateQ(ctx, h.q(), identity, msgID)
	if err != nil {
		log.Printf("restore %d: state: %v", msgID, err)
	}
	var trash *string
	if state != "" {
		trash = &state
	}
	c.JSON(http.StatusOK, gin.H{"id": msgID, "trash": trash})
}

// usageRelease is storage to give back to fmsgid once the purge commits.
type usageRelease struct {
	send bool
	size int64
}

// purge deletes msgID and everything below it for good for identity (with
// thread, msgID is the thread's top as the caller sees it). It gives back
// what each newly purged message counted against identity, and removes from
// the host every purged message no local participant still holds.
func (h *MessageHandler) purge(c *gin.Context, identity string, msgID int64, thread bool) {
	ctx := c.Request.Context()
	fail := func(what string, err error) {
		log.Printf("purge %d: %s: %v", msgID, what, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
	}

	ids, err := h.trashSubtree(ctx, identity, msgID)
	if err != nil {
		fail("subtree", err)
		return
	}
	top := int64(0)
	if thread {
		top = msgID
	}
	now := float64(time.Now().UnixMicro()) / 1e6
	purged, releases, err := h.markPurged(ctx, identity, ids, top, now)
	if err != nil {
		fail("mark", err)
		return
	}
	removed, files, err := h.removeUnheld(ctx, ids)
	if err != nil {
		fail("remove", err)
		return
	}
	h.notifyTrash(ctx, identity, ids)

	for _, p := range files {
		path := p
		afterCommit(c, func() { removeStoredFile(path) })
	}
	if h.IDURL != "" && len(releases) > 0 {
		idURL := h.IDURL
		afterCommit(c, func() {
			for _, r := range releases {
				var err error
				if r.send {
					err = middleware.ReleaseFmsgIDSend(idURL, identity, now, r.size)
				} else {
					err = middleware.ReleaseFmsgIDRecv(idURL, identity, now, r.size)
				}
				if err != nil {
					log.Printf("release usage of %s: %v", identity, err)
				}
			}
		})
	}
	c.JSON(http.StatusOK, gin.H{"id": msgID, "trash": trashPurged, "purged": purged, "removed": removed})
}

// markPurged deletes ids for good for identity, skipping those it already
// has, and returns how many were newly purged and what they counted against
// identity's quota. threadTop, when not 0, is the id whose trash row marks a
// whole thread.
func (h *MessageHandler) markPurged(ctx context.Context, identity string, ids []int64, threadTop int64, now float64) (int, []usageRelease, error) {
	var releases []usageRelease
	var purged int
	for _, id := range ids {
		var already bool
		if err := h.q().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM fmsg_api_trash WHERE addr = $1 AND msg_id = $2 AND time_purged IS NOT NULL)`,
			identity, id).Scan(&already); err != nil {
			return 0, nil, fmt.Errorf("purged state: %w", err)
		}
		if already {
			continue
		}
		if _, err := h.q().Exec(ctx, `
			INSERT INTO fmsg_api_trash (addr, msg_id, thread, time_deleted, time_purged) VALUES ($1, $2, $3, $4, $4)
			ON CONFLICT (addr, msg_id) DO UPDATE SET time_purged = EXCLUDED.time_purged,
			    thread = fmsg_api_trash.thread OR EXCLUDED.thread`,
			identity, id, threadTop != 0 && id == threadTop, now); err != nil {
			return 0, nil, err
		}
		purged++
		rel, err := h.usageOf(ctx, identity, id)
		if err != nil {
			return 0, nil, fmt.Errorf("usage: %w", err)
		}
		releases = append(releases, rel...)
	}
	return purged, releases, nil
}

// removeUnheld removes from the host each of ids that nobody here holds any
// more. ids are deepest first, so a reply is gone before the message it
// replies to. It returns how many were removed and the stored files to
// delete once the transaction commits.
func (h *MessageHandler) removeUnheld(ctx context.Context, ids []int64) (int, []string, error) {
	var removed int
	var files []string
	for _, id := range ids {
		gone, paths, err := h.removeIfUnheld(ctx, id)
		if err != nil {
			return 0, nil, err
		}
		if gone {
			removed++
			files = append(files, paths...)
		}
	}
	return removed, files, nil
}

// removeStoredFile deletes a stored body or attachment file.
func removeStoredFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("purge: remove %s: %v", path, err)
	}
}

// usageOf is what msgID counted against addr's quota: its size as sent if
// addr sent it, and as received for each delivery to addr. The size is the
// one recorded: messageStoredSize for a message sent from this host, and the
// expanded size fmsgd records for one it received.
func (h *MessageHandler) usageOf(ctx context.Context, addr string, msgID int64) ([]usageRelease, error) {
	var from string
	var sent bool
	var deliveries int
	if err := h.q().QueryRow(ctx, `SELECT m.from_addr, m.time_sent IS NOT NULL,
		    (SELECT count(*) FROM msg_to WHERE msg_id = m.id AND lower(addr) = $2 AND time_delivered IS NOT NULL)
		  + (SELECT count(*) FROM msg_add_to WHERE msg_id = m.id AND lower(addr) = $2 AND time_delivered IS NOT NULL)
		 FROM msg m WHERE m.id = $1`, msgID, addr).Scan(&from, &sent, &deliveries); err != nil {
		return nil, err
	}
	isSender := sent && strings.EqualFold(from, addr)
	if !isSender && deliveries == 0 {
		return nil, nil
	}
	size, err := h.recordedSize(ctx, msgID, from)
	if err != nil {
		return nil, err
	}
	var out []usageRelease
	if isSender {
		out = append(out, usageRelease{send: true, size: size})
	}
	for i := 0; i < deliveries; i++ {
		out = append(out, usageRelease{size: size})
	}
	return out, nil
}

// recordedSize is the size a message was recorded at for quotas.
func (h *MessageHandler) recordedSize(ctx context.Context, msgID int64, from string) (int64, error) {
	_, domain := parseAddr(from)
	for _, d := range h.localDomains() {
		if strings.EqualFold(domain, d) {
			return h.messageStoredSize(ctx, msgID)
		}
	}
	// Received by fmsgd, which records the expanded size: what is stored
	// on disk for a compressed part, the wire size otherwise.
	var size int64
	var deflate bool
	var body string
	if err := h.q().QueryRow(ctx, `SELECT size, is_deflate, filepath FROM msg WHERE id = $1`, msgID).Scan(&size, &deflate, &body); err != nil {
		return 0, err
	}
	if deflate {
		size = h.fileSize(body, size)
	}
	rows, err := h.q().Query(ctx, `SELECT flags, filesize, filepath FROM msg_attachment WHERE msg_id = $1`, msgID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var flags int
		var fsize int64
		var path string
		if err := rows.Scan(&flags, &fsize, &path); err != nil {
			return 0, err
		}
		if flags&(1<<1) != 0 {
			fsize = h.fileSize(path, fsize)
		}
		size += fsize
	}
	return size, rows.Err()
}

// fileSize is the size of a stored file, or fallback if it can't be read.
func (h *MessageHandler) fileSize(path string, fallback int64) int64 {
	clean, ok := safeDataPath(path, h.DataDir)
	if !ok {
		return fallback
	}
	if fi, err := os.Stat(clean); err == nil {
		return fi.Size()
	}
	return fallback
}

// removeIfUnheld removes msgID from the host when no local participant holds
// it any more (none sent or accepted it without deleting it for good) and no
// delivery or add-to notification of it is still pending. It returns whether
// it was removed and the stored files to delete once the transaction commits.
func (h *MessageHandler) removeIfUnheld(ctx context.Context, msgID int64) (bool, []string, error) {
	var held, pending bool
	err := h.q().QueryRow(ctx, `SELECT
		EXISTS (
			SELECT 1 FROM (
				SELECT from_addr AS a FROM msg WHERE id = $1 AND time_sent IS NOT NULL
				UNION ALL SELECT addr FROM msg_to WHERE msg_id = $1 AND time_delivered IS NOT NULL
				UNION ALL SELECT addr FROM msg_add_to WHERE msg_id = $1 AND time_delivered IS NOT NULL
			) p
			WHERE split_part(lower(p.a), '@', 3) = ANY($2)
			  AND NOT EXISTS (SELECT 1 FROM fmsg_api_trash t
			                  WHERE t.addr = lower(p.a) AND t.msg_id = $1 AND t.time_purged IS NOT NULL)),
		EXISTS (SELECT 1 FROM msg_to WHERE msg_id = $1 AND time_delivered IS NULL AND (response_code IS NULL OR response_code = -1))
		OR EXISTS (SELECT 1 FROM msg_add_to WHERE msg_id = $1 AND time_delivered IS NULL AND (response_code IS NULL OR response_code = -1))
		OR EXISTS (SELECT 1 FROM msg_add_to_notify n JOIN msg_add_to_batch b ON b.id = n.batch_id
		           WHERE b.msg_id = $1 AND n.time_notified IS NULL)`,
		msgID, h.localDomains()).Scan(&held, &pending)
	if err != nil || held || pending {
		return false, nil, err
	}

	var paths []string
	var body string
	if err := h.q().QueryRow(ctx, `SELECT filepath FROM msg WHERE id = $1`, msgID).Scan(&body); err != nil {
		return false, nil, err
	}
	paths = append(paths, body)
	rows, err := h.q().Query(ctx, `SELECT filepath FROM msg_attachment WHERE msg_id = $1`, msgID)
	if err != nil {
		return false, nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return false, nil, err
		}
		paths = append(paths, p)
	}
	rows.Close()

	// fmsgd keeps sent messages immutable unless the transaction says it is
	// purging (fmsgd dd.sql, protect_msg_parts).
	for _, q := range []string{
		`SET LOCAL fmsg.purge = 'on'`,
		`UPDATE msg SET pid = NULL WHERE pid = $1`,
		`DELETE FROM msg_add_to_notify WHERE batch_id IN (SELECT id FROM msg_add_to_batch WHERE msg_id = $1)`,
		`DELETE FROM msg_add_to WHERE msg_id = $1`,
		`DELETE FROM msg_add_to_batch WHERE msg_id = $1`,
		`DELETE FROM msg_to WHERE msg_id = $1`,
		`DELETE FROM msg_attachment WHERE msg_id = $1`,
		`DELETE FROM msg WHERE id = $1`,
	} {
		var err error
		if strings.Contains(q, "$1") {
			_, err = h.q().Exec(ctx, q, msgID)
		} else {
			_, err = h.q().Exec(ctx, q)
		}
		if err != nil {
			return false, nil, fmt.Errorf("%s: %w", q, err)
		}
	}

	// Only delete files inside the data directory that no other message
	// still uses.
	var files []string
	for _, p := range paths {
		clean, ok := safeDataPath(p, h.DataDir)
		if !ok {
			continue
		}
		var shared bool
		if err := h.q().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM msg WHERE filepath = $1)
			OR EXISTS (SELECT 1 FROM msg_attachment WHERE filepath = $1)`, p).Scan(&shared); err != nil {
			return false, nil, err
		}
		if !shared {
			files = append(files, clean)
		}
	}
	return true, files, nil
}
