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
)

// --- Subjects -----------------------------------------------------------------

func (h *Handler) renderSubjects(ctx context.Context, chatID int64) (string, *bot.InlineKeyboardMarkup, error) {
	subjects, err := h.quiz.ListSubjects(ctx)
	if err != nil {
		return "", nil, err
	}
	// Subject buttons — one per row so long names ("Математика",
	// "Грамотность чтения") fit and stay readable. Each subject gets its
	// own emoji to avoid confusion.
	if len(subjects) == 0 {
		// Fresh database: subjects are added by the administrator (no
		// built-in seed) — say so instead of an empty picker.
		return "📚 Предметов пока нет — скоро они появятся.",
			&bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu))}}, nil
	}
	buttons := make([]bot.InlineKeyboardButton, 0, len(subjects))
	for _, s := range subjects {
		buttons = append(buttons, bot.Btn(subjectEmoji(s.Name)+" "+s.Name, cbSubject+strconv.FormatInt(s.ID, 10)))
	}
	rows := bot.ChunkButtons(buttons, 1)
	// A way home from the subjects list: without it a user who left a test
	// via «📚 К предметам» had no path back to the main menu.
	rows = append(rows, bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)))
	return "📚 Выберите предмет:", &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// subjectEmoji picks a distinct emoji for a subject by its name so the
// subjects are easy to tell apart (falls back to 📚 for unknown names).
func subjectEmoji(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "математика", "математическая грамотность":
		return "📐"
	case "грамотность чтения", "чтение":
		return "📖"
	case "физика":
		return "⚛️"
	case "химия":
		return "🧪"
	case "биология":
		return "🧬"
	case "география":
		return "🌍"
	case "история", "история казахстана", "всемирная история":
		return "🏛️"
	case "казахский язык", "казахский язык и литература":
		return "🇰🇿"
	case "русский язык", "русский язык и литература":
		return "✏️"
	case "английский язык", "английский":
		return "🇬🇧"
	case "информатика":
		return "💻"
	default:
		return "📚"
	}
}

// showSubjects is used from the Reply Keyboard (new message).
func (h *Handler) showSubjects(ctx context.Context, chatID int64) {
	text, kb, err := h.renderSubjects(ctx, chatID)
	if err != nil {
		logf("list subjects: %v", err)
		if _, err := h.tg.SendMessage(ctx, chatID, "Ошибка загрузки предметов 😔", nil); err != nil {
			logf("send subjects error: %v", err)
		}
		return
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, kb); err != nil {
		logf("send subjects: %v", err)
	}
}

// editSubjects is used from an inline button (edits the current message).
func (h *Handler) editSubjects(ctx context.Context, cb *bot.CallbackQuery) {
	text, kb, err := h.renderSubjects(ctx, cb.Message.Chat.ID)
	if err != nil {
		logf("list subjects: %v", err)
		h.answerCallback(ctx, cb, "Ошибка загрузки предметов")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// renderSubject renders the tests grid of a subject: 3 buttons per row,
// paginated (⬅️/➡️), with 🔒 locked, ⏳ generating, ✅ completed and
// ⏸ resumable marks. The page is remembered per user.
func (h *Handler) renderSubject(ctx context.Context, user *models.User, subjectID int64, page int, remember bool) (string, *bot.InlineKeyboardMarkup, error) {
	scr, err := h.quiz.GetSubjectScreen(ctx, user.ID, subjectID, page, remember)
	if err != nil {
		return "", nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", subjectEmoji(scr.Subject.Name), scr.Subject.Name)
	fmt.Fprintf(&b, "Открыто тестов: %d · страница %d/%d\n\n", scr.UnlockedShown, scr.Page+1, scr.TotalPages)
	b.WriteString("Чтобы открыть следующий тест, доведи предыдущий до ")
	fmt.Fprintf(&b, "%d🟢 + %d🟡", models.UnlockGreen, models.UnlockYellow)
	b.WriteString(" — тогда я сразу начну его собирать ⏳")

	// Tests grid: 3 per row.
	buttons := make([]bot.InlineKeyboardButton, 0, len(scr.Slots))
	for _, slot := range scr.Slots {
		label := slot.Label
		data := ""
		switch {
		case slot.Test == nil && slot.Pending:
			label = "⏳ " + label
			// The button carries the coordinates of the missing test: tapping it
			// revives a failed/stuck generation job instead of just showing a toast.
			data = cbPendingID + strconv.FormatInt(subjectID, 10) + ":" + strconv.Itoa(slot.Number)
		case slot.Test == nil:
			label = "🔒 " + label
			data = cbNoop
		case !slot.Unlocked:
			label = "🔒 " + label
			data = cbOpenTest + strconv.FormatInt(slot.Test.ID, 10) // tap shows the requirement
		default:
			if slot.Completed {
				label = "✅ " + label
			} else if slot.Resume {
				label = "⏸ " + label
			}
			data = cbOpenTest + strconv.FormatInt(slot.Test.ID, 10)
		}
		buttons = append(buttons, bot.Btn(label, data))
	}
	rows := bot.ChunkButtons(buttons, models.TestsGridColumns)

	// Pagination row (only when there is more than one page).
	if scr.TotalPages > 1 {
		nav := make([]bot.InlineKeyboardButton, 0, 3)
		if scr.Page > 0 {
			nav = append(nav, bot.Btn("⬅️ Назад", fmt.Sprintf("%s%d:%d", cbSubjectPage, subjectID, scr.Page-1)))
		}
		nav = append(nav, bot.Btn(fmt.Sprintf("%d/%d", scr.Page+1, scr.TotalPages), cbNoop))
		if scr.Page < scr.TotalPages-1 {
			nav = append(nav, bot.Btn("Вперёд ➡️", fmt.Sprintf("%s%d:%d", cbSubjectPage, subjectID, scr.Page+1)))
		}
		rows = append(rows, nav)
	}

	rows = append(rows, bot.Row(
		bot.Btn("⬅️ К предметам", cbSubjects),
		bot.Btn("🏠 Главное меню", cbMainMenu),
	))
	return b.String(), &bot.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func (h *Handler) openSubject(ctx context.Context, cb *bot.CallbackQuery, user *models.User, subjectID int64) {
	// Restore the page where the user stopped last time.
	page, err := h.quiz.SavedTestsPage(ctx, user.ID, subjectID)
	if err != nil {
		logf("saved page %d/%d: %v", user.ID, subjectID, err)
		page = 0
	}
	text, kb, err := h.renderSubject(ctx, user, subjectID, page, false)
	if errors.Is(err, repositories.ErrNotFound) {
		h.answerCallback(ctx, cb, "Предмет не найден")
		return
	}
	if err != nil {
		logf("subject info %d: %v", subjectID, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки предмета")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}

// handlePendingTest handles a tap on a ⏳ (still generating) test button.
// It REVIVES the generation (a failed/stuck job is re-queued as URGENT) and,
// instead of a tiny toast that vanished after a few seconds, sends a clear
// chat message «⏳ Тест генерируется… никуда не уходи». A background watcher
// then edits that message into «✅ Тест готов» with a start button (or an
// honest error with a retry button) — the user is never left guessing.
func (h *Handler) handlePendingTest(ctx context.Context, cb *bot.CallbackQuery, user *models.User, data string) {
	parts := strings.Split(strings.TrimPrefix(data, cbPendingID), ":")
	if len(parts) != 2 {
		h.answerAlert(ctx, cb, "⏳ Тест ещё генерируется. Подожди минуточку и открой предмет снова.")
		return
	}
	subjectID, err1 := strconv.ParseInt(parts[0], 10, 64)
	testNumber, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		h.answerAlert(ctx, cb, "⏳ Тест ещё генерируется. Подожди минуточку и открой предмет снова.")
		return
	}
	// One pass (A5): the existing test, or revive/queue its generation.
	// The test may have been generated a moment ago — open it right away.
	if test, err := h.quiz.ChainTestOrRevive(ctx, subjectID, testNumber, user.ID); err == nil && test != nil {
		h.openTest(ctx, cb, user, test.ID)
		return
	}

	key := chainWatchKey(subjectID, testNumber)
	check := func(ctx context.Context) (*models.Test, bool, error) {
		return h.quiz.ChainTestStatus(ctx, subjectID, testNumber)
	}
	retryData := cbPendingID + strconv.FormatInt(subjectID, 10) + ":" + strconv.Itoa(testNumber)
	note := fmt.Sprintf("⏳ «Тест %d» сейчас генерируется…\n\nНикуда не уходи, подожди минуточку — как только тест будет готов, я сразу пришлю сюда кнопку, чтобы его начать 👇", testNumber)
	h.startGenerationWatch(ctx, cb, key, note, check, retryData)
}

// flipSubjectPage switches the tests-grid page and remembers it.
func (h *Handler) flipSubjectPage(ctx context.Context, cb *bot.CallbackQuery, user *models.User, data string) {
	parts := strings.Split(strings.TrimPrefix(data, cbSubjectPage), ":")
	if len(parts) != 2 {
		return
	}
	subjectID, err1 := strconv.ParseInt(parts[0], 10, 64)
	page, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return
	}
	text, kb, err := h.renderSubject(ctx, user, subjectID, page, true)
	if err != nil {
		logf("subject page %d/%d: %v", subjectID, page, err)
		h.answerCallback(ctx, cb, "Ошибка загрузки предмета")
		return
	}
	h.editMessage(ctx, cb, text, kb)
}
