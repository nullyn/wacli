// Package waexport parses WhatsApp iOS "Export chat" text files and matches
// their lines against messages already stored by wacli.
package waexport

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/store"
)

type Kind int

const (
	KindText   Kind = iota
	KindMedia       // photo, video, voice note, document, sticker…; Text holds the caption
	KindSystem      // calls, encryption notices, group events, deleted placeholders
)

type Line struct {
	TS     time.Time
	Sender string
	Text   string // cleaned text to store
	Kind   Kind
	key    string // normalized text for matching
}

const (
	lrm      = "\u200e"
	editMark = "<This message was edited>"
)

var (
	header    = regexp.MustCompile(`^\x{200e}?\[(\d\d/\d\d/\d\d), (\d{1,2}:\d\d:\d\d)[ \x{202f}]?([AP]M)\] ([^:]+?):(?: (.*))?$`)
	mediaTail = regexp.MustCompile(`\x{200e}?(image|video|audio|sticker|GIF|document|Contact card|video note) omitted$`)
	// Mention isolates (U+2068/U+2069) are kept so the stored text can still be
	// matched to the synced copy, which stores mentions as phone numbers.
	invisible = strings.NewReplacer(lrm, "", "\u2060", "", "\u202f", " ", "\r", "")
)

// Parse reads an iOS export. Timestamps are interpreted in loc, which must be
// the phone's time zone when the export was made.
func Parse(r io.Reader, loc *time.Location) ([]Line, error) {
	var lines []Line
	var cur *Line
	var body strings.Builder
	flush := func() {
		if cur == nil {
			return
		}
		classify(cur, body.String())
		lines = append(lines, *cur)
		cur = nil
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		raw := strings.TrimSuffix(sc.Text(), "\r")
		m := header.FindStringSubmatch(raw)
		if m == nil {
			if cur != nil {
				body.WriteString("\n" + raw)
			}
			continue
		}
		flush()
		ts, err := time.ParseInLocation("02/01/06 3:04:05 PM", m[1]+" "+m[2]+" "+m[3], loc)
		if err != nil {
			return nil, fmt.Errorf("bad timestamp %q: %w", m[1]+" "+m[2], err)
		}
		cur = &Line{TS: ts, Sender: strings.TrimPrefix(m[4], lrm)}
		body.Reset()
		body.WriteString(m[5])
	}
	flush()
	return lines, sc.Err()
}

func classify(l *Line, body string) {
	body = strings.TrimSpace(strings.ReplaceAll(body, lrm+editMark, editMark))
	body = strings.TrimSpace(strings.TrimSuffix(body, editMark))
	switch {
	case mediaTail.MatchString(body):
		l.Kind = KindMedia
		body = mediaTail.ReplaceAllString(body, "")
	case strings.HasPrefix(body, lrm+"Location: "):
		l.Kind = KindMedia
	case strings.HasPrefix(body, lrm):
		// iOS prefixes every system notice with LRM: calls, "This message was
		// deleted", encryption and security-code notices, group membership.
		l.Kind = KindSystem
	}
	l.key = normalize(body)
	l.Text = strings.TrimSpace(invisible.Replace(body))
}

func normalize(s string) string { return store.ImportMatchKey(s) }

type Outcome int

const (
	Duplicate  Outcome = iota // already stored by wacli
	Import                    // not stored; will be inserted
	Borderline                // imported, but a different stored message from the same side is within the window
	Skip                      // system line, empty text, or before the cutoff
)

// Window is how far export and stored timestamps may differ for one message.
// Measured: exports equal the stored time or are one second later.
const Window = 2

type Result struct {
	Line    Line
	Outcome Outcome
}

// Match decides, for each export line, whether wacli already has it. Each
// stored message can absorb at most one export line.
func Match(lines []Line, stored []store.StoredMessage, me string, since time.Time) []Result {
	used := make([]bool, len(stored))
	keys := make([]string, len(stored))
	for i, s := range stored {
		keys[i] = normalize(s.Text)
	}
	results := make([]Result, len(lines))
	for i, l := range lines {
		results[i].Line = l
		if l.Kind == KindSystem || l.Text == "" || l.TS.Before(since) {
			results[i].Outcome = Skip
			// Uncaptioned media is stored by wacli too; consume its row so it
			// can't absorb a different line.
			if l.Kind == KindMedia {
				if j := find(l, stored, used, keys, me, true); j >= 0 {
					used[j] = true
				}
			}
			continue
		}
		if j := find(l, stored, used, keys, me, false); j >= 0 {
			used[j] = true
			results[i].Outcome = Duplicate
			continue
		}
		results[i].Outcome = Import
		if nearby(l, stored, used, me) {
			results[i].Outcome = Borderline
		}
	}
	return results
}

func find(l Line, stored []store.StoredMessage, used []bool, keys []string, me string, mediaOnly bool) int {
	fromMe := l.Sender == me
	ts := l.TS.Unix()
	fallback := -1
	for j, s := range stored {
		if used[j] || s.FromMe != fromMe || abs(s.TS-ts) > Window {
			continue
		}
		if !mediaOnly && keys[j] == l.key {
			return j
		}
		// Locations, contact cards and media render as text in exports but are
		// stored as media or with no text; treat them as the same message.
		if fallback < 0 && (s.HasMedia || keys[j] == "") {
			fallback = j
		}
	}
	return fallback
}

func nearby(l Line, stored []store.StoredMessage, used []bool, me string) bool {
	ts := l.TS.Unix()
	for j, s := range stored {
		if !used[j] && s.FromMe == (l.Sender == me) && abs(s.TS-ts) <= Window {
			return true
		}
	}
	return false
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// MsgID is deterministic so re-importing the same export inserts nothing new.
// occurrence separates identical lines sent in the same second.
func MsgID(chatJID string, l Line, occurrence int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%d", chatJID, l.TS.Unix(), l.Sender, l.Text, occurrence)))
	return store.ImportedMsgIDPrefix + hex.EncodeToString(h[:12])
}
