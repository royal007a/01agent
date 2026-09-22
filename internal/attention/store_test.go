package attention

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInboxPriorityCoalescingAndSingleLease(t *testing.T) {
	store := newTestStore(t)
	publish(t, store, "op-low", "m-low", "general", PrioritySubscription)
	publish(t, store, "op-high", "m-high", "urgent", PriorityCorrection)
	publish(t, store, "op-more", "m-more", "general", PriorityDirect)
	state, err := store.GetState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Inbox) != 2 {
		t.Fatalf("inbox items=%d want=2", len(state.Inbox))
	}
	claim, err := store.Claim(context.Background(), "agent", "op-claim", "lease", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Item.ConversationID != "urgent" || claim.Item.Priority != PriorityCorrection {
		t.Fatalf("claimed=%+v", claim.Item)
	}
	if _, err := store.Claim(context.Background(), "agent", "op-second", "lease-2", time.Minute); !errors.Is(err, ErrLease) {
		t.Fatalf("second claim err=%v want ErrLease", err)
	}
}

func TestExpiredClaimIsRequeued(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	publish(t, store, "op-publish", "message", "chat", PriorityDirect)
	first, err := store.Claim(context.Background(), "agent", "op-claim-1", "lease-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	second, err := store.Claim(context.Background(), "agent", "op-claim-2", "lease-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.Item.ID != second.Item.ID || second.Item.LeaseID != "lease-2" {
		t.Fatalf("reclaim=%+v", second.Item)
	}
}

func TestWorkMarkSurvivesAckAndRestartUntilExplicitClear(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, store, "op-publish", "message", "chat", PriorityDirect)
	claim, err := store.Claim(context.Background(), "agent", "op-claim", "lease", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mark, err := store.SetWorkMark(context.Background(), WorkMarkInput{OperationID: "op-mark", AgentID: "agent", ConversationID: "chat", TaskID: "task", Reason: "follow up"})
	if err != nil || !mark.Active {
		t.Fatalf("mark=%+v err=%v", mark, err)
	}
	if _, err := store.Ack(context.Background(), claim.Item.ID, "op-ack", "agent", "lease"); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopened.GetState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !state.WorkMarks[markKey("agent", "chat")].Active {
		t.Fatal("ack or restart cleared persistent work mark")
	}
	cleared, err := reopened.ClearWorkMark(context.Background(), "agent", "chat", "op-clear")
	if err != nil || cleared.Active {
		t.Fatalf("clear=%+v err=%v", cleared, err)
	}
}

func TestFreshReplyAtomicallyChecksSequenceAndAcknowledges(t *testing.T) {
	store := newTestStore(t)
	publish(t, store, "op-publish", "message", "chat", PriorityDirect)
	claim, err := store.Claim(context.Background(), "agent", "op-claim", "lease", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.SendFresh(context.Background(), FreshReplyInput{
		OperationID: "op-reply", MessageID: "reply", AgentID: "agent", ItemID: claim.Item.ID,
		LeaseID: "lease", ReadSeq: claim.Item.ClaimReadSeq, Content: "answer",
	})
	if err != nil || result.Sent == nil || result.Sent.Seq != 2 {
		t.Fatalf("reply=%+v err=%v", result, err)
	}
	state, err := store.GetState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Inbox[claim.Item.ID].State != Done || state.Conversations["chat"].LastSeq != 2 {
		t.Fatalf("state=%+v", state)
	}
}

func TestFreshnessBarrierPreservesDraftThenRefreshes(t *testing.T) {
	store := newTestStore(t)
	publish(t, store, "op-publish", "message-1", "chat", PriorityDirect)
	claim, err := store.Claim(context.Background(), "agent", "op-claim", "lease", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	publish(t, store, "op-correction", "message-2", "chat", PriorityCorrection)
	stale, err := store.SendFresh(context.Background(), FreshReplyInput{
		OperationID: "op-stale", MessageID: "reply-old", AgentID: "agent", ItemID: claim.Item.ID,
		LeaseID: "lease", ReadSeq: claim.Item.ClaimReadSeq, Content: "old answer",
	})
	if !errors.Is(err, ErrStale) || stale.Draft == nil || len(stale.Missed) != 1 || stale.Missed[0].ID != "message-2" {
		t.Fatalf("stale=%+v err=%v", stale, err)
	}
	state, err := store.GetState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Conversations["chat"].LastSeq != 2 || state.Inbox[claim.Item.ID].State != Claimed {
		t.Fatal("stale reply escaped barrier or acknowledged work")
	}
	refreshed, err := store.Refresh(context.Background(), claim.Item.ID, "op-refresh", "agent", "lease")
	if err != nil || len(refreshed.Messages) != 1 || refreshed.Item.ClaimReadSeq != 2 {
		t.Fatalf("refresh=%+v err=%v", refreshed, err)
	}
	result, err := store.SendFresh(context.Background(), FreshReplyInput{
		OperationID: "op-revised", MessageID: "reply-new", AgentID: "agent", ItemID: claim.Item.ID,
		LeaseID: "lease", ReadSeq: refreshed.Item.ClaimReadSeq, Content: "revised answer",
	})
	if err != nil || result.Sent == nil || result.Sent.Seq != 3 {
		t.Fatalf("revised=%+v err=%v", result, err)
	}
	replayed, err := store.SendFresh(context.Background(), FreshReplyInput{
		OperationID: "op-stale", MessageID: "reply-old", AgentID: "agent", ItemID: claim.Item.ID,
		LeaseID: "lease", ReadSeq: claim.Item.ClaimReadSeq, Content: "old answer",
	})
	if !errors.Is(err, ErrStale) || replayed.Draft == nil || replayed.Draft.Content != "old answer" {
		t.Fatalf("stale replay=%+v err=%v", replayed, err)
	}
}

func TestPublishOperationIsSemanticIdempotent(t *testing.T) {
	store := newTestStore(t)
	input := PublishInput{OperationID: "op", MessageID: "message", ConversationID: "chat", AuthorID: "human", Kind: "human", Content: "hello", Deliveries: []Delivery{{AgentID: "agent", Priority: PriorityDirect}}}
	first, _, err := store.Publish(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.Publish(context.Background(), input)
	if err != nil || first.Seq != second.Seq {
		t.Fatalf("retry=%+v err=%v", second, err)
	}
	input.Content = "changed"
	if _, _, err := store.Publish(context.Background(), input); !errors.Is(err, ErrConflict) {
		t.Fatalf("semantic conflict err=%v", err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func publish(t *testing.T, store *Store, operationID, messageID, conversationID string, priority Priority) {
	t.Helper()
	_, _, err := store.Publish(context.Background(), PublishInput{
		OperationID: operationID, MessageID: messageID, ConversationID: conversationID,
		AuthorID: "human", Kind: "human", Content: messageID,
		Deliveries: []Delivery{{AgentID: "agent", Priority: priority}},
	})
	if err != nil {
		t.Fatal(err)
	}
}
