package config

import (
	"fmt"
	"testing"
)

func TestParseTimes(t *testing.T) {
	got, err := ParseTimes("18:00, 7:00、12:30 07:00")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[07:00 12:30 18:00]" {
		t.Errorf("got %v", got)
	}
	if got, _ := ParseTimes(""); len(got) != 0 {
		t.Errorf("空文字列: %v", got)
	}
	if _, err := ParseTimes("25:00"); err == nil {
		t.Error("不正な時刻がエラーにならない")
	}
}

func TestRoundTrip(t *testing.T) {
	s := Defaults()
	s.DigestTimes = []string{"07:00", "19:00"}
	s.WebFetch = false
	s.UserMemory = "Rustが好き"
	got := Load(s.Map())
	if fmt.Sprint(got) != fmt.Sprint(s) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, s)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("デフォルト値が検証を通らない: %v", err)
	}
}

func TestValidate(t *testing.T) {
	s := Defaults()
	s.Backend = "openai"
	if s.Validate() == nil {
		t.Error("不正なbackendが通る")
	}
	s = Defaults()
	s.FetchInterval = 1
	if s.Validate() == nil {
		t.Error("短すぎる取得間隔が通る")
	}
	s = Defaults()
	s.Timezone = "Mars/Base"
	if s.Validate() == nil {
		t.Error("不正なタイムゾーンが通る")
	}
}
