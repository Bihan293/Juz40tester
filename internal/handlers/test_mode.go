package handlers

// «Test mode»: while the user is inside a test the main menu is hidden and
// nothing else can be opened (owner's request: the visible menu let a
// student jump into another subject's test in the middle of a test).
//
//   - Entering a test (open / resume / retry / an answer on a paused
//     attempt) sets users.active_attempt_id (migration 000031 — the state is
//     in the database, any instance may handle the next update) and replaces
//     the bottom reply keyboard with ONE button «🚪 Выйти из теста». The
//     keyboard is REPLACED, never removed: a ReplyKeyboardRemove makes
//     Telegram mobile clients open the system text keyboard (the old
//     «keyboard pops up by itself» bug).
//   - While a test is active every other update — reply-menu buttons,
//     commands (/start, /menu …), free text, inline buttons of older
//     messages (subjects, other tests, weak topics, «Свой тест», plans,
//     admin panel) — gets «Сначала заверши текущий тест или выйди из него»
//     with «▶️ Продолжить тест» / «🚪 Выйти из теста». Allowed: the answers,
//     the exit buttons of the ACTIVE attempt, reopening the SAME test, the
//     admin text commands (/grant, /revoke, /subinfo, /refund) and payments
//     (pre_checkout / successful_payment never reach the router).
//   - Finishing the test or «✅ Да, выйти» clears the pointer and brings
//     the main menu keyboard back.
//
// Exit never charges anything: the daily quota is charged only when an
// attempt is completed (SubmitAnswer), exactly as before — leaving only
// clears the pointer, the attempt stays in progress (resumable ⏸).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// kbExitTest is the only reply-keyboard button while a test is active.
const kbExitTest = "🚪 Выйти из теста"

const (
	testModeNotice = "📝 Идёт тест — главное меню скрыто до его окончания.\n" +
		"Отвечай кнопками под вопросом. Выйти можно кнопкой «🚪 Выйти из теста» внизу — прогресс сохранится."
	menuBackNotice = "🏠 Главное меню снова доступно 👇"
	activeGuardMsg = "⏳ Сначала заверши текущий тест или выйди из него."
)

// testModeKeyboard replaces the main menu while a test is active.
func testModeKeyboard() *bot.ReplyKeyboardMarkup {
	return &bot.ReplyKeyboardMarkup{
		ResizeKeyboard: true,
		Keyboard:       [][]bot.KeyboardButton{bot.ReplyRow(kbExitTest)},
	}
}

// exitConfirmKeyboard is the yes/no of «❓ Выйти из теста?».
func exitConfirmKeyboard(attemptID int64) *bot.InlineKeyboardMarkup {
	idStr := strconv.FormatInt(attemptID, 10)
	return &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(
			bot.Btn("✅ Да, выйти", cbExitYes+idStr),
			bot.Btn("↩️ Нет, продолжить", cbExitNo+idStr),
		),
	}}
}

const exitConfirmText = "❓ Выйти из теста?\n\nПрогресс сохранится — потом можно будет продолжить с того же вопроса."

// testModeOn reports whether the test-mode bookkeeping is wired (it needs
// the users repository; unit tests with a bare Handler skip it).
func (h *Handler) testModeOn() bool { return h.users != nil }

// enterTest makes attemptID the user's active test before its question is
// shown. ok = false: another test is active — the guard message has been
// sent and the caller must stop. notice = the pointer moved: the caller
// sends the test-mode keyboard (sendTestModeNotice) after the question.
// A database error never blocks the test (fail open, logged).
func (h *Handler) enterTest(ctx context.Context, chatID int64, user *models.User, attemptID int64) (ok, notice bool) {
	if !h.testModeOn() || user.ActiveAttemptID == attemptID {
		return true, false
	}
	changed, err := h.users.EnterAttempt(ctx, user.ID, attemptID)
	if errors.Is(err, repositories.ErrOtherTestActive) {
		h.sendActiveGuard(ctx, chatID, user)
		return false, false
	}
	if err != nil {
		logf("enter test mode %d/%d: %v", user.ID, attemptID, err)
		return true, false
	}
	if changed {
		user.ActiveAttemptID = attemptID
		if h.custom != nil {
			// A pending «send the description» step of «✨ Свой тест» ends.
			if err := h.custom.ClearDraft(ctx, user.ID); err != nil {
				logf("custom clear draft %d: %v", user.ID, err)
			}
		}
	}
	return true, changed
}

// sendTestModeNotice hides the main menu: the bottom keyboard becomes the
// single «🚪 Выйти из теста» button.
func (h *Handler) sendTestModeNotice(ctx context.Context, chatID int64) {
	if _, err := h.tg.SendMessage(ctx, chatID, testModeNotice, testModeKeyboard()); err != nil {
		logf("send test-mode keyboard: %v", err)
	}
}

// leaveTest clears the active test (finish / exit / closed attempt) and,
// when it was active, brings the main menu keyboard back.
func (h *Handler) leaveTest(ctx context.Context, chatID int64, user *models.User, attemptID int64) {
	if !h.testModeOn() {
		return
	}
	cleared, err := h.users.LeaveAttempt(ctx, user.ID, attemptID)
	if err != nil {
		logf("leave test mode %d/%d: %v", user.ID, attemptID, err)
		return
	}
	if !cleared {
		return
	}
	user.ActiveAttemptID = 0
	if _, err := h.tg.SendMessage(ctx, chatID, menuBackNotice, h.menuKeyboard(chatID)); err != nil {
		logf("send menu back: %v", err)
	}
}

// activeTest returns the test the user is inside, or nil (zero queries when
// the user row says no test is open).
func (h *Handler) activeTest(ctx context.Context, user *models.User) *repositories.ActiveTest {
	if !h.testModeOn() || user.ActiveAttemptID == 0 {
		return nil
	}
	at, err := h.users.ActiveTestOf(ctx, user.ID)
	if err != nil {
		logf("active test of %d: %v", user.ID, err)
		return nil
	}
	if at == nil {
		user.ActiveAttemptID = 0
	} else {
		user.ActiveAttemptID = at.AttemptID
	}
	return at
}

// activeTestLabel names the active test in the guard message.
func activeTestLabel(at *repositories.ActiveTest) string {
	var name string
	switch at.TestKind {
	case models.TestKindPersonal, "weak":
		name = "🎯 Слабые темы"
	case models.TestKindCustom:
		name = "✨ Свой тест"
	default:
		name = at.Title
		if name == "" {
			name = fmt.Sprintf("Тест %d", at.TestNumber)
		}
	}
	if at.SubjectName != "" {
		return name + " · " + at.SubjectName
	}
	return name
}

// activeGuardKeyboard: continue the active test or leave it (with the usual
// confirmation).
func activeGuardKeyboard(at *repositories.ActiveTest) *bot.InlineKeyboardMarkup {
	return &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("▶️ Продолжить тест", cbOpenTest+strconv.FormatInt(at.TestID, 10))),
		bot.Row(bot.Btn(kbExitTest, cbExit+strconv.FormatInt(at.AttemptID, 10))),
	}}
}

// activeGuardText is «finish or exit first» naming the active test.
func activeGuardText(at *repositories.ActiveTest) string {
	return activeGuardMsg + "\n\nСейчас открыт: " + activeTestLabel(at) +
		".\nПри выходе прогресс сохранится — потом можно продолжить с того же вопроса."
}

// sendGuardFor sends the guard message with «▶️ Продолжить тест» /
// «🚪 Выйти из теста».
func (h *Handler) sendGuardFor(ctx context.Context, chatID int64, at *repositories.ActiveTest) {
	if _, err := h.tg.SendMessage(ctx, chatID, activeGuardText(at), activeGuardKeyboard(at)); err != nil {
		logf("send active-test guard: %v", err)
	}
}

// sendActiveGuard answers an attempt to enter a test while another one is
// active (a race the router guard could not see, e.g. a delayed start after
// a translation wait).
func (h *Handler) sendActiveGuard(ctx context.Context, chatID int64, user *models.User) {
	user.ActiveAttemptID = -1 // force the lookup: the cached row predates the race
	if at := h.activeTest(ctx, user); at != nil {
		h.sendGuardFor(ctx, chatID, at)
		return
	}
	h.sendText(ctx, chatID, activeGuardMsg)
}

// adminTextCommand: the admin commands that stay available during a test
// (operations, not navigation).
func adminTextCommand(cmd string) bool {
	switch cmd {
	case "/grant", "/revoke", "/subinfo", "/refund":
		return true
	}
	return false
}

// guardMessage handles a message while a test is active. true = handled
// (the router stops).
func (h *Handler) guardMessage(ctx context.Context, m *bot.Message, user *models.User, text string) bool {
	if user.ActiveAttemptID == 0 {
		return false
	}
	cmd := commandKey(text)
	if adminTextCommand(cmd) && h.isAdminTG(m.From.ID) {
		return false
	}
	at := h.activeTest(ctx, user)
	if at == nil {
		return false // the attempt was closed meanwhile — no test mode
	}
	if text == kbExitTest || cmd == "/exit" {
		if _, err := h.tg.SendMessage(ctx, m.Chat.ID, exitConfirmText, exitConfirmKeyboard(at.AttemptID)); err != nil {
			logf("send exit confirmation: %v", err)
		}
		return true
	}
	h.sendGuardFor(ctx, m.Chat.ID, at)
	if isMenuButton(text) {
		// The phone still shows the main menu — hide it again.
		h.sendTestModeNotice(ctx, m.Chat.ID)
	}
	return true
}

// callbackAllowedInTest: the inline buttons that work during a test — the
// answers and exit buttons of the ACTIVE attempt, reopening the SAME test
// (resume) and the no-op button.
func callbackAllowedInTest(data string, at *repositories.ActiveTest) bool {
	if data == cbNoop {
		return true
	}
	if strings.HasPrefix(data, cbAnswer) {
		rest := strings.TrimPrefix(data, cbAnswer)
		if i := strings.IndexByte(rest, ':'); i > 0 {
			id, err := strconv.ParseInt(rest[:i], 10, 64)
			return err == nil && id == at.AttemptID
		}
		return false
	}
	r, id, err := matchIDRoute(data)
	if r == nil || err != nil {
		return false
	}
	switch r.prefix {
	case cbExit, cbExitYes, cbExitNo:
		return id == at.AttemptID
	case cbOpenTest:
		return id == at.TestID
	}
	return false
}

// guardCallback handles an inline button while a test is active. true =
// handled (the router stops).
func (h *Handler) guardCallback(ctx context.Context, cb *bot.CallbackQuery, user *models.User, data string) bool {
	if user.ActiveAttemptID == 0 {
		return false
	}
	at := h.activeTest(ctx, user)
	if at == nil || callbackAllowedInTest(data, at) {
		return false
	}
	h.answerCallback(ctx, cb, "Сначала заверши текущий тест или выйди из него")
	h.sendGuardFor(ctx, cb.Message.Chat.ID, at)
	return true
}
