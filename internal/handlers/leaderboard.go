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

// --- Leaderboard ----------------------------------------------------------------

// renderLeaderboardMenu renders the leaderboard picker: one button per
// subject (levels + 🟢 board of that subject) plus the 🔥 streak board.
func (h *Handler) renderLeaderboardMenu(ctx context.Context) (string, *bot.InlineKeyboardMarkup, error) {
	subjects, err := h.quiz.ListSubjects(ctx)
	if err != nil {
		return "", nil, err
	}
	buttons := make([]bot.InlineKeyboardButton, 0, len(subjects)+1)
	buttons = append(buttons, bot.Btn("🔥 Огоньки (дни подряд)", cbLbStreak))
	for _, s := range subjects {
		buttons = append(buttons, bot.Btn(subjectEmoji(s.Name)+" "+s.Name, cbLbSubject+strconv.FormatInt(s.ID, 10)))
	}
	rows := bot.ChunkButtons(buttons, 1)
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	return "🏆 Таблица лидеров\n\nВыбери, что смотрим:\n• 🔥 Огоньки — кто сколько дней подряд занимается;\n• Предмет — кто сколько уровней открыл и у кого больше всего 🟢.", &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func (h *Handler) showLeaderboardMenu(ctx context.Context, chatID int64) {
	text, kb, err := h.renderLeaderboardMenu(ctx)
	if err != nil {
		logf("leaderboard menu: %v", err)
		if _, err := h.tg.SendMessage(ctx, chatID, "Ошибка загрузки 😔", nil); err != nil {
			logf("send leaderboard menu error: %v", err)
		}
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		logf("send leaderboard menu: %v", err)
	}
}

func (h *Handler) editLeaderboardMenu(ctx context.Context, cb *bot.CallbackQuery) {
	text, kb, err := h.renderLeaderboardMenu(ctx)
	if err != nil {
		logf("leaderboard menu: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// lbName renders a leaderboard row name: first name, falling back to the
// @username, then to a neutral placeholder.
func lbName(e *models.LeaderboardEntry) string {
	if n := strings.TrimSpace(e.FirstName); n != "" {
		return n
	}
	if n := strings.TrimSpace(e.Username); n != "" {
		return "@" + n
	}
	return "Участник"
}

// lbLevel is the displayed chain level of a leaderboard row. After the last
// chain test is passed the raw level is MaxVisibleTests+1 (the unlock
// watermark), which is not a real test — the bot shows at most
// MaxVisibleTests everywhere else («Открыто тестов»), so does the board.
func lbLevel(e *models.LeaderboardEntry) int {
	return models.ClampVisibleTests(e.UnlockedTests)
}

// flame renders the streak suffix of a leaderboard row (" 🔥5"), empty when
// the streak has not lit up yet (day one).
func flame(e *models.LeaderboardEntry) string {
	if e.StreakDays >= 2 {
		return fmt.Sprintf(" 🔥%d", e.StreakDays)
	}
	return ""
}

// showStreakLeaderboard renders the 🔥 board: longest daily streaks first.
func (h *Handler) showStreakLeaderboard(ctx context.Context, cb *bot.CallbackQuery) {
	entries, err := h.quiz.StreakLeaderboard(ctx, services.LeaderboardSize)
	if err != nil {
		logf("streak leaderboard: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	var b strings.Builder
	b.WriteString("🔥 Топ по огонькам\n\n")
	if len(entries) == 0 {
		b.WriteString("Пока пусто — заходи каждый день, и твой огонёк появится здесь!")
	} else {
		b.WriteString("Дней подряд занимаются:\n\n")
		for i := range entries {
			e := &entries[i]
			mark := "—"
			if e.StreakDays >= 2 {
				mark = fmt.Sprintf("🔥 %d %s", e.StreakDays, dayWord(e.StreakDays))
			}
			fmt.Fprintf(&b, "%d. %s — %s\n", i+1, lbName(e), mark)
		}
		b.WriteString("\nОгонёк растёт каждый день, когда ты заходишь, и гаснет, если пропустить день.")
	}
	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("⬅️ К таблице лидеров", cbLeaderboard)),
	}}
	h.editMessage(ctx, cb, b.String(), kb)
}

// showSubjectLeaderboard renders the board of one subject: unlocked levels
// and total 🟢 per user.
func (h *Handler) showSubjectLeaderboard(ctx context.Context, cb *bot.CallbackQuery, subjectID int64) {
	subject, err := h.quiz.GetSubject(ctx, subjectID)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Предмет не найден")
		return
	}
	if err != nil {
		logf("leaderboard subject %d: %v", subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	byLevels, err := h.quiz.UnlockedTestsLeaderboard(ctx, subjectID, services.LeaderboardSize)
	if err != nil {
		logf("levels leaderboard %d: %v", subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}
	byGreen, err := h.quiz.GreenLeaderboard(ctx, subjectID, services.LeaderboardSize)
	if err != nil {
		logf("green leaderboard %d: %v", subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🏆 %s %s\n\n", subjectEmoji(subject.Name), subject.Name)
	b.WriteString("По открытым уровням:\n")
	if len(byLevels) == 0 {
		b.WriteString("Пока никто не начал — будь первым!\n")
	} else {
		for i := range byLevels {
			e := &byLevels[i]
			fmt.Fprintf(&b, "%d. %s — %d ур.%s\n", i+1, lbName(e), lbLevel(e), flame(e))
		}
	}
	b.WriteString("\nПо закреплённым вопросам 🟢:\n")
	if len(byGreen) == 0 {
		b.WriteString("Пока никого нет — пройди тест и стань первым!\n")
	} else {
		for i := range byGreen {
			e := &byGreen[i]
			fmt.Fprintf(&b, "%d. %s — %d🟢%s\n", i+1, lbName(e), e.Green, flame(e))
		}
	}
	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("⬅️ К таблице лидеров", cbLeaderboard)),
	}}
	h.editMessage(ctx, cb, b.String(), kb)
}
