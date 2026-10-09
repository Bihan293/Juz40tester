package handlers

import (
	"context"
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
// result» — one Telegram call instead of an edit plus a new message; when
// the edit is impossible editMessage sends the result as a new message.
// Without cb the result is sent as a new message. Either way the bottom
// menu (hidden during the test) is restored afterwards: a reply keyboard
// can only come with a NEW message.
func (h *Handler) renderSummaryInto(ctx context.Context, cb *bot.CallbackQuery, header string, sum *services.AttemptSummary, user *models.User, attemptID, chatID int64, extra string, outcome services.CompletionOutcome) {
	target := chatID
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
	idStr := strconv.FormatInt(attemptID, 10)
	rows := [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("🔄 Пройти тест ещё раз", cbRetry+idStr)),
	}
	// Personal weak-topics test: once it is mastered (15🟢 + 5🟡) it can be
	// finished — the test is deleted and the next weak-topics run generates
	// a fresh one.
	if sum.Test.Kind == models.TestKindPersonal {
		if models.MeetsUnlockBar(green, yellow) {
			b.WriteString("\n\n🏁 Ты закрыл эти слабые темы! Можешь закончить тест — в следующий раз соберу новый.")
			rows = append(rows, bot.Row(bot.Btn("🏁 Закончить тест", cbFinish+strconv.FormatInt(sum.Test.ID, 10))))
		} else {
			fmt.Fprintf(&b, "\n\nДоведи тест до %d🟢 + %d🟡 — тогда его можно будет закончить и получить новый.", models.UnlockGreen, models.UnlockYellow)
		}
		rows = append(rows, bot.Row(bot.Btn("🎯 Слабые темы", cbWeakMenu)))
	} else {
		// Chain test: reaching the bar unlocks the next test AND starts its
		// generation right away (that is the only trigger the user wants).
		// Say it here so the user knows the next test is on its way — but
		// only when a next test actually exists (the chain is capped).
		if models.MeetsUnlockBar(green, yellow) {
			if next := sum.Test.TestNumber + 1; next <= models.MaxVisibleTests {
				// Only what actually happened is announced: a retry of an
				// earlier test opens nothing new, and «я уже начал его
				// собирать» is said only while the next test is really being
				// generated (not when it already exists or generation is off).
				switch {
				case outcome.NewUnlock && outcome.NextGenerating:
					fmt.Fprintf(&b, "\n\n🔓 Ты открыл «Тест %d»! Я уже начал его собирать по твоим результатам — обычно это занимает пару минут ⏳", next)
				case outcome.NewUnlock:
					fmt.Fprintf(&b, "\n\n🔓 Ты открыл «Тест %d»!", next)
				}
				rows = append(rows, bot.Row(bot.Btn(fmt.Sprintf("➡️ Следующий тест (Тест %d)", next),
					cbPendingID+strconv.FormatInt(sum.Test.SubjectID, 10)+":"+strconv.Itoa(next))))
			} else {
				b.WriteString("\n\n🏆 Это был последний тест цепочки — ты прошёл её целиком!")
			}
		}
		rows = append(rows, bot.Row(bot.Btn("📚 К предметам", cbSubjects)))
	}
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: rows}
	if cb != nil && cb.Message != nil {
		h.editMessage(ctx, cb, withHeader(header, b.String()), kb)
	} else if _, err := h.tg.SendMessage(ctx, target, withHeader(header, b.String()), kb); err != nil {
		logf("send result: %v", err)
	}
	// The run is over — the bottom menu (hidden at the start) comes back.
	h.restoreReplyKeyboard(ctx, target)
}

// finishPersonalTest handles "🏁 Закончить тест": the mastered personal
// test is deleted, so the next weak-topics visit generates a fresh one.
func (h *Handler) finishPersonalTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, testID int64) {
	if err := h.quiz.FinishPersonalTest(ctx, user.ID, testID); err != nil {
		logf("finish personal test %d/%d: %v", user.ID, testID, err)
		h.answerCallback(ctx, cb, "Не удалось закончить тест")
		return
	}
	h.answerCallback(ctx, cb, "Тест завершён 🎉")
	h.editMessage(ctx, cb, "🏁 Тест по слабым темам завершён и удалён.\n\nКогда захочешь — нажми «🎯 Слабые темы» в главном меню, и я соберу новый тест по актуальным пробелам.",
		&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
			bot.Row(bot.Btn("🎯 Слабые темы", cbWeakMenu)),
			bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
		}})
	// The bottom menu was already restored together with the result screen
	// — no extra note needed here.
}
