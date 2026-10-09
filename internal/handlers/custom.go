package handlers

// «✨ Свой тест»: main menu → subject picker → subject screen (the user's
// unfinished custom tests + «➕ Новый тест — 15⭐») → description (≤ 500
// characters) → AI pre-check → invoice (admin: free, test-mode purchase) →
// generation → «▶️ Начать тест». After a test, an exit or «🏁 Закончить
// тест» the user lands back on the custom-test screen of the subject.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
)

// Custom-test callbacks.
const (
	cbCustomMenu    = "cust:menu"   // subject picker
	cbCustomSubject = "cust:subj:"  // + subjectID (the subject's custom tests)
	cbCustomNew     = "cust:new:"   // + subjectID (start a description)
	cbCustomFinish  = "cust:fin:"   // + testID (finish & delete a mastered custom test)
	cbCustomCancel  = "cust:cncl:"  // + subjectID (cancel the description step)
	cbCustomFree    = "cust:free:"  // + orderID (confirm a free order)
	kbCustom        = "✨ Свой тест" // reply-keyboard button
	customExitNote  = "🚪 Вы вышли из теста. Попытка сохранена — нажми на тест с ⏸, чтобы продолжить с места остановки."
	customOffText   = "✨ Свой тест сейчас недоступен 😔 Загляни чуть позже."
	customDraftHint = "Например: «Квадратные уравнения и теорема Виета», «Электролиз и законы Фарадея, задачи», «Причастный оборот — запятые»."
)

// WithCustomTests turns «✨ Свой тест» on (nil = off).
func (h *Handler) WithCustomTests(c *services.CustomTestService) *Handler {
	h.custom = c
	return h
}

func (h *Handler) customOn() bool { return h.custom != nil && h.custom.Enabled() }

// customPriceLabel is the price of a new test for this user.
func (h *Handler) customPriceLabel(tgUserID int64) string {
	switch h.custom.PayMode(tgUserID) {
	case services.CustomPayFree:
		return "бесплатно"
	case services.CustomPayAdmin:
		return fmt.Sprintf("%d⭐ (для админа бесплатно)", h.custom.Price())
	}
	return fmt.Sprintf("%d⭐", h.custom.Price())
}

// --- Subject picker ------------------------------------------------------------------

func (h *Handler) renderCustomMenu(ctx context.Context, user *models.User) (string, *bot.InlineKeyboardMarkup, error) {
	subjects, err := h.quiz.ListSubjects(ctx)
	if err != nil {
		return "", nil, err
	}
	counts, err := h.custom.Counts(ctx, user.ID)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	b.WriteString("✨ Свой тест\n\nОпиши своими словами, какой тест тебе нужен (до 500 символов), — я сгенерирую 20 вопросов в формате ЕНТ именно по этой теме.\n\n")
	fmt.Fprintf(&b, "• Цена: %s за тест, дневной лимит прохождений не тратит.\n", h.customPriceLabel(user.TelegramID))
	fmt.Fprintf(&b, "• До %d незаконченных тестов в каждом предмете.\n", h.custom.MaxPerSubject())
	fmt.Fprintf(&b, "• Доведи тест до %d🟢 + %d🟡 — и его можно закончить.\n\nВыбери предмет:", models.UnlockGreen, models.UnlockYellow)
	buttons := make([]bot.InlineKeyboardButton, 0, len(subjects))
	for _, s := range subjects {
		label := subjectEmoji(s.Name) + " " + s.Name
		if n := counts[s.ID]; n > 0 {
			label += fmt.Sprintf(" (%d)", n)
		}
		buttons = append(buttons, bot.Btn(label, cbCustomSubject+strconv.FormatInt(s.ID, 10)))
	}
	rows := bot.ChunkButtons(buttons, 1)
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	if len(subjects) == 0 {
		return "✨ Свой тест\n\nПредметов пока нет — скоро они появятся.", &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
	}
	return b.String(), &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// showCustomMenu is used from the reply keyboard (new message).
func (h *Handler) showCustomMenu(ctx context.Context, chatID int64, user *models.User) {
	if !h.customOn() {
		h.sendText(ctx, chatID, customOffText)
		return
	}
	text, kb, err := h.renderCustomMenu(ctx, user)
	if err != nil {
		logf("custom menu: %v", err)
		h.sendText(ctx, chatID, "Ошибка загрузки 😔")
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		logf("send custom menu: %v", err)
	}
}

// editCustomMenu is used from an inline button.
func (h *Handler) editCustomMenu(ctx context.Context, cb *bot.CallbackQuery, user *models.User) {
	if !h.customOn() {
		h.answerAlert(ctx, cb, customOffText)
		return
	}
	text, kb, err := h.renderCustomMenu(ctx, user)
	if err != nil {
		logf("custom menu: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	h.answerCallback(ctx, cb, "")
	h.editMessage(ctx, cb, text, kb)
}

// --- Subject screen ----------------------------------------------------------------------

// renderCustomSubject renders the user's custom tests of a subject.
func (h *Handler) renderCustomSubject(ctx context.Context, user *models.User, subjectID int64) (string, *bot.InlineKeyboardMarkup, error) {
	if h.custom == nil {
		return "", nil, errNoTestList
	}
	st, err := h.custom.SubjectState(ctx, user.ID, subjectID)
	if err != nil {
		return "", nil, err
	}
	ids := make([]int64, len(st.Tests))
	for i, t := range st.Tests {
		ids[i] = t.ID
	}
	active := map[int64]bool{}
	if len(ids) > 0 {
		if active, err = h.quiz.ActiveAttempts(ctx, user.ID, ids); err != nil {
			logf("custom active attempts %d: %v", user.ID, err)
			active = map[int64]bool{}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✨ Свой тест · %s %s\n\n", subjectEmoji(st.Subject.Name), st.Subject.Name)
	var rows [][]bot.InlineKeyboardButton
	if len(st.Tests) == 0 {
		b.WriteString("Своих тестов по этому предмету пока нет.\n")
	} else {
		fmt.Fprintf(&b, "Твои тесты (%d из %d):\n", st.Used, h.custom.MaxPerSubject())
		for _, t := range st.Tests {
			mark := "▶️ "
			if active[t.ID] {
				mark = "⏸ "
			}
			rows = append(rows, bot.Row(bot.Btn(mark+t.Title, cbOpenTest+strconv.FormatInt(t.ID, 10))))
		}
	}
	if o := st.Inflight; o != nil {
		if o.SubjectID == subjectID {
			fmt.Fprintf(&b, "\n⏳ Генерирую тест «%s» — пришлю кнопку, как только он будет готов.\n", o.Title)
		} else {
			b.WriteString("\n⏳ Сейчас генерируется твой тест по другому предмету — новый можно заказать, когда он будет готов.\n")
		}
	}
	fmt.Fprintf(&b, "\nДоведи тест до %d🟢 + %d🟡 — тогда его можно закончить, и освободится место для нового.", models.UnlockGreen, models.UnlockYellow)
	if h.customOn() {
		rows = append(rows, bot.Row(bot.Btn("➕ Новый тест — "+h.customPriceLabel(user.TelegramID), cbCustomNew+strconv.FormatInt(subjectID, 10))))
	}
	rows = append(rows,
		bot.Row(bot.Btn("⬅️ К предметам", cbCustomMenu)),
		bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	return b.String(), &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// openCustomSubject shows the custom-test screen of a subject.
func (h *Handler) openCustomSubject(ctx context.Context, cb *bot.CallbackQuery, user *models.User, subjectID int64) {
	if h.custom == nil {
		h.answerCallback(ctx, cb, "")
		return
	}
	// Leaving the description step by navigation cancels it.
	if err := h.custom.ClearDraft(ctx, user.ID); err != nil {
		logf("custom clear draft %d: %v", user.ID, err)
	}
	text, kb, err := h.renderCustomSubject(ctx, user, subjectID)
	if err != nil {
		logf("custom subject %d/%d: %v", user.ID, subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	h.answerCallback(ctx, cb, "")
	h.editMessage(ctx, cb, text, kb)
}

// newCustomTest: «➕ Новый тест» — asks for the description.
func (h *Handler) newCustomTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, subjectID int64) {
	if h.custom == nil {
		h.answerCallback(ctx, cb, "")
		return
	}
	err := h.custom.StartDraft(ctx, user.ID, subjectID)
	switch {
	case errors.Is(err, services.ErrCustomDisabled):
		h.answerAlert(ctx, cb, customOffText)
		return
	case errors.Is(err, services.ErrCustomInFlight):
		h.answerAlert(ctx, cb, "⏳ Твой тест ещё генерируется. Дождись его — потом можно заказать следующий.")
		return
	case errors.Is(err, services.ErrCustomCap):
		h.answerAlert(ctx, cb, fmt.Sprintf("В этом предмете уже %d твоих теста — это максимум. Закончи один из тестов (доведи до %d🟢 + %d🟡 и нажми «🏁 Закончить тест»), и место освободится.",
			h.custom.MaxPerSubject(), models.UnlockGreen, models.UnlockYellow))
		return
	case err != nil:
		logf("custom draft %d/%d: %v", user.ID, subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка, попробуй ещё раз")
		return
	}
	subj := ""
	if s, err := h.quiz.GetSubject(ctx, subjectID); err == nil {
		subj = subjectEmoji(s.Name) + " " + s.Name
	}
	h.answerCallback(ctx, cb, "")
	text := fmt.Sprintf("✍️ Новый тест · %s\n\nНапиши одним сообщением, какой тест тебе нужен — тема, тип заданий, уровень (до %d символов).\n\n%s\n\nЦена: %s. Сначала я проверю, что по запросу можно составить учебный тест, — если нет, ничего не спишется.",
		subj, services.CustomDescMaxRunes, customDraftHint, h.customPriceLabel(user.TelegramID))
	h.editMessage(ctx, cb, text, &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("❌ Отмена", cbCustomCancel+strconv.FormatInt(subjectID, 10))),
	}})
}

// cancelCustomDraft: «❌ Отмена» of the description step.
func (h *Handler) cancelCustomDraft(ctx context.Context, cb *bot.CallbackQuery, user *models.User, subjectID int64) {
	h.openCustomSubject(ctx, cb, user, subjectID)
}

// --- Description ------------------------------------------------------------------------

// handleCustomDescription handles a text message while the user is in the
// description step; false = no draft (the message is handled as usual).
func (h *Handler) handleCustomDescription(ctx context.Context, m *bot.Message, user *models.User, text string) bool {
	if h.custom == nil || text == "" {
		return false
	}
	subjectID, ok, err := h.custom.Draft(ctx, user.ID)
	if err != nil {
		logf("custom draft lookup %d: %v", user.ID, err)
		return false
	}
	if !ok {
		return false
	}
	chatID := m.Chat.ID
	back := bot.Row(bot.Btn("⬅️ Назад", cbCustomSubject+strconv.FormatInt(subjectID, 10)))
	noteID, err := h.tg.SendMessage(ctx, chatID, "🔎 Проверяю запрос…", nil)
	if err != nil {
		logf("send custom check note: %v", err)
		noteID = 0
	}
	reply := func(text string, rows ...[]bot.InlineKeyboardButton) {
		kb := &bot.InlineKeyboardMarkup{InlineKeyboard: rows}
		if noteID > 0 {
			h.editNote(ctx, chatID, noteID, text, kb)
			return
		}
		if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
			logf("send custom reply: %v", err)
		}
	}
	res, err := h.custom.SubmitDescription(ctx, user.ID, m.From.ID, subjectID, text)
	if errors.Is(err, services.ErrCustomDisabled) {
		reply(customOffText, back)
		return true
	}
	if err != nil {
		logf("custom submit %d/%d: %v", user.ID, subjectID, err)
		reply("Ошибка 😔 Попробуй отправить описание ещё раз через минуту.", back)
		return true
	}
	switch res.Kind {
	case services.CustomSubmitTooLong:
		reply(fmt.Sprintf("✂️ Слишком длинно: %d символов, а можно до %d. Сократи описание и отправь ещё раз.", res.Runes, services.CustomDescMaxRunes), back)
	case services.CustomSubmitTooShort:
		reply("Опиши тему чуть подробнее (хотя бы пару слов) и отправь ещё раз.\n\n"+customDraftHint, back)
	case services.CustomSubmitInFlight:
		reply("⏳ Твой предыдущий тест ещё генерируется — дождись его, потом закажи следующий.", back)
	case services.CustomSubmitCap:
		reply(fmt.Sprintf("В этом предмете уже %d твоих теста — закончи один из них, чтобы заказать новый.", h.custom.MaxPerSubject()), back)
	case services.CustomSubmitUnavailable:
		reply("😔 Не получилось проверить запрос — сервис генерации сейчас перегружен. Ничего не списано. Отправь описание ещё раз через пару минут.", back)
	case services.CustomSubmitRefused:
		reply("🙅 По такому запросу тест составить не получится: "+res.Reason+"\n\nНичего не списано. Напиши другое описание — я жду сообщение.", back)
	case services.CustomSubmitAccepted:
		h.offerCustomOrder(ctx, chatID, user, m.From.ID, res, reply)
	}
	return true
}

// offerCustomOrder sends the invoice of an accepted order (or starts a
// free / admin order at once).
func (h *Handler) offerCustomOrder(ctx context.Context, chatID int64, user *models.User, tgUserID int64, res *services.CustomSubmit, reply func(string, ...[]bot.InlineKeyboardButton)) {
	o := res.Order
	subj := res.Subject.Name
	back := bot.Row(bot.Btn("⬅️ Назад", cbCustomSubject+strconv.FormatInt(o.SubjectID, 10)))
	switch h.custom.PayMode(tgUserID) {
	case services.CustomPayFree:
		reply(fmt.Sprintf("✅ Запрос принят: «%s» (%s).\n\nНажми «Сгенерировать» — я соберу 20 вопросов.", o.Title, subj),
			bot.Row(bot.Btn("✨ Сгенерировать", cbCustomFree+strconv.FormatInt(o.ID, 10))), back)
		return
	case services.CustomPayAdmin:
		if h.canBypassPay(tgUserID) {
			reply(fmt.Sprintf("✅ Запрос принят: «%s» (%s).", o.Title, subj))
			h.bypassPurchase(ctx, chatID, user, tgUserID, billing.CustomTestPayload(o.ID))
			return
		}
	}
	reply(fmt.Sprintf("✅ Запрос принят: «%s» (%s).\n\nОплати %d⭐ ниже — и я начну генерацию. Если тест собрать не получится, звёзды вернутся автоматически.", o.Title, subj, o.Amount), back)
	desc := fmt.Sprintf("%s: 20 вопросов в формате ЕНТ по твоему запросу: «%s». Дневной лимит не тратит.", subj, o.Description)
	_, err := h.tg.SendInvoice(ctx, chatID, bot.Invoice{
		Title:       "Свой тест: " + o.Title,
		Description: desc,
		Payload:     billing.CustomTestPayload(o.ID),
		Prices:      []bot.LabeledPrice{{Label: "Свой тест", Amount: o.Amount}},
	}, &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.InlineKeyboardButton{Text: fmt.Sprintf("⭐ Оплатить %d⭐", o.Amount), Pay: true}),
	}})
	if err != nil {
		logf("send custom invoice %d: %v", o.ID, err)
		h.sendText(ctx, chatID, "Не удалось создать счёт 😔 Попробуй через минуту.")
	}
}

// confirmFreeCustom: «✨ Сгенерировать» of a free order.
func (h *Handler) confirmFreeCustom(ctx context.Context, cb *bot.CallbackQuery, user *models.User, orderID int64) {
	if h.custom == nil {
		h.answerCallback(ctx, cb, "")
		return
	}
	out, err := h.custom.ConfirmFree(ctx, user.ID, orderID)
	if err != nil {
		logf("custom free order %d: %v", orderID, err)
		h.answerCallback(ctx, cb, "Ошибка, попробуй ещё раз")
		return
	}
	h.answerCallback(ctx, cb, "")
	if out.Kind == services.OutcomeRejected {
		h.editMessage(ctx, cb, "Этот запрос уже неактуален (заказан другой тест или предмет заполнен). Открой «✨ Свой тест» заново.",
			&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{bot.Row(bot.Btn("✨ Свой тест", cbCustomMenu))}})
		return
	}
	h.sendPaymentOutcome(ctx, cb.Message.Chat.ID, user, out)
}

// customOutcome renders the custom-test payment outcomes (true = handled).
func (h *Handler) customOutcome(ctx context.Context, chatID int64, out *services.PaymentOutcome) bool {
	switch out.Kind {
	case services.OutcomeCustomPending:
		h.sendText(ctx, chatID, "✅ Принято! Генерирую твой тест ⏳\n\nОбычно это несколько минут — я пришлю кнопку, как только он будет готов. Если собрать тест не получится, звёзды вернутся автоматически.")
	case services.OutcomeCustomReady:
		h.sendWithKeyboard(ctx, chatID, "✅ Твой тест готов: «"+out.Test.Title+"»",
			bot.Row(bot.Btn("▶️ Начать тест", cbOpenTest+strconv.FormatInt(out.Test.ID, 10))))
	case services.OutcomeCustomRefunded:
		h.sendText(ctx, chatID, "😔 Не получилось сгенерировать тест — звёзды вернутся на твой баланс в ближайшие минуты.")
	default:
		return false
	}
	return true
}

// CustomTestReady implements services.CustomNotifier.
func (h *Handler) CustomTestReady(ctx context.Context, tgUserID int64, test *models.Test) {
	h.sendWithKeyboard(ctx, tgUserID, "✅ Твой тест готов: «"+test.Title+"»\n\n20 вопросов по твоему запросу. Доведи его до 15🟢 + 5🟡 — и тест можно будет закончить.",
		bot.Row(bot.Btn("▶️ Начать тест", cbOpenTest+strconv.FormatInt(test.ID, 10))),
		bot.Row(bot.Btn("✨ Мои тесты", cbCustomSubject+strconv.FormatInt(test.SubjectID, 10))))
}

// CustomTestFailed implements services.CustomNotifier (free / admin orders).
func (h *Handler) CustomTestFailed(ctx context.Context, tgUserID int64, o repositories.CustomOrder) {
	h.sendWithKeyboard(ctx, tgUserID, fmt.Sprintf("😔 Не получилось сгенерировать тест «%s». Попробуй заказать его ещё раз чуть позже.", o.Title),
		bot.Row(bot.Btn("✨ Свой тест", cbCustomSubject+strconv.FormatInt(o.SubjectID, 10))))
}

// --- Finish ------------------------------------------------------------------------------

// finishCustomTest handles «🏁 Закончить тест» of a custom test.
func (h *Handler) finishCustomTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, testID int64) {
	if h.custom == nil {
		h.answerCallback(ctx, cb, "")
		return
	}
	sid, err := h.custom.FinishTest(ctx, user.ID, testID)
	if err != nil {
		logf("finish custom test %d/%d: %v", user.ID, testID, err)
		h.answerCallback(ctx, cb, "Не удалось закончить тест")
		return
	}
	h.answerCallback(ctx, cb, "Тест завершён 🎉")
	text, kb := h.afterTestScreen(ctx, user, &models.Test{ID: testID, SubjectID: sid, Kind: models.TestKindCustom},
		"🏁 Тест завершён и удалён — место для нового теста освободилось.", nil)
	h.editMessage(ctx, cb, withHeader("", text), kb)
}
