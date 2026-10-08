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
	if test.Kind == models.TestKindPersonal && test.OwnerUserID != user.ID {
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
		// Continue the saved attempt from the current question — the menu
		// must be hidden for the resumed run too (the user may have left the
		// test earlier and got the menu back).
		h.hideReplyKeyboard(ctx, cb.Message.Chat.ID)
		h.showCurrentQuestion(ctx, cb, user, resume.ID)
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
	// A fresh attempt begins: hide the bottom menu for the whole run.
	h.hideReplyKeyboard(ctx, cb.Message.Chat.ID)
	// Show the first question of the new attempt.
	h.showCurrentQuestion(ctx, cb, user, attempt.ID)
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
		return
	}
	admin := h.isAdminTG(user.TelegramID)
	h.editMessage(ctx, cb, renderQuestionFor(view, admin), questionKeyboardFor(view, admin))
}

// sendCurrentQuestion sends the next unanswered question as a NEW message
// (used after an answer, so the answered question above stays untouched).
func (h *Handler) sendCurrentQuestion(ctx context.Context, chatID int64, user *models.User, attemptID int64) {
	view := h.loadCurrentQuestion(ctx, nil, user, attemptID)
	if view == nil {
		// All questions answered -> show the result.
		h.showResult(ctx, nil, user, attemptID, chatID)
		return
	}
	admin := h.isAdminTG(user.TelegramID)
	if _, err := h.tg.SendMessage(ctx, chatID, renderQuestionFor(view, admin), questionKeyboardFor(view, admin)); err != nil {
		logf("send question: %v", err)
	}
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
	// From here on the single allowed answer is spent — later errors go to
	// the chat as plain messages.
	h.answerCallback(ctx, cb, "")

	// Finalize the answered message: verdict + option marks + question
	// knowledge level, and REMOVE the A/B/C/D keyboard so the question
	// cannot be answered twice. The message is never edited again.
	h.editMessage(ctx, cb, renderAnswered(view, res), nil)

	chatID := cb.Message.Chat.ID
	if res.Finished {
		// Test over -> build the summary FIRST: the fresh 🟢/🟡 counts decide
		// whether the next chain test starts generating (only at 15🟢+5🟡),
		// then send the result as a new message below.
		sum, err := h.quiz.BuildSummary(ctx, attemptID, user.ID)
		if err != nil {
			logf("summary: %v", err)
			h.sendText(ctx, chatID, "Ошибка загрузки результата 😔")
			return
		}
		h.quiz.OnTestCompleted(ctx, user.ID, sum.Test,
			sum.StatusCounts[models.StatusMastered], sum.StatusCounts[models.StatusPartial])
		extra := ""
		if res.Charged {
			extra = h.chargedLine(ctx, user)
		}
		h.renderSummary(ctx, sum, user, attemptID, chatID, extra)
		return
	}
	// The next question is sent as a NEW message right below the answered
	// one; the answered message keeps its final state.
	h.sendCurrentQuestion(ctx, chatID, user, attemptID)
}

// renderAnswered renders the question with the verdict and the correct
// option revealed (🟢). The wrong selection is marked 🔴.
func renderAnswered(v *services.QuestionView, res *repositories.AnswerResult) string {
	var b strings.Builder
	if res.Correct {
		b.WriteString("🟢 Правильно\n\n")
	} else {
		b.WriteString("🔴 Неправильно\n\n")
	}
	fmt.Fprintf(&b, "❓ Вопрос %d/%d\n\n%s\n\n", v.AttemptQ.Position, v.Total, v.Text)
	for i, orig := range v.AttemptQ.OptionOrder {
		mark := ""
		switch {
		case orig == v.Question.CorrectAnswer:
			mark = " 🟢"
		case orig == res.SelectedAnswer:
			mark = " 🔴"
		}
		fmt.Fprintf(&b, "%s) %s%s\n", services.OptionLabels[i], v.DisplayTexts[i], mark)
	}
	if !res.Correct {
		correctText := ""
		for i, orig := range v.AttemptQ.OptionOrder {
			if orig == v.Question.CorrectAnswer {
				correctText = fmt.Sprintf("%s) %s", services.OptionLabels[i], v.DisplayTexts[i])
				break
			}
		}
		fmt.Fprintf(&b, "\n🟢 Правильный ответ: %s", correctText)
	}
	// Current knowledge level of this question after the answer.
	fmt.Fprintf(&b, "\n\nУровень вопроса: %s %d", models.StatusEmoji(res.NewStatus), res.NewStatus)
	return b.String()
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
	// The menu was restored with the result screen — hide it again for the
	// new run (retry bypasses openTest, which normally does this).
	h.hideReplyKeyboard(ctx, cb.Message.Chat.ID)
	h.showCurrentQuestion(ctx, cb, user, newAttempt.ID)
}

// confirmExit replaces the question with a yes/no confirmation instead of
// exiting right away — an accidental tap on 🚪 must not lose the flow.
func (h *Handler) confirmExit(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	if err := h.quiz.Exit(ctx, attemptID, user.ID); err != nil {
		logf("exit confirm attempt %d: %v", attemptID, err)
		h.sendText(ctx, cb.Message.Chat.ID, "Попытка не найдена")
		return
	}
	idStr := strconv.FormatInt(attemptID, 10)
	h.editMessage(ctx, cb, "❓ Выйти из теста?\n\nПрогресс сохранится — потом можно будет продолжить с того же вопроса.",
		&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
			bot.Row(
				bot.Btn("✅ Да, выйти", cbExitYes+idStr),
				bot.Btn("↩️ Нет, продолжить", cbExitNo+idStr),
			),
		}})
}

// exitTest performs the actual exit after the user confirmed it.
func (h *Handler) exitTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	if err := h.quiz.Exit(ctx, attemptID, user.ID); err != nil {
		logf("exit attempt %d: %v", attemptID, err)
		h.answerCallback(ctx, cb, "Не удалось выйти")
		return
	}
	h.answerCallback(ctx, cb, "Прогресс сохранён")
	h.editMessage(ctx, cb, "🚪 Вы вышли из теста. Попытка сохранена — её можно продолжить с места остановки.",
		&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
			bot.Row(bot.Btn("📚 К предметам", cbSubjects)),
			bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
		}})
	// Test is no longer running — give the bottom menu back.
	h.restoreReplyKeyboard(ctx, cb.Message.Chat.ID)
}

// cancelExit returns the user to the current question of the attempt.
func (h *Handler) cancelExit(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64) {
	h.answerCallback(ctx, cb, "Продолжаем 💪")
	h.showCurrentQuestion(ctx, cb, user, attemptID)
}
