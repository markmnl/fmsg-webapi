package handlers

import (
	"net/http"
	"testing"

	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// unlimited is a detail with every limit unlimited and plenty of usage.
func unlimited() middleware.FmsgIDDetail {
	return middleware.FmsgIDDetail{
		AcceptingNew:       true,
		LimitRecvSizeTotal: -1, LimitRecvSizePer1d: -1, LimitRecvCountPer1d: -1,
		LimitSendSizeTotal: -1, LimitSendSizePerMsg: -1, LimitSendSizePer1d: -1, LimitSendCountPer1d: -1,
		RecvSizeTotal: 1 << 40, RecvSizePer1d: 1 << 40, RecvCountPer1d: 1 << 40,
		SendSizeTotal: 1 << 40, SendSizePer1d: 1 << 40, SendCountPer1d: 1 << 40,
	}
}

func TestLocalDeliveryCode(t *testing.T) {
	with := func(f func(*middleware.FmsgIDDetail)) *middleware.FmsgIDDetail {
		d := unlimited()
		f(&d)
		return &d
	}
	tests := []struct {
		name   string
		status int
		detail *middleware.FmsgIDDetail
		size   int64
		want   int
		ok     bool
	}{
		{"unknown", http.StatusNotFound, nil, 10, responseCodeUserUnknown, true},
		{"unexpected status", http.StatusInternalServerError, nil, 10, 0, false},
		{"not accepting", http.StatusOK, with(func(d *middleware.FmsgIDDetail) { d.AcceptingNew = false }), 10, responseCodeUserNotAccepting, true},
		{"unlimited", http.StatusOK, with(func(*middleware.FmsgIDDetail) {}), 1 << 30, responseCodeAccept, true},
		{"count fits", http.StatusOK, with(func(d *middleware.FmsgIDDetail) { d.LimitRecvCountPer1d, d.RecvCountPer1d = 10, 9 }), 10, responseCodeAccept, true},
		{"count full", http.StatusOK, with(func(d *middleware.FmsgIDDetail) { d.LimitRecvCountPer1d, d.RecvCountPer1d = 10, 10 }), 10, responseCodeUserFull, true},
		{"day size fits exactly", http.StatusOK, with(func(d *middleware.FmsgIDDetail) { d.LimitRecvSizePer1d, d.RecvSizePer1d = 100, 90 }), 10, responseCodeAccept, true},
		{"day size over", http.StatusOK, with(func(d *middleware.FmsgIDDetail) { d.LimitRecvSizePer1d, d.RecvSizePer1d = 100, 91 }), 10, responseCodeUserFull, true},
		{"storage over", http.StatusOK, with(func(d *middleware.FmsgIDDetail) { d.LimitRecvSizeTotal, d.RecvSizeTotal = 1000, 995 }), 10, responseCodeUserFull, true},
		{"zero limit", http.StatusOK, with(func(d *middleware.FmsgIDDetail) { d.LimitRecvCountPer1d, d.RecvCountPer1d = 0, 0 }), 0, responseCodeUserFull, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := localDeliveryCode(tt.status, tt.detail, tt.size)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("localDeliveryCode = %d, %v; want %d, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCheckSendLimits(t *testing.T) {
	tests := []struct {
		name   string
		set    func(*middleware.FmsgIDDetail)
		size   int64
		status int // 0 when the message may be sent
		code   string
	}{
		{"unlimited", func(*middleware.FmsgIDDetail) {}, 1 << 30, 0, ""},
		{"per message fits", func(d *middleware.FmsgIDDetail) { d.LimitSendSizePerMsg = 100 }, 100, 0, ""},
		{"per message over", func(d *middleware.FmsgIDDetail) { d.LimitSendSizePerMsg = 100 }, 101, http.StatusRequestEntityTooLarge, "send_size_per_msg_limit"},
		{"count fits", func(d *middleware.FmsgIDDetail) { d.LimitSendCountPer1d, d.SendCountPer1d = 5, 4 }, 1, 0, ""},
		{"count reached", func(d *middleware.FmsgIDDetail) { d.LimitSendCountPer1d, d.SendCountPer1d = 5, 5 }, 1, http.StatusTooManyRequests, "send_count_per_1d_limit"},
		{"day size over", func(d *middleware.FmsgIDDetail) { d.LimitSendSizePer1d, d.SendSizePer1d = 1000, 950 }, 51, http.StatusTooManyRequests, "send_size_per_1d_limit"},
		{"storage over", func(d *middleware.FmsgIDDetail) { d.LimitSendSizeTotal, d.SendSizeTotal = 1000, 950 }, 51, http.StatusUnprocessableEntity, "send_size_total_limit"},
		{"storage fits exactly", func(d *middleware.FmsgIDDetail) { d.LimitSendSizeTotal, d.SendSizeTotal = 1000, 950 }, 50, 0, ""},
		{"per message checked first", func(d *middleware.FmsgIDDetail) {
			d.LimitSendSizePerMsg = 10
			d.LimitSendCountPer1d, d.SendCountPer1d = 1, 1
		}, 11, http.StatusRequestEntityTooLarge, "send_size_per_msg_limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := unlimited()
			tt.set(&d)
			got := checkSendLimits(&d, tt.size)
			if tt.status == 0 {
				if got != nil {
					t.Fatalf("refused: %+v", *got)
				}
				return
			}
			if got == nil || got.status != tt.status || got.code != tt.code || got.message == "" {
				t.Fatalf("checkSendLimits = %+v, want %d %s", got, tt.status, tt.code)
			}
		})
	}
}
