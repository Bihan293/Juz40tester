package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
)

// --- Test flow -----------------------------------------------------------------

// openTest opens the test immediately: tapping a test button shows the first
// (or current) question right away. If the user has an unfinished attempt it
// is resumed from where it stopped; otherwise a brand-new attempt is created.
func (h *Handler) openTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, testID int64) {
	test, err := h.quiz.GetTest(ctx, testID)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Тест не найден")
		return
	} else if err != nil {
		logf("get test %d: %v", testID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки теста")
		return
	}

	// A personal weak-topics test opens only for its owner.
	if models.IsOwnedKind(test.Kind) && test.OwnerUserID != user.ID {
		h.answerCallback(ctx, cb, "Этот тест собран под другого ученика")
		return
	}

	// Unlock chain: a chain test opens only when the previous one is at
	// 15🟢 + 5🟡. Tapping a 🔒 button shows what is missing.
	allowed, reason, err := h.quiz.CanOpenTest(ctx, user.ID, test)
	if err != nil {
		logf("can open test %d: %v", testID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки теста")
		return
	}
	if !allowed {
		// A modal alert (with OK) instead of a vanishing toast: the unlock
		// requirement is long and must be readable.
		h.answerAlert(ctx, cb, reason)
		return
	}

	// The callback is acknowledged right away (a silent ack stops the
	// spinner; with subscriptions on it shows «Осталось прохождений
	// сегодня: X/Y»); every further notice goes to the chat as a normal
	// message.
	h.answerCallback(ctx, cb, h.quotaToast(ctx, user, test))

	// Kazakh users: the test content must have a Kazakh version. A cached
	// translation is used right away (ZERO API calls). Otherwise (R-4) a
	// background translation job is queued and the user gets «⏳ Перевод
	// готовится…» — the handler returns immediately and frees its webhook
	// slot; the first question is shown by the waiter as soon as the
	// translation is ready (or in Russian if it failed — a missing
	// translation never blocks a test). Language subjects are never
	// translated (PrepareTranslation reports nothing to wait for).
	if h.deferUntilTranslated(ctx, cb, user, test, func(ctx context.Context) {
		h.startOrResume(ctx, cb, user, test)
	}) {
		return
	}
	h.startOrResume(ctx, cb, user, test)
}

// startOrResume continues the saved attempt of the test or starts a new one
// and shows its current question. The callback is already answered.
func (h *Handler) startOrResume(ctx context.Context, cb *bot.CallbackQuery, user *models.User, test *models.Test) {
	testID := test.ID
	resume, err := h.quiz.ResumeOrNil(ctx, user.ID, testID)
	if err != nil {
		logf("resume lookup %d: %v", testID, err)
		h.failOpenTest(ctx, cb, "Ошибка загрузки теста")
		return
	}
	if resume != nil {
		// Continue the saved attempt from the current question.
		h.showAttempt(ctx, cb, user, resume.ID)
		return
	}

	// The test row is already loaded above — no second GetTest (R-2).
	attempt, err := h.quiz.StartTestFor(ctx, user.ID, test)
	if errors.Is(err, repositories.ErrQuotaExceeded) {
		h.sendQuotaExceeded(ctx, cb.Message.Chat.ID, user)
		return
	}
	if errors.Is(err, repositories.ErrNotFound) {
		h.failOpenTest(ctx, cb, "Тест не найден")
		return
	}
	if err != nil {
		logf("start test %d: %v", testID, err)
		h.failOpenTest(ctx, cb, "Не удалось начать тест")
		return
	}
	// A fresh attempt begins: test mode on (the main menu is replaced by
	// «🚪 Выйти из теста» — replaced, never removed: a ReplyKeyboardRemove
	// made phones open the text keyboard), then its first question.
	h.showAttempt(ctx, cb, user, attempt.ID)
}

// showAttempt enters test mode for the attempt (users.active_attempt_id)
// and shows its current question; when the user just entered the test the
// main menu is hidden right after the question (one extra message, once per
// entry — never per answer). Another active test blocks it (guard message).
func (h *Handler) showAttempt(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	ok, notice := h.enterTest(ctx, cb.Message.Chat.ID, user, attemptID)
	if !ok {
		return
	}
	h.showCurrentQuestion(ctx, cb, user, attemptID)
	h.noticeIfActive(ctx, cb.Message.Chat.ID, user, attemptID, notice)
}

// noticeIfActive sends the test-mode keyboard when the user has just
// entered attemptID and is still inside it (a finished / closed attempt
// already brought the menu back).
func (h *Handler) noticeIfActive(ctx context.Context, chatID int64, user *models.User, attemptID int64, notice bool) {
	if notice && user.ActiveAttemptID == attemptID {
		h.sendTestModeNotice(ctx, chatID)
	}
}

// failOpenTest reports an open-test failure. openTest acknowledges the
// callback right away (Telegram allows exactly ONE answer per
// callback_query), so the error always goes as a plain chat message.
func (h *Handler) failOpenTest(ctx context.Context, cb *bot.CallbackQuery, text string) {
	if _, err := h.tg.SendMessage(ctx, cb.Message.Chat.ID, text, nil); err != nil {
		logf("send open-test failure: %v", err)
	}
}

// loadCurrentQuestion loads the next unanswered question view. Returns nil
// view (and nil error) when the attempt has nothing left to answer.
func (h *Handler) loadCurrentQuestion(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) *services.QuestionView {
	view, err := h.quiz.CurrentQuestion(ctx, attemptID, user)
	if err != nil {
		logf("current question: %v", err)
		if cb != nil {
			h.answerCallback(ctx, cb, "Ошибка загрузки вопроса")
		}
		return nil
	}
	return view
}

// showCurrentQuestion renders the next unanswered question by EDITING the
// message that triggered the callback (used when a test is opened/resumed
// from a test button).
func (h *Handler) showCurrentQuestion(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	view := h.loadCurrentQuestion(ctx, cb, user, attemptID)
	if view == nil {
		// All questions answered -> show the result.
		h.showResult(ctx, cb, user, attemptID, 0)
		h.leaveTest(ctx, cb.Message.Chat.ID, user, attemptID)
		return
	}
	if view.Attempt != nil && view.Attempt.Status != models.AttemptInProgress {
		// A stale button («↩️ Нет, продолжить» of an attempt that was
		// restarted or reaped meanwhile) must not show a question that can
		// no longer be answered.
		text, kb := h.closedAttemptScreen(ctx, user, view.Attempt.TestID)
		h.editMessage(ctx, cb, text, kb)
		h.leaveTest(ctx, cb.Message.Chat.ID, user, attemptID)
		return
	}
	admin := h.isAdminTG(user.TelegramID)
	h.editMessage(ctx, cb, renderQuestionFor(view, admin), questionKeyboardFor(view, admin))
}

// closedAttemptText is shown instead of a question of an attempt that is no
// longer in progress.
const closedAttemptText = "⏹ Эта попытка уже закрыта — тест был начат заново или попытка устарела.\n\nОткрой тест, чтобы продолжить."

// closedAttemptScreen is closedAttemptText above the list the test belongs
// to (the subject's tests grid / the weak-topics picker), so the user can
// reopen the test from there. Falls back to «▶️ Открыть тест» when the test
// cannot be loaded.
func (h *Handler) closedAttemptScreen(ctx context.Context, user *models.User, testID int64) (string, *bot.InlineKeyboardMarkup) {
	if h.quiz != nil {
		test, err := h.quiz.GetTest(ctx, testID)
		if err == nil {
			if listText, listKb, err := h.testListScreen(ctx, user, test); err == nil {
				return closedAttemptText + "\n\n" + listText, listKb
			} else if !errors.Is(err, errNoTestList) {
				logf("test list of closed attempt: %v", err)
			}
		} else {
			logf("closed attempt test %d: %v", testID, err)
		}
	}
	return closedAttemptText, closedAttemptKeyboard(testID)
}

func closedAttemptKeyboard(testID int64) *bot.InlineKeyboardMarkup {
	return &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("▶️ Открыть тест", cbOpenTest+strconv.FormatInt(testID, 10))),
		bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
	}}
}

func renderQuestion(v *services.QuestionView) string { return renderQuestionFor(v, false) }

// correctDisplayIndex is the displayed position of the correct option
// (-1 when unknown).
func correctDisplayIndex(v *services.QuestionView) int {
	if v == nil || v.AttemptQ == nil || v.Question == nil {
		return -1
	}
	for i, orig := range v.AttemptQ.OptionOrder {
		if orig == v.Question.CorrectAnswer && i < len(v.DisplayTexts) {
			return i
		}
	}
	return -1
}

// renderQuestionFor renders a question; admin = the viewer is an
// administrator (ADMIN_IDS): the correct option is marked ✅ and named
// below. Only the RENDERING differs — the answer is checked and counted
// exactly like for every other user.
func renderQuestionFor(v *services.QuestionView, admin bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "❓ Вопрос %d/%d\n\n%s\n\n", v.AttemptQ.Position, v.Total, v.Text)
	correct := -1
	if admin {
		correct = correctDisplayIndex(v)
	}
	for i, text := range v.DisplayTexts {
		mark := ""
		if i == correct {
			mark = " ✅"
		}
		fmt.Fprintf(&b, "%s) %s%s\n", services.OptionLabels[i], text, mark)
	}
	if correct >= 0 {
		fmt.Fprintf(&b, "\n🛠 Админ: правильный ответ — %s", services.OptionLabels[correct])
	}
	return strings.TrimRight(b.String(), "\n")
}

func questionKeyboard(v *services.QuestionView) *bot.InlineKeyboardMarkup {
	return questionKeyboardFor(v, false)
}

func questionKeyboardFor(v *services.QuestionView, admin bool) *bot.InlineKeyboardMarkup {
	rows := make([][]bot.InlineKeyboardButton, 0, len(v.DisplayTexts)+1)
	prefix := fmt.Sprintf("%s%d:%d:", cbAnswer, v.Attempt.ID, v.AttemptQ.Position)
	correct := -1
	if admin {
		correct = correctDisplayIndex(v)
	}
	for i := range v.DisplayTexts {
		label := fmt.Sprintf("%s) %s", services.OptionLabels[i], v.DisplayTexts[i])
		if i == correct {
			label = "✅ " + label
		}
		// Full-width buttons with the option text: easy to read and to tap.
		rows = append(rows, bot.Row(bot.Btn(label, prefix+strconv.Itoa(i))))
	}
	rows = append(rows, bot.Row(bot.Btn("🚪 Выйти", cbExit+strconv.FormatInt(v.Attempt.ID, 10))))
	return &bot.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (h *Handler) handleAnswer(ctx context.Context, cb *bot.CallbackQuery, user *models.User, data string) {
	// data: "ans:<attemptID>:<position>:<displayIndex>"
	parts := strings.Split(strings.TrimPrefix(data, cbAnswer), ":")
	if len(parts) != 3 {
		h.answerCallback(ctx, cb, "Некорректный ответ")
		return
	}
	attemptID, err1 := strconv.ParseInt(parts[0], 10, 64)
	position, err2 := strconv.Atoi(parts[1])
	displayIndex, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		h.answerCallback(ctx, cb, "Некорректный ответ")
		return
	}

	// Resolve displayed letter -> original option label using the stored shuffle.
	view, err := h.quiz.QuestionAtPosition(ctx, attemptID, user, position)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Попытка не найдена")
		return
	}
	if err != nil {
		logf("load attempt question: %v", err)
		h.answerCallback(ctx, cb, "Ошибка")
		return
	}
	original, err := services.MapDisplayToOriginal(view.AttemptQ, displayIndex)
	if err != nil {
		h.answerCallback(ctx, cb, "Некорректный вариант")
		return
	}
	// Test mode: answering a question (e.g. of a paused attempt's older
	// message) means being inside that test. Free for the active attempt
	// (the users row read by Upsert already says so — zero queries).
	notice := false
	if view.Attempt != nil && view.Attempt.Status == models.AttemptInProgress {
		var ok bool
		if ok, notice = h.enterTest(ctx, cb.Message.Chat.ID, user, attemptID); !ok {
			h.answerCallback(ctx, cb, "Сначала заверши текущий тест или выйди из него")
			return
		}
	}

	res, err := h.quiz.SubmitAnswer(ctx, user.ID, attemptID, position, original)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Попытка не найдена")
		return
	}
	if errors.Is(err, repositories.ErrAnswerOutOfOrder) {
		// Stale / forged callback for a question that is not the current one.
		h.answerCallback(ctx, cb, "Этот вопрос уже неактуален")
		return
	}
	if errors.Is(err, repositories.ErrAttemptClosed) {
		// Late duplicate of the final answer, or a stale question of an
		// attempt that was closed meanwhile (restarted / reaped): nothing
		// failed — the answer simply belongs to a finished attempt.
		h.answerCallback(ctx, cb, "Эта попытка уже завершена — открой тест заново")
		return
	}
	if err != nil {
		logf("submit answer: %v", err)
		h.answerCallback(ctx, cb, "Не удалось сохранить ответ")
		return
	}
	if res.AlreadyAnswered {
		// Duplicate callback — answer already counted, just acknowledge.
		h.answerCallback(ctx, cb, "Ответ уже засчитан")
		return
	}
	// BUG FIX: the happy path never acknowledged the callback, so the tapped
	// A/B/C/D button kept spinning for ~15 s after every single answer.
	// The acknowledgement (answerCallbackQuery — free, outside the
	// TG_MAX_RPS limiter) carries the verdict as a toast. From here on the
	// single allowed answer is spent — later errors go to the chat.
	h.answerCallback(ctx, cb, verdictToast(res))

	// ONE rate-limited Telegram call per answer: the SAME message is edited
	// into «verdict of the previous question + the next question with its
	// keyboard» (before: an edit of the answered question + a NEW message
	// with the next one = 2 calls). The chat no longer keeps the history of
	// the answered questions — accepted by the owner for twice the
	// throughput. The old A/B/C/D keyboard disappears with the edit, and a
	// stale tap on a copy of it is refused by SubmitAnswer
	// (ErrAnswerOutOfOrder / AlreadyAnswered) above.
	header := answerHeader(view, res)
	if res.Finished {
		// Test over -> build the summary FIRST: the fresh 🟢/🟡 counts decide
		// whether the next chain test starts generating (only at 15🟢+5🟡).
		sum, err := h.quiz.BuildSummary(ctx, attemptID, user.ID)
		if err != nil {
			logf("summary: %v", err)
			h.editMessage(ctx, cb, withHeader(header, "Ошибка загрузки результата 😔"), nil)
			return
		}
		outcome := h.quiz.OnTestCompleted(ctx, user.ID, sum.Test,
			sum.StatusCounts[models.StatusMastered], sum.StatusCounts[models.StatusPartial])
		extra := ""
		if res.Charged {
			extra = h.chargedLine(ctx, user)
		}
		// The last question turns into the result screen (one edit), then
		// the bottom menu comes back (one send, as before).
		h.renderSummaryInto(ctx, cb, header, sum, user, attemptID, cb.Message.Chat.ID, extra, outcome)
		// Test over: test mode off, the main menu comes back.
		h.leaveTest(ctx, cb.Message.Chat.ID, user, attemptID)
		return
	}
	h.showNextQuestion(ctx, cb, user, attemptID, view.Attempt, header)
	h.noticeIfActive(ctx, cb.Message.Chat.ID, user, attemptID, notice)
}

// showNextQuestion edits the answered message into the verdict header plus
// the next unanswered question (ONE Telegram call; editMessage falls back
// to a new message when the edit is impossible, so the test never gets
// stuck).
func (h *Handler) showNextQuestion(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64, answered *models.TestAttempt, header string) {
	next, err := h.quiz.CurrentQuestion(ctx, attemptID, user)
	if err != nil {
		logf("current question: %v", err)
		// The answer is saved; offer to continue from the next question
		// instead of leaving the answered keyboard on screen.
		var kb *bot.InlineKeyboardMarkup
		if answered != nil {
			kb = &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
				bot.Row(bot.Btn("▶️ Продолжить тест", cbOpenTest+strconv.FormatInt(answered.TestID, 10))),
				bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
			}}
		}
		h.editMessage(ctx, cb, withHeader(header, "Ошибка загрузки вопроса 😔 Нажми «Продолжить тест»."), kb)
		return
	}
	if next == nil {
		// Nothing left although this answer did not finish the attempt (a
		// concurrent final answer): show the result in the same message.
		h.showResultInto(ctx, cb, user, attemptID, header)
		h.leaveTest(ctx, cb.Message.Chat.ID, user, attemptID)
		return
	}
	if next.Attempt != nil && next.Attempt.Status != models.AttemptInProgress {
		text, kb := h.closedAttemptScreen(ctx, user, next.Attempt.TestID)
		h.editMessage(ctx, cb, withHeader(header, text), kb)
		h.leaveTest(ctx, cb.Message.Chat.ID, user, attemptID)
		return
	}
	admin := h.isAdminTG(user.TelegramID)
	h.editMessage(ctx, cb, withHeader(header, renderQuestionFor(next, admin)), questionKeyboardFor(next, admin))
}

// verdictToast is the callback toast shown right after an answer.
func verdictToast(res *repositories.AnswerResult) string {
	if res.Correct {
		return "🟢 Правильно"
	}
	return "🔴 Неправильно"
}

// headerOptionMaxRunes caps the correct option quoted in the verdict header
// (the full option is long only in rare generated tests).
const headerOptionMaxRunes = 300

// answerHeader is the short verdict of the previous question shown above
// the next question: «🟢 Правильно» or «🔴 Неправильно — правильный ответ:
// D) <text>».
func answerHeader(v *services.QuestionView, res *repositories.AnswerResult) string {
	if res.Correct {
		return "🟢 Правильно"
	}
	if v == nil || v.AttemptQ == nil || v.Question == nil {
		return "🔴 Неправильно"
	}
	for i, orig := range v.AttemptQ.OptionOrder {
		if orig == v.Question.CorrectAnswer && i < len(v.DisplayTexts) && i < len(services.OptionLabels) {
			text := v.DisplayTexts[i]
			if r := []rune(text); len(r) > headerOptionMaxRunes {
				text = string(r[:headerOptionMaxRunes-1]) + "…"
			}
			return fmt.Sprintf("🔴 Неправильно — правильный ответ: %s) %s", services.OptionLabels[i], text)
		}
	}
	return "🔴 Неправильно"
}

// messageMaxUTF16 is the budget of one message text in UTF-16 code units
// (Telegram's limit is 4096; a margin is kept for safety).
const messageMaxUTF16 = 4000

// withHeader puts the verdict header above body (separated by an empty
// line) and keeps the whole text within Telegram's 4096 limit: the body
// (the question and its options) wins — the header is shortened to the
// bare verdict or dropped when the body is very long (the toast still
// shows the verdict).
func withHeader(header, body string) string {
	if utf16Len(body) > messageMaxUTF16 {
		body = truncateUTF16(body, messageMaxUTF16-1)
	}
	if header == "" {
		return body
	}
	room := messageMaxUTF16 - utf16Len(body) - 2
	if utf16Len(header) > room {
		short, _, _ := strings.Cut(header, " — ")
		if utf16Len(short) > room {
			return body
		}
		header = short
	}
	return header + "\n\n" + body
}

func (h *Handler) retryTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	// NOTE: the router has already answered this callback (only ONE answer
	// is allowed) — errors are reported as chat messages.
	sum, err := h.quiz.BuildSummary(ctx, attemptID, user.ID)
	if err != nil {
		logf("retry summary %d: %v", attemptID, err)
		h.sendText(ctx, cb.Message.Chat.ID, "Ошибка 😔 Попробуй ещё раз.")
		return
	}
	// The user may have switched the test language to Kazakh since this test
	// was created — the Kazakh version must exist before the new run. A
	// cached translation is reused at once (zero API calls); otherwise a
	// background job is queued and the run starts when it is ready (R-4 —
	// the handler never waits). A failure falls back to the Russian master
	// version, never blocks the retry.
	testID := sum.Test.ID
	if h.deferUntilTranslated(ctx, cb, user, sum.Test, func(ctx context.Context) {
		h.restartRun(ctx, cb, user, testID)
	}) {
		return
	}
	h.restartRun(ctx, cb, user, testID)
}

// restartRun starts a brand-new attempt of the test and shows its first
// question (retry flow; the callback is already answered).
func (h *Handler) restartRun(ctx context.Context, cb *bot.CallbackQuery, user *models.User, testID int64) {
	// Every retry is a brand-new attempt with fresh shuffled question and
	// option orders.
	newAttempt, err := h.quiz.RestartTest(ctx, user.ID, testID)
	if errors.Is(err, repositories.ErrQuotaExceeded) {
		h.sendQuotaExceeded(ctx, cb.Message.Chat.ID, user)
		return
	}
	if err != nil {
		logf("retry test: %v", err)
		h.sendText(ctx, cb.Message.Chat.ID, "Не удалось начать тест 😔")
		return
	}
	h.showAttempt(ctx, cb, user, newAttempt.ID)
}

// confirmExit replaces the question with a yes/no confirmation instead of
// exiting right away — an accidental tap on 🚪 must not lose the flow.
func (h *Handler) confirmExit(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	if err := h.quiz.Exit(ctx, attemptID, user.ID); err != nil {
		logf("exit confirm attempt %d: %v", attemptID, err)
		h.sendText(ctx, cb.Message.Chat.ID, "Попытка не найдена")
		return
	}
	h.editMessage(ctx, cb, exitConfirmText, exitConfirmKeyboard(attemptID))
}

// exitTest performs the actual exit after the user confirmed it.
// The user lands back in the list the test was opened from (the subject's
// tests grid, or the weak-topics picker for a personal test).
func (h *Handler) exitTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	test, err := h.quiz.ExitTest(ctx, attemptID, user.ID)
	if err != nil {
		logf("exit attempt %d: %v", attemptID, err)
		h.answerCallback(ctx, cb, "Не удалось выйти")
		return
	}
	h.answerCallback(ctx, cb, "Прогресс сохранён")
	note := exitNoteChain
	switch test.Kind {
	case models.TestKindPersonal:
		note = exitNotePersonal
	case models.TestKindCustom:
		note = customExitNote
	}
	text, kb := h.afterTestScreen(ctx, user, test, note, nil)
	h.editMessage(ctx, cb, withHeader("", text), kb)
	// Test mode off: the main menu comes back. Nothing is charged — the
	// attempt stays in progress (resumable ⏸); only a completed attempt
	// is charged against the daily quota.
	h.leaveTest(ctx, cb.Message.Chat.ID, user, attemptID)
}

// Notes on top of the list after «✅ Да, выйти».
const (
	exitNoteChain    = "🚪 Вы вышли из теста. Попытка сохранена — нажми на тест с ⏸, чтобы продолжить с места остановки."
	exitNotePersonal = "🚪 Вы вышли из теста. Попытка сохранена — выбери предмет ниже, чтобы продолжить с места остановки."
)

// cancelExit returns the user to the current question of the attempt.
func (h *Handler) cancelExit(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	h.answerCallback(ctx, cb, "Продолжаем 💪")
	h.showAttempt(ctx, cb, user, attemptID)
}
