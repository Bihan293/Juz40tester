package services

import (
	"fmt"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// TestBillingWeakReplaceKeptWhenNoTopics: the weak topics disappeared
// between the pre-checkout and the payment — the order is refunded and the
// student KEEPS the personal test they had (it used to be deleted first).
func TestBillingWeakReplaceKeptWhenNoTopics(t *testing.T) {
	e := newBillEnv(t)
	old := &models.Test{ID: 9_000_101}
	e.tests.current = old
	o, err := e.svc.OpenWeakOrder(e.ctx, e.user.ID, e.tgID, e.sid)
	if err != nil {
		t.Fatal(err)
	}
	if ok, msg := e.svc.PreCheckout(e.ctx, e.user.ID, "XTR", 10, billing.WeakTestPayload(o.ID)); !ok {
		t.Fatalf("pre-checkout: %s", msg)
	}
	e.tests.topics, e.build.topics = nil, nil
	out, err := e.svc.OnSuccessfulPayment(e.ctx, e.user.ID, e.tgID, PaymentInfo{Currency: "XTR", Amount: 10,
		Payload: billing.WeakTestPayload(o.ID), ChargeID: fmt.Sprintf("w%d-keep", e.tgID)})
	if err != nil || out.Kind != OutcomeWeakRefunded {
		t.Fatalf("outcome: %v %+v", err, out)
	}
	if len(e.tests.deleted) != 0 || e.tests.current == nil {
		t.Fatalf("the current test must survive a refunded order: deleted=%v", e.tests.deleted)
	}
	if e.build.calls != 0 {
		t.Fatalf("nothing must be built: %d", e.build.calls)
	}
}

// TestBillingAdminRefundStopsPaidOrder: an admin /refund (or a refund made
// by Telegram) of a weak-test payment whose test is still being built stops
// the order — the test is not generated after the Stars went back, and the
// Stars are refunded exactly once.
func TestBillingAdminRefundStopsPaidOrder(t *testing.T) {
	e := newBillEnv(t)
	e.build.pending = true
	charge := fmt.Sprintf("w%d-adm", e.tgID)
	o, out := e.payWeak(t, charge)
	if out.Kind != OutcomeWeakPending {
		t.Fatalf("outcome: %+v", out)
	}
	if ok, err := e.svc.AdminRefund(e.ctx, charge); err != nil || !ok {
		t.Fatalf("admin refund: %v %v", ok, err)
	}
	if got, _ := e.repo.WeakOrderByID(e.ctx, o.ID); got.Status != repositories.OrderRefunded {
		t.Fatalf("order must stop: %+v", got)
	}
	// The generation finishes later: the order is NOT fulfilled.
	e.build.mu.Lock()
	e.build.pending, e.build.test = false, &models.Test{ID: 9_000_111}
	e.build.mu.Unlock()
	for i := 0; i < 3; i++ {
		e.dueNow()
		e.svc.ReconcileOnce(e.ctx)
	}
	if got, _ := e.repo.WeakOrderByID(e.ctx, o.ID); got.Status != repositories.OrderRefunded {
		t.Fatalf("refunded order fulfilled: %+v", got)
	}
	// The reconciler is global (other tests' refunds may be due too):
	// count THIS charge only.
	count := func(xs []string) int {
		n := 0
		for _, x := range xs {
			if x == charge {
				n++
			}
		}
		return n
	}
	if count(e.stars.refunds) != 1 || count(e.notify.refunded) != 1 || len(e.notify.ready) != 0 {
		t.Fatalf("refunds=%v notices=%v ready=%v", e.stars.refunds, e.notify.refunded, e.notify.ready)
	}

	// Refund made by Telegram itself (refunded_payment update).
	e.build.mu.Lock()
	e.build.pending, e.build.test = true, nil
	e.build.mu.Unlock()
	charge2 := fmt.Sprintf("w%d-ext", e.tgID)
	o2, _ := e.payWeak(t, charge2)
	if err := e.svc.OnRefundedPayment(e.ctx, charge2); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.repo.WeakOrderByID(e.ctx, o2.ID); got.Status != repositories.OrderRefunded {
		t.Fatalf("externally refunded order must stop: %+v", got)
	}
}
