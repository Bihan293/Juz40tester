package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/services"
)

// --- Result -----------------------------------------------------------------

// showResult renders the attempt summary as a NEW message. cb may be nil;
// in that case chatID must be provided.
func (h *Handler) showResult(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID, chatID int64) {
	sum, err := h.quiz.BuildSummary(ctx, attemptID, user.ID)
	if err != nil {
		logf("summary: %v", err)
		if cb != nil {
			h.answerCallback(ctx, cb, "Ошибка загрузки результата")
		}
		return
	}

	// Resolve the target chat: from the callback message or the argument.
	target := chatID
	if cb != nil && cb.Message != nil {
		target = cb.Message.Chat.ID
	}
	// Re-showing a finished attempt unlocks nothing new and starts nothing.
	h.renderSummary(ctx, sum, user, attemptID, target, "", services.CompletionOutcome{})
}

// showResultInto edits the message of cb into header + the attempt summary
// (the answer flow, when the attempt turned out to be finished already).
func (h *Handler) showResultInto(ctx context.Context, cb *bot.CallbackQuery, user *models.User, attemptID int64, header string) {
	sum, err := h.quiz.BuildSummary(ctx, attemptID, user.ID)
	if err != nil {
		logf("summary: %v", err)
		h.editMessage(ctx, cb, withHeader(header, "Ошибка загрузки результата 😔"), nil)
		return
	}
	h.renderSummaryInto(ctx, cb, header, sum, user, attemptID, cb.Message.Chat.ID, "", services.CompletionOutcome{})
}

// renderSummary sends the attempt summary as a NEW message. It is separated
// from showResult so the answer flow can reuse the summary it already built
// (needed to decide whether the next test starts generating) without
// querying the database twice.
// extra (optional) is appended under the score (the quota charge line).
// outcome is what the completion of the attempt did (a new unlock, the next
// test's generation) — the text never claims more than that.
func (h *Handler) renderSummary(ctx context.Context, sum *services.AttemptSummary, user *models.User, attemptID, chatID int64, extra string, outcome services.CompletionOutcome) {
	h.renderSummaryInto(ctx, nil, "", sum, user, attemptID, chatID, extra, outcome)
}

// renderSummaryInto renders the attempt summary. With cb (the answer flow)
// the answered question message itself is EDITED into «verdict header +
// result» — one Telegram call; when the edit is impossible editMessage sends
// the result as a new message. Without cb the result is sent as a new
// message.
//
// Under the result the user is back in the menu the test was opened from
// (afterTestScreen): the subject's tests grid for a chain test, the
// weak-topics picker for a personal test — not a separate mini-menu with
// only «К предметам / Главное меню». The bottom reply menu is never touched.
func (h *Handler) renderSummaryInto(ctx context.Context, cb *bot.CallbackQuery, header string, sum *services.AttemptSummary, user *models.User, attemptID, chatID int64, extra string, outcome services.CompletionOutcome) {
	text, kb := h.resultScreen(ctx, sum, user, extra, outcome)
	if cb != nil && cb.Message != nil {
		h.editMessage(ctx, cb, withHeader(header, text), kb)
	} else if _, err := h.tg.SendMessage(ctx, chatID, withHeader(header, text), kb); err != nil {
		logf("send result: %v", err)
	}
}

// resultScreen builds the result text and keyboard (see renderSummaryInto).
func (h *Handler) resultScreen(ctx context.Context, sum *services.AttemptSummary, user *models.User, extra string, outcome services.CompletionOutcome) (string, *bot.InlineKeyboardMarkup) {
	var b strings.Builder
	b.WriteString("🎉 Тест завершён!\n\n")
	fmt.Fprintf(&b, "🟢 Закреплено: %d\n", sum.StatusCounts[models.StatusMastered])
	fmt.Fprintf(&b, "🟡 В процессе: %d\n", sum.StatusCounts[models.StatusPartial])
	fmt.Fprintf(&b, "🔴 Требует повторения: %d\n\n", sum.StatusCounts[models.StatusNone])
	fmt.Fprintf(&b, "Результат: %d/%d", sum.Attempt.CorrectCount, sum.Total)
	if extra != "" {
		b.WriteString("\n\n" + extra)
	}

	green := sum.StatusCounts[models.StatusMastered]
	yellow := sum.StatusCounts[models.StatusPartial]
	var top [][]bot.InlineKeyboardButton
	// Personal weak-topics test: once it is mastered (15🟢 + 5🟡) it can be
	// finished — the test is deleted and the next weak-topics run generates
	// a fresh one.
	if sum.Test.Kind == models.TestKindCustom {
		if models.MeetsUnlockBar(green, yellow) {
			b.WriteString("\n\n🏁 Отличный результат! Можешь закончить тест — он удалится, и освободится место для нового.")
			top = append(top, bot.Row(bot.Btn("🏁 Закончить тест", cbCustomFinish+strconv.FormatInt(sum.Test.ID, 10))))
		} else {
			fmt.Fprintf(&b, "\n\nДоведи тест до %d🟢 + %d🟡 — тогда его можно будет закончить.", models.UnlockGreen, models.UnlockYellow)
		}
	} else if sum.Test.Kind == models.TestKindPersonal {
		if models.MeetsUnlockBar(green, yellow) {
			b.WriteString("\n\n🏁 Ты закрыл эти слабые темы! Можешь закончить тест — в следующий раз соберу новый.")
			top = append(top, bot.Row(bot.Btn("🏁 Закончить тест", cbFinish+strconv.FormatInt(sum.Test.ID, 10))))
		} else {
			fmt.Fprintf(&b, "\n\nДоведи тест до %d🟢 + %d🟡 — тогда его можно будет закончить и получить новый.", models.UnlockGreen, models.UnlockYellow)
		}
	} else if models.MeetsUnlockBar(green, yellow) {
		// Chain test: reaching the bar unlocks the next test AND starts its
		// generation right away. Only what actually happened is announced:
		// a retry of an earlier test opens nothing new, and «я уже начал его
		// собирать» is said only while the next test is really being
		// generated. The next test itself is a button of the tests grid below.
		if next := sum.Test.TestNumber + 1; next <= models.MaxVisibleTests {
			switch {
			case outcome.NewUnlock && outcome.NextGenerating:
				fmt.Fprintf(&b, "\n\n🔓 Ты открыл «Тест %d»! Я уже начал его собирать по твоим результатам — обычно это занимает пару минут ⏳", next)
			case outcome.NewUnlock:
				fmt.Fprintf(&b, "\n\n🔓 Ты открыл «Тест %d»!", next)
			}
		} else {
			b.WriteString("\n\n🏆 Это был последний тест цепочки — ты прошёл её целиком!")
		}
	}
	return h.afterTestScreen(ctx, user, sum.Test, b.String(), top)
}

// afterTestScreen is the screen the user lands on when a test is over
// (finished, exited, closed): note on top, then the list the test belongs
// to — the subject's tests grid (exactly what «open subject» shows: Тест 1,
// Тест 2, …, «К предметам», «Главное меню») for a chain test, the
// weak-topics picker for a personal test. top (optional) are extra button
// rows placed above the list. If the list cannot be loaded the user still
// gets a way back (К предметам / Слабые темы + Главное меню).
func (h *Handler) afterTestScreen(ctx context.Context, user *models.User, test *models.Test, note string, top [][]bot.InlineKeyboardButton) (string, *bot.InlineKeyboardMarkup) {
	rows := append([][]bot.InlineKeyboardButton(nil), top...)
	listText, listKb, err := h.testListScreen(ctx, user, test)
	if err != nil {
		if !errors.Is(err, errNoTestList) {
			logf("test list after test: %v", err)
		}
		if test != nil && test.Kind == models.TestKindPersonal {
			rows = append(rows, bot.Row(bot.Btn("🎯 Слабые темы", cbWeakMenu)))
		} else if test != nil && test.Kind == models.TestKindCustom {
			rows = append(rows, bot.Row(bot.Btn("✨ Свой тест", cbCustomMenu)))
		} else {
			rows = append(rows, bot.Row(bot.Btn("📚 К предметам", cbSubjects)))
		}
		rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
		return note, &bot.InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	rows = append(rows, listKb.InlineKeyboard...)
	return note + "\n\n" + listText, &bot.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// errNoTestList: there is nothing to load the test list from (no test, or a
// handler without the quiz service in unit tests).
var errNoTestList = errors.New("no test list")

// testListScreen renders the list a test belongs to: the subject's tests
// grid on the remembered page (chain test) or the weak-topics picker
// (personal test).
func (h *Handler) testListScreen(ctx context.Context, user *models.User, test *models.Test) (string, *bot.InlineKeyboardMarkup, error) {
	if h.quiz == nil || test == nil || user == nil {
		return "", nil, errNoTestList
	}
	if test.Kind == models.TestKindPersonal {
		return h.renderWeakMenu(ctx, user)
	}
	if test.Kind == models.TestKindCustom {
		return h.renderCustomSubject(ctx, user, test.SubjectID)
	}
	page, err := h.quiz.SavedTestsPage(ctx, user.ID, test.SubjectID)
	if err != nil {
		logf("saved page %d/%d: %v", user.ID, test.SubjectID, err)
		page = 0
	}
	return h.renderSubject(ctx, user, test.SubjectID, page, false)
}

// finishPersonalTest handles "🏁 Закончить тест": the mastered personal
// test is deleted, so the next weak-topics visit generates a fresh one. The
// user lands back in the weak-topics picker.
func (h *Handler) finishPersonalTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, testID int64) {
	if err := h.quiz.FinishPersonalTest(ctx, user.ID, testID); err != nil {
		logf("finish personal test %d/%d: %v", user.ID, testID, err)
		h.answerCallback(ctx, cb, "Не удалось закончить тест")
		return
	}
	h.answerCallback(ctx, cb, "Тест завершён 🎉")
	text, kb := h.afterTestScreen(ctx, user, &models.Test{ID: testID, Kind: models.TestKindPersonal},
		"🏁 Тест по слабым темам завершён и удалён. Когда захочешь — выбери предмет ниже, и я соберу новый тест по актуальным пробелам.", nil)
	h.editMessage(ctx, cb, withHeader("", text), kb)
}
