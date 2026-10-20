package app

import (
	"testing"
	"time"
)

func TestDueSlot(t *testing.T) {
	times := []string{"07:00", "12:00", "18:00"}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 20, h, m, 0, 0, time.UTC) }
	cases := []struct {
		now  time.Time
		want string
	}{
		{at(6, 59), ""},
		{at(7, 0), "07:00"},
		{at(11, 59), "07:00"},
		// サーバ停止で12:00を逃していても、その日の直近の枠だけを実行する
		{at(19, 30), "18:00"},
	}
	for _, c := range cases {
		slot, date := DueSlot(times, c.now)
		if slot != c.want || date != "2026-10-20" {
			t.Errorf("%v: got (%q, %q), want %q", c.now, slot, date, c.want)
		}
	}
	if slot, _ := DueSlot(nil, at(12, 0)); slot != "" {
		t.Errorf("時刻未設定なのに枠がある: %q", slot)
	}
}
