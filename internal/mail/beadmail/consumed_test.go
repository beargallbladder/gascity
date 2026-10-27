package beadmail

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

// Every successful read, mark-read or archive of a message, the auto-handoff
// archives included, reports it to the consumed hook (OnConsumed); a failed
// one, and a mark-unread, does not.
func TestOnConsumedReportsEachConsumedMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(p *Provider, id string) error
	}{
		{"read", func(p *Provider, id string) error { _, err := p.Read(id); return err }},
		{"mark read", func(p *Provider, id string) error { return p.MarkRead(id) }},
		{"archive", func(p *Provider, id string) error { return p.Archive(id) }},
		{"delete", func(p *Provider, id string) error { return p.Delete(id) }},
		{"archive many", func(p *Provider, id string) error { _, err := p.ArchiveMany([]string{id}); return err }},
		{"delete many", func(p *Provider, id string) error { _, err := p.DeleteMany([]string{id}); return err }},
		{"archive matching", func(p *Provider, _ string) error {
			_, _, err := p.ArchiveMatching(ArchiveFilter{From: "alice", IncludeRead: true})
			return err
		}},
		{"archive injected handoffs", func(p *Provider, id string) error { return p.ArchiveInjectedAutoHandoffs([]string{id}) }},
		{"archive injected handoffs from inbox", func(p *Provider, id string) error {
			inbox, err := p.InboxForRoutes([]string{"bob"})
			if err != nil {
				return err
			}
			return p.ArchiveInjectedAutoHandoffsFromInbox(inbox, []string{id}, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			p := New(store)
			var consumed []string
			p.OnConsumed(func(id string) { consumed = append(consumed, id) })
			msg, err := store.Create(beads.Bead{
				Title: "hi", Type: messageBeadType, Assignee: "bob", From: "alice",
				Labels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.do(p, msg.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !slices.Equal(consumed, []string{msg.ID}) {
				t.Fatalf("consumed = %v, want [%s]", consumed, msg.ID)
			}
		})
	}
	t.Run("not consumed", func(t *testing.T) {
		store := beads.NewMemStore()
		p := New(store)
		var consumed []string
		p.OnConsumed(func(id string) { consumed = append(consumed, id) })
		msg, err := p.Send("alice", "bob", "hi", "body")
		if err != nil {
			t.Fatal(err)
		}
		if err := p.MarkUnread(msg.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Read("gc-missing"); err == nil {
			t.Fatal("Read of a missing message succeeded")
		}
		if err := p.Archive(msg.ID); err != nil {
			t.Fatal(err)
		}
		if err := p.Archive(msg.ID); err == nil {
			t.Fatal("a second Archive succeeded")
		}
		if !slices.Equal(consumed, []string{msg.ID}) {
			t.Fatalf("consumed = %v, want only the first archive's [%s]", consumed, msg.ID)
		}
	})
}
