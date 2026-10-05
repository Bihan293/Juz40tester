package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// --- Progress & settings ---------------------------------------------------------

// renderProgress renders the first statistics screen: the 🔥 streak line,
// a short per-subject summary and a subject picker (inline buttons one per
// row — long names stay readable). Detailed statistics of a subject are
// shown after the user picks it.
func (h *Handler) renderProgress(ctx context.Context, user *models.User) (string, *bot.InlineKeyboardMarkup, error) {
	subjects, err := h.quiz.ListSubjects(ctx)
	if err != nil {
		return "", nil, err
	}

	var b strings.Builder
	b.WriteString(streakLine(user))
	b.WriteString("\n\n📊 Ваша статистика по предметам:\n")
	shown := 0
	buttons := make([]bot.InlineKeyboardButton, 0, len(subjects))
	// All subjects' summaries in a fixed number of queries (was ~4 per subject).
	ids := make([]int64, 0, len(subjects))
	for _, s := range subjects {
		ids = append(ids, s.ID)
	}
	all, err := h.quiz.AllSubjectsProgress(ctx, user.ID, ids)
	if err != nil {
		logf("progress summary: %v", err)
		all = nil // keep the subject picker usable, just without summaries
	}
	for _, s := range subjects {
		buttons = append(buttons, bot.Btn(subjectEmoji(s.Name)+" "+s.Name, cbProgSubject+strconv.FormatInt(s.ID, 10)))
		sp := all[s.ID]
		if sp == nil {
			continue
		}
		if sp.Green+sp.Yellow == 0 && sp.CorrectCount+sp.WrongCount == 0 {
			continue // not started yet — keep the summary short
		}
		fmt.Fprintf(&b, "%s %s: 🟢 %d · 🟡 %d · 🔴 %d\n", subjectEmoji(s.Name), s.Name, sp.Green, sp.Yellow, sp.Red)
		shown++
	}
	if shown == 0 {
		b.WriteString("Пока пусто — пройди первый тест в «📚 Предметы».\n")
	}
	b.WriteString("\nВыберите предмет для подробной статистики:")

	rows := bot.ChunkButtons(buttons, 1)
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	return b.String(), &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// streakLine renders the 🔥 «огонёк» line of the statistics screens from the
// existing daily-streak counter (users.streak_days, maintained by
// UserRepository.Upsert on every message/tap — the user row passed here was
// refreshed by ensureUser for THIS update, so the number is current).
func streakLine(user *models.User) string {
	days := 0
	if user != nil {
		days = models.EffectiveStreak(user.StreakDays, user.LastActiveDate, models.StreakToday(time.Now()))
	}
	switch {
	case days <= 0:
		return "🔥 Огонёк пока не горит — занимайся каждый день, чтобы его зажечь!"
	case days == 1:
		return "🔥 У вас 1-й день огонька! Зайди завтра — и он разгорится."
	default:
		return fmt.Sprintf("🔥 У вас %d-й день огонька! (%d %s подряд)", days, days, dayWord(days))
	}
}

// renderSubjectProgress renders the knowledge statistics of a single subject.
func (h *Handler) renderSubjectProgress(ctx context.Context, user *models.User, subjectID int64) (string, *bot.InlineKeyboardMarkup, error) {
	sp, err := h.quiz.SubjectProgress(ctx, user.ID, subjectID)
	if err != nil {
		return "", nil, err
	}

	var b strings.Builder
	b.WriteString(streakLine(user))
	fmt.Fprintf(&b, "\n\n📊 Ваша статистика — %s\n\n", sp.SubjectName)
	fmt.Fprintf(&b, "🟢 Закреплено: %d\n", sp.Green)
	fmt.Fprintf(&b, "🟡 В процессе: %d\n", sp.Yellow)
	fmt.Fprintf(&b, "🔴 Требует повторения: %d\n", sp.Red)
	fmt.Fprintf(&b, "Всего вопросов: %d\n", sp.TotalQuestions)
	// The personal weak-topics test (and its questions) is training material,
	// not part of the course: its questions are excluded from the totals so a
	// 20-question personal test never inflates the counters (the 🔴 count
	// used to jump by the size of the personal test).
	b.WriteString("\n🎯 Персональный тест по слабым темам в общую статистику не входит.")

	kb := &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn("📚 К тестам предмета", cbSubject+strconv.FormatInt(subjectID, 10))),
		bot.Row(bot.Btn("⬅️ К выбору предмета", cbProgress)),
	}}
	return b.String(), kb, nil
}

// showSubjectProgress shows statistics of one subject (from the picker).
func (h *Handler) showSubjectProgress(ctx context.Context, cb *bot.CallbackQuery, user *models.User, subjectID int64) {
	text, kb, err := h.renderSubjectProgress(ctx, user, subjectID)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Предмет не найден")
		return
	}
	if err != nil {
		logf("subject progress %d: %v", subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки прогресса")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// showProgress is used from the Reply Keyboard (new message).
func (h *Handler) showProgress(ctx context.Context, chatID int64, user *models.User) {
	text, kb, err := h.renderProgress(ctx, user)
	if err != nil {
		logf("user progress: %v", err)
		if _, err := h.tg.SendMessage(ctx, chatID, "Ошибка загрузки прогресса 😔", nil); err != nil {
			logf("send progress error: %v", err)
		}
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		logf("send progress: %v", err)
	}
}

// editProgress is used from an inline button (edits the current message).
func (h *Handler) editProgress(ctx context.Context, cb *bot.CallbackQuery, user *models.User) {
	text, kb, err := h.renderProgress(ctx, user)
	if err != nil {
		logf("user progress: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки прогресса")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}
