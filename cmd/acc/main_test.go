package main

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "0s"},
		{0, "0s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m 00s"},
		{12*time.Minute + 5*time.Second, "12m 05s"},
		{59*time.Minute + 59*time.Second, "59m 59s"},
		{time.Hour, "1h 00m"},
		{time.Hour + 2*time.Minute + 59*time.Second, "1h 02m"},
		{23*time.Hour + 59*time.Minute, "23h 59m"},
		{24 * time.Hour, "1d 0h 00m"},
		{3*24*time.Hour + 4*time.Hour + 12*time.Minute + 30*time.Second, "3d 4h 12m"},
		{497 * 24 * time.Hour, "497d 0h 00m"},
		{1500 * time.Millisecond, "2s"},
	}
	for _, tc := range tests {
		if got := formatDuration(tc.d); got != tc.want {
			t.Errorf("formatDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestFormatSeen(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 30, 0, 0, time.Local)
	tests := []struct {
		t    time.Time
		want string
	}{
		{now.Add(-time.Minute), "15:29"},
		{time.Date(2026, 10, 14, 0, 5, 0, 0, time.Local), "00:05"},
		{time.Date(2026, 10, 13, 23, 59, 0, 0, time.Local), "13 Oct 23:59"},
		{time.Date(2026, 1, 2, 9, 0, 0, 0, time.Local), "2 Jan 09:00"},
		{time.Date(2025, 10, 14, 15, 30, 0, 0, time.Local), "14 Oct 15:30"},
	}
	for _, tc := range tests {
		if got := formatSeen(tc.t, now); got != tc.want {
			t.Errorf("formatSeen(%s) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestFormatUptime(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 30, 0, 0, time.Local)
	secs := int64(3*86400 + 4*3600 + 12*60)
	zero := int64(0)
	tests := []struct {
		name string
		n    Nas
		want string
	}{
		{"online with uptime", Nas{Online: true, UptimeSeconds: &secs, UptimeSource: "snmp"}, "3d 4h 12m"},
		{"online, just booted", Nas{Online: true, UptimeSeconds: &zero, UptimeSource: "snmp"}, "0s"},
		{"online without SNMP", Nas{Online: true, LastSeen: now}, "-"},
		{"offline, seen today", Nas{LastSeen: now.Add(-2 * time.Hour)}, "last seen 13:30"},
		{"offline, seen yesterday", Nas{LastSeen: now.Add(-24 * time.Hour)}, "last seen 13 Oct 15:30"},
		{"offline with a stale uptime", Nas{UptimeSeconds: &secs, LastSeen: now}, "last seen 15:30"},
		{"never seen", Nas{}, "never seen"},
	}
	for _, tc := range tests {
		if got := formatUptime(tc.n, now); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseSeconds(t *testing.T) {
	good := map[string]time.Duration{
		"4": 4 * time.Second, "0.5": 500 * time.Millisecond, "30": 30 * time.Second, "1e3": 1000 * time.Second,
	}
	for in, want := range good {
		if got, err := parseSeconds("--timeout", in); err != nil || got != want {
			t.Errorf("parseSeconds(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-1", "abc", "NaN", "Inf", "-Inf", "1e300", "1000001", "4s"} {
		if got, err := parseSeconds("--timeout", in); err == nil {
			t.Errorf("parseSeconds(%q) = %s, want an error", in, got)
		}
	}
}

func TestOnlineOnly(t *testing.T) {
	list := []Nas{{Name: "A", Online: true}, {Name: "B"}, {Name: "C", Online: true}}
	got := onlineOnly(list)
	if len(got) != 2 || got[0].Name != "A" || got[1].Name != "C" {
		t.Errorf("got %v", got)
	}
	if got := onlineOnly(nil); len(got) != 0 {
		t.Errorf("got %v from nil", got)
	}
}
