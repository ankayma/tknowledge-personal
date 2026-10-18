package tka_test

import (
	"context"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// "agent" is whichever AI writes through the grant (owner 14/10/2026); which
// one it was is the row's producer, from the grant — ChatGPT writing D-130 was
// stored as claude because neither said otherwise.
func TestAgentWritesUnderItsOwnProducer(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	a.ActorID = "chatgpt"

	d, err := r.AddDocument(ctx, a, tka.DocumentInput{Text: "Gemini listed the store's principles.", TextProducer: "agent",
		Method: "opened_source", SourceURI: "https://gemini.google.com/app/1"})
	if err != nil || d.Producer != "chatgpt" || d.TextProducer == nil || *d.TextProducer != "agent" {
		t.Fatalf("document: %+v %v", d, err)
	}
	th, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Other AIs on the store", Keywords: []string{"cross-ai"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Link(ctx, a, tka.LinkInput{Thread: th.ID, IDs: []string{d.ID}}); err != nil {
		t.Fatal(err)
	}
	s, err := r.Summarize(ctx, a, tka.SummarizeInput{Thread: th.ID, Text: "ChatGPT wrote the test; Gemini ran it (" + d.ID + ").", Author: "agent"})
	if err != nil || s.Author != "agent" || s.Producer != "chatgpt" {
		t.Fatalf("summary: %+v %v", s, err)
	}
	// The older name still works.
	if _, err := r.AddDocument(ctx, a, tka.DocumentInput{Text: "older client", TextProducer: "claude",
		Method: "opened_source", SourceURI: "https://example.com/older"}); err != nil {
		t.Fatalf("claude stays accepted: %v", err)
	}
}
