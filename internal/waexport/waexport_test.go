package waexport

import (
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

const sample = "[14/03/16, 10:39:27 AM] Sam: Hey there.\r\n" +
	"Where are you?\r\n" +
	"‎[14/03/16, 10:40:00 AM] Alex: ‎image omitted\r\n" +
	"‎[14/03/16, 10:40:01 AM] Alex: Look ‎image omitted\r\n" +
	"[14/03/16, 10:41:00 AM] Sam: ‎Missed voice call, ‎Tap to call back\r\n" +
	"[14/03/16, 10:42:00 AM] Sam: Hi @⁨Alex⁩ ‎<This message was edited>\r\n" +
	"[14/03/16, 10:43:00 AM] Sam:\r\n"

func parseSample(t *testing.T) []Line {
	t.Helper()
	lines, err := Parse(strings.NewReader(sample), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestParse(t *testing.T) {
	lines := parseSample(t)
	if len(lines) != 6 {
		t.Fatalf("got %d lines, want 6", len(lines))
	}
	want := []struct {
		kind Kind
		text string
	}{
		{KindText, "Hey there.\nWhere are you?"},
		{KindMedia, ""},
		{KindMedia, "Look"},
		{KindSystem, "Missed voice call, Tap to call back"},
		{KindText, "Hi @\u2068Alex\u2069"},
		{KindText, ""},
	}
	for i, w := range want {
		if lines[i].Kind != w.kind || lines[i].Text != w.text {
			t.Errorf("line %d = (%v, %q), want (%v, %q)", i, lines[i].Kind, lines[i].Text, w.kind, w.text)
		}
	}
	if got := lines[0].TS; !got.Equal(time.Date(2016, 3, 14, 10, 39, 27, 0, time.UTC)) {
		t.Errorf("ts = %v", got)
	}
}

func TestMatch(t *testing.T) {
	lines := parseSample(t)
	base := lines[0].TS.Unix()
	stored := []store.StoredMessage{
		{MsgID: "A", TS: base - 1, Text: "Hey there.\nWhere are you?"},               // 1s clock skew
		{MsgID: "B", TS: base + 33, FromMe: true, HasMedia: true},               // uncaptioned image
		{MsgID: "C", TS: base + 34, FromMe: true, HasMedia: true, Text: "Look"}, // captioned image
		{MsgID: "D", TS: base + 153, Text: "Hi @919999999999"},                  // mention stored as number
	}
	got := Match(lines, stored, "Alex", time.Time{})
	want := []Outcome{Duplicate, Skip, Duplicate, Skip, Duplicate, Skip}
	for i, w := range want {
		if got[i].Outcome != w {
			t.Errorf("line %d (%q) = %v, want %v", i, lines[i].Text, got[i].Outcome, w)
		}
	}

	// Without stored rows everything with text imports; a cutoff skips older lines.
	got = Match(lines, nil, "Alex", lines[4].TS)
	want = []Outcome{Skip, Skip, Skip, Skip, Import, Skip}
	for i, w := range want {
		if got[i].Outcome != w {
			t.Errorf("cutoff: line %d = %v, want %v", i, got[i].Outcome, w)
		}
	}
}

func TestMatchConsumesEachStoredRowOnce(t *testing.T) {
	ts := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	lines := []Line{{TS: ts, Sender: "Alex", Text: "Ok", key: "Ok"}, {TS: ts, Sender: "Alex", Text: "Ok", key: "Ok"}}
	stored := []store.StoredMessage{{MsgID: "A", TS: ts.Unix(), FromMe: true, Text: "Ok"}}
	got := Match(lines, stored, "Alex", time.Time{})
	if got[0].Outcome != Duplicate || got[1].Outcome != Import {
		t.Fatalf("got %v, %v; want Duplicate, Import", got[0].Outcome, got[1].Outcome)
	}
	if MsgID("c", lines[0], 0) == MsgID("c", lines[1], 1) {
		t.Fatal("identical lines in the same second must get distinct IDs")
	}
}
