package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/waexport"
	"github.com/spf13/cobra"
)

func newHistoryImportCmd(flags *rootFlags) *cobra.Command {
	var chatJID, file, me, since, tz string
	var apply, undo bool
	var samples int
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import a WhatsApp iOS chat export (_chat.txt) into the local store",
		Long: `Import a WhatsApp iOS "Export chat" text file into one existing chat.

Lines already stored by wacli (same direction, within 2 seconds, same text) are
skipped; everything else is inserted with a synthetic "import-" message ID that
is never sent to WhatsApp. System lines (calls, notices, deleted placeholders)
and media without a caption are skipped. Previews by default; pass --apply to
write. --undo removes every imported message from the chat.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if chatJID == "" {
				return fmt.Errorf("--chat is required")
			}
			if undo || apply {
				if err := flags.requireWritable(); err != nil {
					return err
				}
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			a, lk, err := newApp(ctx, flags, undo || apply, true)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)
			db := a.DB()

			chat, err := db.GetChat(chatJID)
			if err != nil {
				return fmt.Errorf("chat %s not found in local store: %w", chatJID, err)
			}
			if undo {
				n, err := db.DeleteImportedMessages(chat.JID)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stdout, "Removed %d imported message(s) from %s.\n", n, chat.Name)
				return nil
			}
			if file == "" || me == "" {
				return fmt.Errorf("--file and --me are required")
			}

			loc := time.Local
			if tz != "" {
				if loc, err = time.LoadLocation(tz); err != nil {
					return err
				}
			}
			var cutoff time.Time
			if since != "" {
				if cutoff, err = time.ParseInLocation("2006-01-02", since, loc); err != nil {
					return fmt.Errorf("--since: %w", err)
				}
			}
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()
			lines, err := waexport.Parse(f, loc)
			if err != nil {
				return err
			}
			stored, err := db.ChatMessagesForImport(chat.JID)
			if err != nil {
				return err
			}
			results := waexport.Match(lines, stored, me, cutoff)

			senders, err := senderJIDs(db, chat)
			if err != nil {
				return err
			}
			var oldestStored int64
			for _, s := range stored {
				if !store.IsImportedMsgID(s.MsgID) && (oldestStored == 0 || s.TS < oldestStored) {
					oldestStored = s.TS
				}
			}

			var toInsert []store.ImportedMessage
			var counts [4]int
			older, gaps := 0, 0
			var gapSamples, borderSamples []waexport.Line
			seen := map[string]int{}
			for _, r := range results {
				counts[r.Outcome]++
				if r.Outcome != waexport.Import && r.Outcome != waexport.Borderline {
					continue
				}
				l := r.Line
				if oldestStored != 0 && l.TS.Unix() >= oldestStored {
					gaps++
					if len(gapSamples) < samples {
						gapSamples = append(gapSamples, l)
					}
				} else {
					older++
				}
				if r.Outcome == waexport.Borderline && len(borderSamples) < samples {
					borderSamples = append(borderSamples, l)
				}
				base := fmt.Sprintf("%d|%s|%s", l.TS.Unix(), l.Sender, l.Text)
				id := waexport.MsgID(chat.JID, l, seen[base])
				seen[base]++
				fromMe := l.Sender == me
				m := store.ImportedMessage{MsgID: id, TS: l.TS.Unix(), FromMe: fromMe, SenderName: l.Sender, Text: l.Text}
				switch {
				case fromMe:
				case senders == nil:
					m.SenderJID = chat.JID
				default:
					m.SenderJID = senders[l.Sender]
				}
				toInsert = append(toInsert, m)
			}

			fmt.Fprintf(os.Stdout, "%s (%s)\n", chat.Name, chat.JID)
			fmt.Fprintf(os.Stdout, "  export lines         %d\n", len(lines))
			fmt.Fprintf(os.Stdout, "  already in wacli     %d\n", counts[waexport.Duplicate])
			fmt.Fprintf(os.Stdout, "  skipped              %d  (calls, notices, uncaptioned media, before --since)\n", counts[waexport.Skip])
			fmt.Fprintf(os.Stdout, "  to import            %d  (older history %d, gaps in synced period %d)\n", len(toInsert), older, gaps)
			fmt.Fprintf(os.Stdout, "  borderline           %d  (imported; a different message from the same side is within 2s)\n", counts[waexport.Borderline])
			printSamples("gap-fill samples", gapSamples)
			printSamples("borderline samples", borderSamples)
			if !apply {
				fmt.Fprintln(os.Stdout, "Preview only. Re-run with --apply to write.")
				return nil
			}
			n, err := db.InsertImportedMessages(chat.JID, chat.Name, toInsert)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "Imported %d message(s) (%d already imported earlier).\n", n, len(toInsert)-n)
			return nil
		},
	}
	cmd.Flags().StringVar(&chatJID, "chat", "", "chat JID to import into (must already exist)")
	cmd.Flags().StringVar(&file, "file", "", "path to the export's _chat.txt")
	cmd.Flags().StringVar(&me, "me", "", "your own name as it appears in the export")
	cmd.Flags().StringVar(&since, "since", "", "only import messages on or after this date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&tz, "tz", "", "time zone the export was made in (default: local)")
	cmd.Flags().BoolVar(&apply, "apply", false, "write to the store (default is a preview)")
	cmd.Flags().BoolVar(&undo, "undo", false, "remove all imported messages from the chat")
	cmd.Flags().IntVar(&samples, "samples", 5, "sample lines to show per category")
	return cmd
}

// senderJIDs maps group sender names to JIDs using contact names that match
// exactly one contact. It returns nil for DMs, where the sender is the chat.
func senderJIDs(db *store.DB, chat store.Chat) (map[string]string, error) {
	m := map[string]string{}
	if !strings.HasSuffix(chat.JID, "@g.us") {
		return nil, nil
	}
	contacts, err := db.ListContacts(0)
	if err != nil {
		return nil, err
	}
	owners := map[string]map[string]bool{}
	for _, c := range contacts {
		for _, n := range []string{c.Alias, c.SystemName, c.Name} {
			if n = strings.TrimSpace(n); n != "" {
				if owners[n] == nil {
					owners[n] = map[string]bool{}
				}
				owners[n][c.JID] = true
			}
		}
	}
	for name, jids := range owners {
		if len(jids) == 1 {
			for j := range jids {
				m[name] = j
			}
		}
	}
	return m, nil
}

func printSamples(title string, lines []waexport.Line) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(os.Stdout, "  %s:\n", title)
	for _, l := range lines {
		text := strings.ReplaceAll(l.Text, "\n", " ")
		if r := []rune(text); len(r) > 70 {
			text = string(r[:70]) + "…"
		}
		fmt.Fprintf(os.Stdout, "    %s  %s: %s\n", l.TS.Format("2006-01-02 15:04:05"), l.Sender, text)
	}
}
