package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/metrics"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
)

// --- Subscriptions / Telegram Stars UI ------------------------------------------

// Billing callbacks.
const (
	cbPlans   = "sub:menu"  // «⭐ Подписка / Тарифы» screen
	cbBuyPlan = "sub:buy:"  // + plan code: invoice link of the plan
	cbWeakBuy = "weak:buy:" // + subjectID: invoice of a new weak-topics test
)

// kbPlans is the reply-keyboard button of the subscription screen.
const kbPlans = "⭐ Подписка"

// WithBilling turns the subscriptions on (nil = off: no quota, no plans
// screen, free weak-topics tests — the old behaviour).
func (h *Handler) WithBilling(b *services.BillingService) *Handler {
	h.billing = b
	return h
}

// WithAdmins installs the administrator check (ADMIN_IDS) for /grant,
// /revoke, /subinfo, /refund.
func (h *Handler) WithAdmins(isAdmin func(tgUserID int64) bool) *Handler {
	h.isAdmin = isAdmin
	return h
}

// paidWeakTests reports whether weak-topics tests are sold for Stars.
func (h *Handler) paidWeakTests() bool {
	return h.billing != nil && h.billing.WeakTestPrice() > 0
}

// invoiceLinks caches the createInvoiceLink result per plan and price: a
// link is not bound to a user (the payer is the From of the payment), so
// one link per plan serves everybody — no Bot API call per tap.
var invoiceLinks sync.Map // "plan:price" -> link

func (h *Handler) planInvoiceLink(ctx context.Context, p billing.Plan) (string, error) {
	key := p.Code + ":" + strconv.Itoa(p.PriceStars)
	if v, ok := invoiceLinks.Load(key); ok {
		return v.(string), nil
	}
	link, err := h.tg.CreateInvoiceLink(ctx, bot.Invoice{
		Title:              "JUZ40 " + p.Title,
		Description:        fmt.Sprintf("%d %s теста в день · подписка на 30 дней с автопродлением", p.DailyLimit, passWord(p.DailyLimit)),
		Payload:            billing.SubscriptionPayload(p.Code),
		Prices:             []bot.LabeledPrice{{Label: "Подписка " + p.Title, Amount: p.PriceStars}},
		SubscriptionPeriod: billing.SubscriptionPeriodSeconds,
	})
	if err != nil {
		return "", err
	}
	invoiceLinks.Store(key, link)
	return link, nil
}

// passWord returns the Russian plural of «прохождение».
func passWord(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 14:
		return "прохождений"
	}
	switch n % 10 {
	case 1:
		return "прохождение"
	case 2, 3, 4:
		return "прохождения"
	default:
		return "прохождений"
	}
}

// quotaLoc is the quota calendar location for texts (reset time).
func (h *Handler) quotaReset(u *repositories.Usage) string {
	left := u.ResetAt.Sub(h.billing.Now())
	return fmt.Sprintf("через %s (в 00:00 по %s)", billing.HumanDuration(left), tzName(u.ResetAt))
}

// tzName renders the quota time zone for users.
func tzName(t time.Time) string {
	if loc := t.Location().String(); loc == "Asia/Almaty" {
		return "Алматы"
	} else if loc != "" && loc != "Local" {
		return loc
	}
	return "времени Казахстана"
}

// renderPlans renders the «⭐ Подписка / Тарифы» screen.
func (h *Handler) renderPlans(ctx context.Context, user *models.User) (string, *bot.InlineKeyboardMarkup, error) {
	u, err := h.billing.Usage(ctx, user.ID)
	if err != nil {
		return "", nil, err
	}
	cat := h.billing.Catalog()
	var b strings.Builder
	b.WriteString("⭐ Подписка и тарифы\n\n")
	fmt.Fprintf(&b, "Твой план: %s\n", u.Plan.Title)
	if !u.Plan.IsFree() {
		fmt.Fprintf(&b, "Действует до: %s\n", u.ExpiresAt.In(u.ResetAt.Location()).Format("02.01.2006 15:04"))
		if u.Stored != nil && u.Stored.Source == billing.SourceStars {
			b.WriteString("Продлевается автоматически (отменить: Telegram → Настройки → Мои звёзды → Подписки)\n")
		}
	}
	fmt.Fprintf(&b, "Осталось прохождений сегодня: %d/%d\n", u.Left, u.Limit)
	if u.OpenToday > 0 && u.CanStart < u.Left {
		fmt.Fprintf(&b, "Начато и не закончено сегодня: %d (место за ними уже занято)\n", u.OpenToday)
	}
	fmt.Fprintf(&b, "Лимит обновится %s\n\n", h.quotaReset(u))

	b.WriteString("Тарифы (оплата Telegram Stars ⭐, раз в 30 дней):\n")
	rows := [][]bot.InlineKeyboardButton{}
	for _, p := range cat.Plans() {
		mark := ""
		if p.Code == u.Plan.Code {
			mark = " ✅ твой план"
		}
		if p.IsFree() {
			fmt.Fprintf(&b, "• %s — бесплатно, %d %s в день%s\n", p.Title, p.DailyLimit, passWord(p.DailyLimit), mark)
			continue
		}
		fmt.Fprintf(&b, "• %s — %d⭐/мес, %d %s в день%s\n", p.Title, p.PriceStars, p.DailyLimit, passWord(p.DailyLimit), mark)
		if _, v := cat.CanBuy(u.Plan, p.Code); v == billing.BuyAllowed {
			verb := "⭐ Купить"
			if !u.Plan.IsFree() {
				verb = "⬆️ Перейти на"
			}
			rows = append(rows, bot.Row(bot.Btn(fmt.Sprintf("%s %s — %d⭐/мес", verb, p.Title, p.PriceStars), cbBuyPlan+p.Code)))
		}
	}
	b.WriteString("\n«Прохождение» — тест, решённый до конца (все вопросы) с итоговым результатом. Начатый и не законченный тест ничего не списывает; начатый до исчерпания лимита тест можно спокойно доделать.")
	if price := h.billing.WeakTestPrice(); price > 0 {
		fmt.Fprintf(&b, "\n\n🎯 Тест по слабым темам оплачивается отдельно — %d⭐ за тест — и дневной лимит не тратит.", price)
	}
	if !u.Plan.IsFree() {
		b.WriteString("\n\nПереход на более дорогой план — сразу (старая подписка отменяется). Перейти на более дешёвый можно после окончания текущего: отмени автопродление в Telegram.")
	}
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	return b.String(), &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// showPlans is used from the reply keyboard / command (new message).
func (h *Handler) showPlans(ctx context.Context, chatID int64, user *models.User) {
	if h.billing == nil {
		h.sendMainMenu(ctx, chatID, user, false)
		return
	}
	text, kb, err := h.renderPlans(ctx, user)
	if err != nil {
		logf("plans screen %d: %v", user.ID, err)
		h.sendText(ctx, chatID, "Ошибка загрузки 😔")
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		logf("send plans: %v", err)
	}
}

// editPlans is used from an inline button.
func (h *Handler) editPlans(ctx context.Context, cb *bot.CallbackQuery, user *models.User) {
	if h.billing == nil {
		h.answerCallback(ctx, cb, "")
		return
	}
	text, kb, err := h.renderPlans(ctx, user)
	if err != nil {
		logf("plans screen %d: %v", user.ID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	h.answerCallback(ctx, cb, "")
	h.editMessage(ctx, cb, text, kb)
}

// buyPlan answers a «Купить план» tap with the invoice link of the plan.
func (h *Handler) buyPlan(ctx context.Context, cb *bot.CallbackQuery, user *models.User, code string) {
	if h.billing == nil {
		h.answerCallback(ctx, cb, "")
		return
	}
	p, verdict, u, err := h.billing.CanBuy(ctx, user.ID, code)
	if err != nil {
		logf("buy plan %s user %d: %v", code, user.ID, err)
		h.answerCallback(ctx, cb, "Ошибка, попробуй позже")
		return
	}
	switch verdict {
	case billing.BuyAlreadyActive:
		h.answerAlert(ctx, cb, fmt.Sprintf("План %s уже активен до %s — он продлится автоматически.", p.Title, u.ExpiresAt.In(u.ResetAt.Location()).Format("02.01.2006")))
		return
	case billing.BuyDowngrade:
		h.answerAlert(ctx, cb, fmt.Sprintf("Сейчас активен %s до %s. Перейти на %s можно после его окончания: отмени автопродление в Telegram (Настройки → Мои звёзды → Подписки).", u.Plan.Title, u.ExpiresAt.In(u.ResetAt.Location()).Format("02.01.2006"), p.Title))
		return
	case billing.BuyUnknownPlan:
		h.answerCallback(ctx, cb, "Такого плана нет")
		return
	}
	if h.canBypassPay(cb.From.ID) {
		// Administrator: the purchase goes through at once, no invoice.
		h.answerCallback(ctx, cb, "")
		h.bypassPurchase(ctx, cb.Message.Chat.ID, user, cb.From.ID, billing.SubscriptionPayload(p.Code))
		return
	}
	link, err := h.planInvoiceLink(ctx, p)
	if err != nil {
		logf("invoice link %s: %v", p.Code, err)
		h.answerCallback(ctx, cb, "Не удалось создать счёт, попробуй позже")
		return
	}
	h.answerCallback(ctx, cb, "")
	text := fmt.Sprintf("⭐ План %s — %d⭐ в месяц, %d %s теста в день.\n\nНажми кнопку ниже — откроется оплата Telegram Stars. Подписка продлевается автоматически каждые 30 дней, отменить можно в любой момент в настройках Telegram.",
		p.Title, p.PriceStars, p.DailyLimit, passWord(p.DailyLimit))
	if !u.Plan.IsFree() {
		text += fmt.Sprintf("\n\nПлан %s сменится на %s сразу после оплаты, автопродление %s будет отменено.", u.Plan.Title, p.Title, u.Plan.Title)
	}
	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.URLBtn(fmt.Sprintf("⭐ Оплатить %d⭐", p.PriceStars), link)),
		bot.Row(bot.Btn("⬅️ К тарифам", cbPlans)),
	}}
	if _, err := h.tg.SendMessage(ctx, cb.Message.Chat.ID, text, kb); err != nil {
		logf("send plan invoice: %v", err)
	}
}

// --- Daily quota in the test flow -----------------------------------------------

// quotaToast is the «Осталось сегодня» notice shown when a chain test is
// opened ("" when subscriptions are off or the lookup failed).
func (h *Handler) quotaToast(ctx context.Context, user *models.User, test *models.Test) string {
	if h.billing == nil || test.Kind == models.TestKindPersonal {
		return ""
	}
	u, err := h.billing.Usage(ctx, user.ID)
	if err != nil {
		logf("usage %d: %v", user.ID, err)
		return ""
	}
	return fmt.Sprintf("Осталось прохождений сегодня: %d/%d (спишется, когда закончишь тест)", u.Left, u.Limit)
}

// quotaLine is the one-line quota summary of the subject screen.
func (h *Handler) quotaLine(ctx context.Context, user *models.User) string {
	if h.billing == nil {
		return ""
	}
	u, err := h.billing.Usage(ctx, user.ID)
	if err != nil {
		logf("usage %d: %v", user.ID, err)
		return ""
	}
	return fmt.Sprintf("🎟 Осталось прохождений сегодня: %d/%d (план %s)", u.Left, u.Limit, u.Plan.Title)
}

// chargedLine is appended to the result screen after a charged completion.
func (h *Handler) chargedLine(ctx context.Context, user *models.User) string {
	metrics.Inc(metrics.QuotaCharges)
	if h.billing == nil {
		return ""
	}
	u, err := h.billing.Usage(ctx, user.ID)
	if err != nil {
		logf("usage %d: %v", user.ID, err)
		return ""
	}
	return fmt.Sprintf("🎟 Тест пройден — списано 1 прохождение. Осталось сегодня: %d/%d", u.Left, u.Limit)
}

// sendQuotaExceeded explains that today's completions are used up, when
// they come back, and offers a better plan.
func (h *Handler) sendQuotaExceeded(ctx context.Context, chatID int64, user *models.User) {
	metrics.Inc(metrics.QuotaRefusals)
	text := "⛔ На сегодня прохождения закончились."
	rows := [][]bot.InlineKeyboardButton{}
	if h.billing != nil {
		if u, err := h.billing.Usage(ctx, user.ID); err == nil {
			text = fmt.Sprintf("⛔ На сегодня прохождения закончились: план %s — %d %s в день.\n\nНовые появятся %s.",
				u.Plan.Title, u.Limit, passWord(u.Limit), h.quotaReset(u))
			if u.OpenToday > 0 {
				text += fmt.Sprintf("\n\nУ тебя есть начатые сегодня тесты (%d) — их можно доделать: открой их в «📚 Предметы» (отмечены ⏸).", u.OpenToday)
			}
			if _, v := h.billing.Catalog().CanBuy(u.Plan, topPlan(h.billing.Catalog()).Code); v == billing.BuyAllowed {
				text += "\n\nХочешь заниматься больше? Улучши план 👇"
				rows = append(rows, bot.Row(bot.Btn("⭐ Улучшить план", cbPlans)))
			}
		} else {
			logf("usage %d: %v", user.ID, err)
		}
	}
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	if _, err := h.tg.SendMessage(ctx, chatID, text, &bot.InlineKeyboardMarkup{InlineKeyboard: rows}); err != nil {
		logf("send quota exceeded: %v", err)
	}
}

func topPlan(c *billing.Catalog) billing.Plan {
	ps := c.Plans()
	return ps[len(ps)-1]
}

// --- Paid weak-topics tests -------------------------------------------------------

// openWeakSubjectPaid is the weak-topics flow when tests are sold for
// Stars: an existing (still useful) personal test opens for free; a new
// one is generated only after a successful payment.
func (h *Handler) openWeakSubjectPaid(ctx context.Context, cb *bot.CallbackQuery, user *models.User, subjectID int64) {
	test, stale, topics, err := h.quiz.PersonalTestState(ctx, user.ID, subjectID)
	if err != nil {
		logf("personal test state %d/%d: %v", user.ID, subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки теста")
		return
	}
	if test != nil && (!stale || len(topics) == 0) {
		h.openTest(ctx, cb, user, test.ID)
		return
	}
	if len(topics) == 0 {
		h.answerAlert(ctx, cb, "В этом предмете у тебя больше нет слабых тем 🎉 Пройди обычные тесты в «📚 Предметы» — новые слабые темы появятся здесь.")
		h.editWeakMenu(ctx, cb, user)
		return
	}
	if o, err := h.billing.PaidWeakOrder(ctx, user.ID, subjectID); err != nil {
		logf("paid order %d/%d: %v", user.ID, subjectID, err)
	} else if o != nil {
		h.billing.Wake()
		h.answerAlert(ctx, cb, "Оплата получена, тест по слабым темам собирается ⏳ Я пришлю кнопку, как только он будет готов.")
		return
	}
	// A generation started before the switch to paid tests (or by an old
	// version) is still running: wait for it the old way, no new payment.
	if jobID, jerr := h.quiz.ActivePersonalJobID(ctx, user.ID, subjectID); jerr == nil && jobID > 0 {
		h.openWeakSubjectFree(ctx, cb, user, subjectID)
		return
	}
	h.answerCallback(ctx, cb, "")
	h.sendWeakInvoice(ctx, cb.Message.Chat.ID, user, cb.From.ID, subjectID, topics, test)
}

// buyWeakTest handles «Купить новый тест» (a stale personal test exists).
func (h *Handler) buyWeakTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, subjectID int64) {
	if !h.paidWeakTests() {
		h.openWeakSubject(ctx, cb, user, subjectID)
		return
	}
	test, _, topics, err := h.quiz.PersonalTestState(ctx, user.ID, subjectID)
	if err != nil {
		logf("personal test state %d/%d: %v", user.ID, subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	if len(topics) == 0 {
		h.answerAlert(ctx, cb, "В этом предмете у тебя больше нет слабых тем 🎉")
		return
	}
	h.answerCallback(ctx, cb, "")
	h.sendWeakInvoice(ctx, cb.Message.Chat.ID, user, cb.From.ID, subjectID, topics, test)
}

// sendWeakInvoice sends the Stars invoice of a new weak-topics test.
func (h *Handler) sendWeakInvoice(ctx context.Context, chatID int64, user *models.User, tgUserID, subjectID int64, topics []string, current *models.Test) {
	order, err := h.billing.OpenWeakOrder(ctx, user.ID, tgUserID, subjectID)
	if err != nil || order == nil {
		logf("open weak order %d/%d: %v", user.ID, subjectID, err)
		h.sendText(ctx, chatID, "Не удалось создать счёт 😔 Попробуй через минуту.")
		return
	}
	if h.canBypassPay(tgUserID) {
		// Administrator: the order is paid at once, no invoice.
		h.bypassPurchase(ctx, chatID, user, tgUserID, billing.WeakTestPayload(order.ID))
		return
	}
	subjName := ""
	if s, err := h.quiz.GetSubject(ctx, subjectID); err == nil {
		subjName = s.Name
	}
	desc := fmt.Sprintf("%s: 20 новых вопросов по твоим слабым темам — %s. Дневной лимит не тратит.", subjName, strings.Join(topics, ", "))
	if current != nil {
		desc = "Текущий тест будет заменён новым. " + desc
	}
	rows := [][]bot.InlineKeyboardButton{
		bot.Row(bot.InlineKeyboardButton{Text: fmt.Sprintf("⭐ Оплатить %d⭐", order.Amount), Pay: true}),
	}
	if current != nil {
		rows = append(rows, bot.Row(bot.Btn("Открыть текущий тест", cbOpenTest+strconv.FormatInt(current.ID, 10))))
	}
	_, err = h.tg.SendInvoice(ctx, chatID, bot.Invoice{
		Title:       "Тест по слабым темам",
		Description: desc,
		Payload:     billing.WeakTestPayload(order.ID),
		Prices:      []bot.LabeledPrice{{Label: "Тест по слабым темам", Amount: order.Amount}},
	}, &bot.InlineKeyboardMarkup{InlineKeyboard: rows})
	if err != nil {
		logf("send weak invoice %d: %v", order.ID, err)
		h.sendText(ctx, chatID, "Не удалось создать счёт 😔 Попробуй через минуту.")
	}
}

// --- Payment updates ------------------------------------------------------------------

// handlePreCheckout answers Telegram's last confirmation before charging
// (within 10 seconds). Never throttled.
func (h *Handler) handlePreCheckout(ctx context.Context, q *bot.PreCheckoutQuery) {
	ok, msg := false, "Оплата сейчас недоступна."
	if h.billing != nil {
		user, err := h.ensureUser(ctx, q.From)
		if err != nil {
			logf("pre-checkout upsert %d: %v", q.From.ID, err)
			msg = "Не удалось проверить оплату. Попробуй через минуту."
		} else {
			ok, msg = h.billing.PreCheckout(ctx, user.ID, q.Currency, q.TotalAmount, q.InvoicePayload)
		}
	}
	if !ok {
		logf("pre-checkout %s of tg %d refused (%s): %s", q.InvoicePayload, q.From.ID, q.Currency, msg)
	}
	if err := h.tg.AnswerPreCheckoutQuery(ctx, q.ID, ok, msg); err != nil {
		logf("answer pre-checkout: %v", err)
	}
}

// handleSuccessfulPayment applies a payment (idempotent by charge id) and
// tells the user what they got.
//
// An error (database unavailable) is returned so the durable update queue
// retries the update; the charge id keeps the retry idempotent.
func (h *Handler) handleSuccessfulPayment(ctx context.Context, m *bot.Message) error {
	sp := m.SuccessfulPayment
	if h.billing == nil {
		logf("successful payment %s of tg %d while subscriptions are OFF — not applied", sp.TelegramPaymentChargeID, m.From.ID)
		return nil
	}
	user, err := h.ensureUser(ctx, m.From)
	if err != nil {
		return fmt.Errorf("payment %s: upsert user: %w", sp.TelegramPaymentChargeID, err)
	}
	pi := services.PaymentInfo{
		Currency: sp.Currency, Amount: sp.TotalAmount, Payload: sp.InvoicePayload,
		ChargeID: sp.TelegramPaymentChargeID, ProviderChargeID: sp.ProviderPaymentChargeID,
		IsRecurring: sp.IsRecurring, IsFirstRecurring: sp.IsFirstRecurring,
	}
	if sp.SubscriptionExpirationDate > 0 {
		pi.SubExpiresAt = time.Unix(sp.SubscriptionExpirationDate, 0)
	}
	out, err := h.billing.OnSuccessfulPayment(ctx, user.ID, m.From.ID, pi)
	if err != nil {
		logf("apply payment %s: %v", sp.TelegramPaymentChargeID, err)
		return fmt.Errorf("apply payment %s: %w", sp.TelegramPaymentChargeID, err)
	}
	if out.Duplicate {
		return nil
	}
	h.sendPaymentOutcome(ctx, m.Chat.ID, user, out)
	return nil
}

// sendPaymentOutcome tells the user what a (real or admin-bypass) payment
// gave them.
func (h *Handler) sendPaymentOutcome(ctx context.Context, chatID int64, user *models.User, out *services.PaymentOutcome) {
	switch out.Kind {
	case services.OutcomeSubscription:
		until := ""
		if !out.ExpiresAt.IsZero() {
			until = " до " + out.ExpiresAt.In(h.billing.Now().Location()).Format("02.01.2006")
			if u, err := h.billing.Usage(ctx, user.ID); err == nil {
				until = " до " + u.ExpiresAt.In(u.ResetAt.Location()).Format("02.01.2006")
			}
		}
		text := fmt.Sprintf("✅ Оплата получена! План %s активен%s — %d %s в день.", out.Plan.Title, until, out.Plan.DailyLimit, passWord(out.Plan.DailyLimit))
		switch out.Decision {
		case billing.DecisionRenewal:
			text = fmt.Sprintf("🔁 Подписка %s продлена%s. Спасибо!", out.Plan.Title, until)
		case billing.DecisionUpgrade:
			text = fmt.Sprintf("⬆️ План улучшен до %s%s — %d %s в день. Автопродление прежней подписки отменено.", out.Plan.Title, until, out.Plan.DailyLimit, passWord(out.Plan.DailyLimit))
		}
		h.sendWithKeyboard(ctx, chatID, text, bot.Row(bot.Btn("📚 К предметам", cbSubjects)), bot.Row(bot.Btn("⭐ Мой план", cbPlans)))
	case services.OutcomeWeakReady:
		h.sendWithKeyboard(ctx, chatID, "✅ Оплата получена! Тест по слабым темам готов 🎯",
			bot.Row(bot.Btn("▶️ Начать тест", cbOpenTest+strconv.FormatInt(out.Test.ID, 10))))
	case services.OutcomeWeakPending:
		h.sendText(ctx, chatID, "✅ Оплата получена! Собираю тест по твоим слабым темам ⏳\n\nОбычно это пара минут — я пришлю кнопку, как только он будет готов. Если собрать тест не получится, звёзды вернутся автоматически.")
	case services.OutcomeWeakRefunded:
		h.sendText(ctx, chatID, "😔 Не получилось собрать тест по слабым темам — звёзды вернутся на твой баланс в ближайшие минуты.")
	case services.OutcomeRejected:
		if out.Decision == billing.DecisionDuplicate {
			h.sendText(ctx, chatID, fmt.Sprintf("План %s у тебя уже активен — повторный платёж вернётся автоматически, а лишняя подписка будет отменена.", out.Plan.Title))
			return
		}
		h.sendText(ctx, chatID, "Этот платёж не может быть применён (план уже сменён или счёт устарел) — звёзды вернутся автоматически.")
	}
}

func (h *Handler) sendWithKeyboard(ctx context.Context, chatID int64, text string, rows ...[]bot.InlineKeyboardButton) {
	if _, err := h.tg.SendMessage(ctx, chatID, text, &bot.InlineKeyboardMarkup{InlineKeyboard: rows}); err != nil {
		logf("send: %v", err)
	}
}

// WeakTestReady implements services.BillingNotifier (the reconciler
// built a paid test asynchronously).
func (h *Handler) WeakTestReady(ctx context.Context, tgUserID int64, test *models.Test) {
	h.sendWithKeyboard(ctx, tgUserID, "✅ Твой тест по слабым темам готов 🎯",
		bot.Row(bot.Btn("▶️ Начать тест", cbOpenTest+strconv.FormatInt(test.ID, 10))))
}

// PaymentRefunded implements services.BillingNotifier.
func (h *Handler) PaymentRefunded(ctx context.Context, tgUserID int64, p repositories.Payment) {
	text := fmt.Sprintf("↩️ Возвращено %d⭐ на твой баланс Telegram Stars.", p.Amount)
	if p.Kind == billing.KindWeakTest {
		text = fmt.Sprintf("↩️ Тест по слабым темам собрать не удалось — %d⭐ возвращены на твой баланс. Попробуй позже ещё раз 🙏", p.Amount)
	}
	h.sendText(ctx, tgUserID, text)
}

// --- Admin commands ----------------------------------------------------------------------

// handleAdmin runs an administrator command; false = not an admin command
// (or not an admin — the text is then handled as usual).
func (h *Handler) handleAdmin(ctx context.Context, m *bot.Message, text string) bool {
	if h.isAdmin == nil || h.billing == nil || !h.isAdmin(m.From.ID) {
		return false
	}
	f := strings.Fields(text)
	if len(f) == 0 {
		return false
	}
	cmd := commandKey(f[0])
	reply := func(s string) { h.sendText(ctx, m.Chat.ID, s) }
	switch cmd {
	case "/grant":
		if len(f) < 3 {
			reply("Формат: /grant <telegram_id> <free|plus|pro|premium> [дней=30]")
			return true
		}
		tgID, err := strconv.ParseInt(f[1], 10, 64)
		days := 30
		if len(f) >= 4 {
			if d, derr := strconv.Atoi(f[3]); derr == nil && d > 0 && d <= 3660 {
				days = d
			}
		}
		if err != nil {
			reply("Неверный telegram_id")
			return true
		}
		uid, until, err := h.billing.Grant(ctx, tgID, f[2], days)
		if err == nil && h.admin != nil {
			h.admin.LogAction(ctx, m.From.ID, services.ActionCommandGrant, uid, fmt.Sprintf("plan=%s days=%d", f[2], days))
		}
		switch {
		case errors.Is(err, services.ErrUnknownPlan):
			reply("Неизвестный план: " + f[2])
		case errors.Is(err, repositories.ErrNotFound):
			reply("Пользователь не найден (он должен хотя бы раз написать боту)")
		case err != nil:
			reply("Ошибка: " + err.Error())
		case strings.EqualFold(f[2], billing.PlanFree):
			reply(fmt.Sprintf("✅ Пользователь %d переведён на Free", tgID))
		default:
			reply(fmt.Sprintf("✅ Пользователю %d выдан план %s до %s", tgID, f[2], until.Format("02.01.2006 15:04")))
		}
	case "/revoke":
		if len(f) < 2 {
			reply("Формат: /revoke <telegram_id>")
			return true
		}
		tgID, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			reply("Неверный telegram_id")
			return true
		}
		uid, _, err := h.billing.Grant(ctx, tgID, billing.PlanFree, 0)
		if err != nil {
			reply("Ошибка: " + err.Error())
			return true
		}
		if h.admin != nil {
			h.admin.LogAction(ctx, m.From.ID, services.ActionCommandRevoke, uid, "plan=free")
		}
		reply(fmt.Sprintf("✅ План пользователя %d отозван (Free)", tgID))
	case "/subinfo":
		if len(f) < 2 {
			reply("Формат: /subinfo <telegram_id>")
			return true
		}
		tgID, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			reply("Неверный telegram_id")
			return true
		}
		u, ps, err := h.billing.AdminInfo(ctx, tgID)
		if err != nil {
			reply("Ошибка: " + err.Error())
			return true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Пользователь %d\nПлан: %s", tgID, u.Plan.Title)
		if !u.ExpiresAt.IsZero() {
			fmt.Fprintf(&b, " до %s", u.ExpiresAt.Format("02.01.2006 15:04 MST"))
		}
		if u.Stored != nil {
			fmt.Fprintf(&b, " (источник: %s)", u.Stored.Source)
		}
		fmt.Fprintf(&b, "\nСегодня (%s): списано %d из %d, открыто %d\n\nПлатежи:", u.Day, u.Used, u.Limit, u.OpenToday)
		if len(ps) == 0 {
			b.WriteString(" нет")
		}
		for _, p := range ps {
			fmt.Fprintf(&b, "\n• %s %s %s %d⭐ %s/%s\n  %s", p.CreatedAt.Format("02.01 15:04"), p.Kind, p.Plan, p.Amount, p.Decision, p.Status, p.ChargeID)
		}
		reply(b.String())
	case "/refund":
		if len(f) < 2 {
			reply("Формат: /refund <telegram_payment_charge_id>")
			return true
		}
		ok, err := h.billing.AdminRefund(ctx, f[1])
		if ok && h.admin != nil {
			h.admin.LogAction(ctx, m.From.ID, services.ActionCommandRefund, 0, "charge="+f[1])
		}
		switch {
		case err != nil:
			reply("Ошибка: " + err.Error())
		case !ok:
			reply("Платёж не найден или уже возвращается/возвращён")
		default:
			reply("✅ Возврат поставлен в очередь — он будет выполнен в ближайшую минуту")
		}
	default:
		return false
	}
	return true
}
