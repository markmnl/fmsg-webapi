// Package edit implements the FMSG-008 edit document: a result size followed
// by copy and insert operations that derive a message's edited data from its
// original data.
package edit

import (
	"encoding/binary"
	"errors"
)

// MediaType is the type of an FMSG-008 edit message.
const MediaType = "application/vnd.fmsg.edit"

const (
	opCopy   = 0
	opInsert = 1

	sizeLen   = 4 // result size
	copyLen   = 9 // code, offset, length
	insertLen = 5 // code, length; literal bytes follow
)

var (
	// ErrMalformed reports a document that is not a well-formed edit
	// document for the subject it was tested against.
	ErrMalformed = errors.New("malformed edit document")
	// ErrTooLarge reports a well-formed document whose result exceeds the
	// caller's limit.
	ErrTooLarge = errors.New("edited data exceeds limit")
)

// Check reports whether doc is a well-formed edit document for subject data
// of subjectLen bytes, returning the byte length of the edited data.
func Check(doc []byte, subjectLen int) (uint32, error) {
	return walk(doc, uint64(subjectLen), nil)
}

// Apply returns the edited data doc produces from subject. limit bounds the
// result size and is checked before anything is allocated; a negative limit
// means no limit.
func Apply(doc, subject []byte, limit int64) ([]byte, error) {
	size, err := Check(doc, len(subject))
	if err != nil {
		return nil, err
	}
	if limit >= 0 && int64(size) > limit {
		return nil, ErrTooLarge
	}
	out := make([]byte, 0, size)
	if _, err := walk(doc, uint64(len(subject)), func(isCopy bool, a, b uint64) {
		if isCopy {
			out = append(out, subject[a:a+b]...)
		} else {
			out = append(out, doc[a:a+b]...)
		}
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// walk validates doc, calling emit for each operation: for a copy with the
// subject offset and length, for an insert with the offset of its literal
// bytes within doc and their length.
func walk(doc []byte, subjectLen uint64, emit func(isCopy bool, a, b uint64)) (uint32, error) {
	if len(doc) < sizeLen {
		return 0, ErrMalformed
	}
	size := binary.LittleEndian.Uint32(doc)
	var total uint64
	for p := sizeLen; p < len(doc); {
		switch doc[p] {
		case opCopy:
			if len(doc)-p < copyLen {
				return 0, ErrMalformed
			}
			off := uint64(binary.LittleEndian.Uint32(doc[p+1:]))
			n := uint64(binary.LittleEndian.Uint32(doc[p+5:]))
			if n == 0 || off+n > subjectLen {
				return 0, ErrMalformed
			}
			if emit != nil {
				emit(true, off, n)
			}
			total += n
			p += copyLen
		case opInsert:
			if len(doc)-p < insertLen {
				return 0, ErrMalformed
			}
			n := uint64(binary.LittleEndian.Uint32(doc[p+1:]))
			if n == 0 || n > uint64(len(doc)-p-insertLen) {
				return 0, ErrMalformed
			}
			if emit != nil {
				emit(false, uint64(p+insertLen), n)
			}
			total += n
			p += insertLen + int(n)
		default:
			return 0, ErrMalformed
		}
		if total > uint64(size) {
			return 0, ErrMalformed
		}
	}
	if total != uint64(size) {
		return 0, ErrMalformed
	}
	return size, nil
}

// Diff returns a compact edit document producing target from subject. It
// keeps the bytes the two share at the start and at the end and inserts what
// lies between, or inserts the whole target when that is no larger.
func Diff(subject, target []byte) []byte {
	prefix := 0
	for prefix < len(subject) && prefix < len(target) && subject[prefix] == target[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(subject)-prefix && suffix < len(target)-prefix &&
		subject[len(subject)-1-suffix] == target[len(target)-1-suffix] {
		suffix++
	}
	// A copy costs 9 bytes; one shorter than that is cheaper inserted.
	if prefix <= copyLen {
		prefix = 0
	}
	if suffix <= copyLen {
		suffix = 0
	}
	middle := target[prefix : len(target)-suffix]

	doc := make([]byte, sizeLen, sizeLen+2*copyLen+insertLen+len(middle))
	binary.LittleEndian.PutUint32(doc, uint32(len(target)))
	if prefix > 0 {
		doc = appendCopy(doc, 0, prefix)
	}
	if len(middle) > 0 {
		doc = append(doc, opInsert)
		doc = binary.LittleEndian.AppendUint32(doc, uint32(len(middle)))
		doc = append(doc, middle...)
	}
	if suffix > 0 {
		doc = appendCopy(doc, len(subject)-suffix, suffix)
	}
	return doc
}

func appendCopy(doc []byte, offset, length int) []byte {
	doc = append(doc, opCopy)
	doc = binary.LittleEndian.AppendUint32(doc, uint32(offset))
	return binary.LittleEndian.AppendUint32(doc, uint32(length))
}
