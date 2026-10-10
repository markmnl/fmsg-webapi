package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// FmsgIDDetail is the part of fmsgid's GET /fmsgid/:address answer used to
// check quotas: whether the address accepts new messages, its limits (-1 is
// unlimited) and its usage. For an address in a quota pool, fmsgid answers
// with the pool owner's limits and the usage of the whole pool.
type FmsgIDDetail struct {
	AcceptingNew        bool  `json:"acceptingNew"`
	LimitRecvSizeTotal  int64 `json:"limitRecvSizeTotal"`
	LimitRecvSizePer1d  int64 `json:"limitRecvSizePer1d"`
	LimitRecvCountPer1d int64 `json:"limitRecvCountPer1d"`
	LimitSendSizeTotal  int64 `json:"limitSendSizeTotal"`
	LimitSendSizePerMsg int64 `json:"limitSendSizePerMsg"`
	LimitSendSizePer1d  int64 `json:"limitSendSizePer1d"`
	LimitSendCountPer1d int64 `json:"limitSendCountPer1d"`
	RecvSizeTotal       int64 `json:"recvSizeTotal"`
	RecvSizePer1d       int64 `json:"recvSizePer1d"`
	RecvCountPer1d      int64 `json:"recvCountPer1d"`
	SendSizeTotal       int64 `json:"sendSizeTotal"`
	SendSizePer1d       int64 `json:"sendSizePer1d"`
	SendCountPer1d      int64 `json:"sendCountPer1d"`
}

// FetchFmsgIDDetail fetches addr's detail from fmsgid, bypassing the lookup
// cache CheckFmsgID uses: usage changes with every message, so quota checks
// need it fresh. It returns fmsgid's HTTP status, and the detail only when
// that is 200. A limit fmsgid leaves out is unlimited. A 200 answer also
// refreshes the cached lookup CheckFmsgID uses.
func FetchFmsgIDDetail(idURL, addr string) (*FmsgIDDetail, int, error) {
	lookupURL := fmsgIDAddressURL(idURL, addr)
	req, err := http.NewRequest(http.MethodGet, lookupURL, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := doFmsgID(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}

	var raw struct {
		FmsgIDDetail
		AcceptingNew *bool `json:"acceptingNew"`
	}
	raw.LimitRecvSizeTotal, raw.LimitRecvSizePer1d, raw.LimitRecvCountPer1d = -1, -1, -1
	raw.LimitSendSizeTotal, raw.LimitSendSizePerMsg, raw.LimitSendSizePer1d, raw.LimitSendCountPer1d = -1, -1, -1, -1
	if err := decodeJSON(resp.Body, &raw); err != nil {
		return nil, 0, fmt.Errorf("decoding fmsgid address: %w", err)
	}
	if raw.AcceptingNew == nil {
		return nil, 0, errors.New("fmsgid address response is missing acceptingNew")
	}
	detail := raw.FmsgIDDetail
	detail.AcceptingNew = *raw.AcceptingNew

	fmsgIDCache.Store(lookupURL, fmsgIDEntry{
		expires:      time.Now().Add(fmsgIDCacheTTL),
		code:         http.StatusOK,
		acceptingNew: detail.AcceptingNew,
	})
	return &detail, http.StatusOK, nil
}

// RecordFmsgIDRecv records with fmsgid that addr received a message of size
// bytes at ts (seconds since the Unix epoch).
func RecordFmsgIDRecv(idURL, addr string, ts float64, size int64) error {
	return postFmsgIDUsage(idURL, "recv", addr, ts, size)
}

// RecordFmsgIDSend records with fmsgid that addr sent a message of size bytes
// at ts (seconds since the Unix epoch).
func RecordFmsgIDSend(idURL, addr string, ts float64, size int64) error {
	return postFmsgIDUsage(idURL, "send", addr, ts, size)
}

func postFmsgIDUsage(idURL, part, addr string, ts float64, size int64) error {
	payload, err := json.Marshal(map[string]any{"address": addr, "ts": ts, "size": size})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(idURL, "/")+"/fmsgid/"+part, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := doFmsgID(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST /fmsgid/%s returned %d", part, resp.StatusCode)
	}
	return nil
}

// doFmsgID sends req to fmsgid with the bounded-timeout client.
func doFmsgID(req *http.Request) (*http.Response, error) {
	return fmsgIDClient.Do(req) //nolint:gosec // URLs are built from trusted config
}
