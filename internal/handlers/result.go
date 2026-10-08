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

// showResult renders the attempt summary as a NEW message. cb may be nil
// (called after an answer); in that case chatID must be provided.
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
	h.renderSummary(ctx, sum, user, attemptID, target, "")
}

// renderSummary sends the attempt summary as a NEW message. It is separated
// from showResult so the answer flow can reuse the summary it already built
// (needed to decide whether the next test starts generating) without
// querying the database twice.
// extra (optional) is appended under the score (the quota charge line).
func (h *Handler) renderSummary(ctx context.Context, sum *services.AttemptSummary, user *models.User, attemptID, chatID int64, extra string) {
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
			if sum.Test.TestNumber+1 <= models.MaxVisibleTests {
				fmt.Fprintf(&b, "\n\n🔓 Ты открыл «Тест %d»! Я уже начал его собирать по твоим результатам — обычно это занимает пару минут ⏳", sum.Test.TestNumber+1)
				rows = append(rows, bot.Row(bot.Btn(fmt.Sprintf("➡️ Следующий тест (Тест %d)", sum.Test.TestNumber+1),
					cbPendingID+strconv.FormatInt(sum.Test.SubjectID, 10)+":"+strconv.Itoa(sum.Test.TestNumber+1))))
			} else {
				b.WriteString("\n\n🏆 Это был последний тест цепочки — ты прошёл её целиком!")
			}
		}
		rows = append(rows, bot.Row(bot.Btn("📚 К предметам", cbSubjects)))
	}
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: rows}
	// Send as a new message so the answered question stays visible above.
	if _, err := h.tg.SendMessage(ctx, target, b.String(), kb); err != nil {
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
