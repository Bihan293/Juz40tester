package handlers

import (
	"context"
	"fmt"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
)

// Reply-keyboard (bottom of chat) button texts.
const (
	kbSubjects = "📚 Предметы"
	kbWeak     = "🎯 Слабые темы"
	kbProgress = "📊 Статистика"
	kbTop      = "🏆 Топ"
	kbSettings = "⚙️ Настройки"
)

// --- Main menu ---------------------------------------------------------------

// mainMenuKeyboard is the persistent Reply Keyboard at the bottom of the chat:
// Предметы · Слабые темы / Статистика · Топ / Настройки.
func mainMenuKeyboard() *bot.ReplyKeyboardMarkup {
	return &bot.ReplyKeyboardMarkup{
		ResizeKeyboard: true,
		Keyboard: [][]bot.KeyboardButton{
			bot.ReplyRow(kbSubjects, kbWeak),
			bot.ReplyRow(kbProgress, kbTop),
			bot.ReplyRow(kbSettings),
		},
	}
}

// hideReplyKeyboard sends a tiny message with ReplyKeyboardRemove so the
// bottom menu (Предметы / Слабые темы / Статистика / Настройки) disappears
// while a test is in progress and cannot distract or break the flow.
//
// R-2: fewer Telegram calls.
//   - When this process already hid the menu in this chat (and nothing has
//     shown it again since), the call is a no-op — 0 requests instead of 3.
//   - Otherwise the removal vehicle and the previous «menu is back» note
//     are deleted with ONE deleteMessages call (was 2 deleteMessage calls).
//
// The hidden state is per process (in memory): after a restart the state is
// unknown and the menu is hidden again the normal way, never skipped.
func (h *Handler) hideReplyKeyboard(ctx context.Context, chatID int64) {
	if v, ok := h.kbHidden.Load(chatID); ok && v.(bool) {
		return
	}
	msgID, err := h.tg.SendMessage(ctx, chatID, "✍️ Идёт тест — меню скрыто до конца. Вопросы ниже 👇", bot.RemoveKeyboard)
	if err != nil {
		logf("hide reply keyboard: %v", err)
		return
	}
	h.kbHidden.Store(chatID, true)
	// The message is only the keyboard-removal vehicle — delete it so the
	// chat stays clean (the keyboard stays hidden after the deletion),
	// together with the note that carried the menu: it is useless now.
	ids := []int64{msgID}
	if prev, ok := h.kbNotes.LoadAndDelete(chatID); ok {
		if old, ok := prev.(int64); ok && old != msgID {
			ids = append(ids, old)
		}
	}
	if err := h.tg.DeleteMessages(ctx, chatID, ids); err != nil {
		logf("delete keyboard-removal note(s): %v", err)
	}
}

// restoreReplyKeyboard brings the bottom main menu back once the test is
// over (finished or exited) — it was hidden while the test ran.
//
// Telegram clients drop a reply keyboard together with the message that
// carried it, so the note that restores the menu must stay in the chat
// while the menu is visible. To keep that from turning into spam (one
// «🏠 Главное меню…» line per finished test), only the LATEST note is
// kept: the previous one is deleted right after the new one is sent
// (deleting an OLDER message does not affect the keyboard of the newer
// one), and the current one is deleted when the next test hides the menu.
// In the normal test cycle the previous note was already removed by
// hideReplyKeyboard, so this is ONE Telegram call.
func (h *Handler) restoreReplyKeyboard(ctx context.Context, chatID int64) {
	msgID, err := h.tg.SendMessage(ctx, chatID, "🏠 Меню снова доступно 👇", mainMenuKeyboard())
	if err != nil {
		logf("restore reply keyboard: %v", err)
		return
	}
	h.kbHidden.Delete(chatID)
	if prev, loaded := h.kbNotes.Swap(chatID, msgID); loaded {
		if old, ok := prev.(int64); ok && old != msgID {
			if err := h.tg.DeleteMessage(ctx, chatID, old); err != nil {
				logf("delete previous menu note: %v", err)
			}
		}
	}
}

func (h *Handler) sendMainMenu(ctx context.Context, chatID int64, user *models.User, greet bool) {
	text := fmt.Sprintf("Главное меню JUZ40 Tester. %s\nВыберите раздел кнопками ниже 👇", streakBadge(user))
	if greet {
		name := user.FirstName
		if name == "" {
			name = user.Username
		}
		if name == "" {
			name = "друг"
		}
		text = fmt.Sprintf("👋 Привет, %s! %s\n\nДобро пожаловать в JUZ40 Tester — бот для подготовки к ЕНТ.\nВыберите раздел кнопками ниже 👇", name, streakBadge(user))
	}
	if _, err := h.tg.SendMessage(ctx, chatID, text, mainMenuKeyboard()); err != nil {
		logf("send main menu: %v", err)
		return
	}
	h.kbHidden.Delete(chatID) // the menu keyboard is visible again

}

// streakBadge renders the daily-streak flame for the main menu: from the
// second consecutive day the 🔥 appears with the day count; on day one a
// soft nudge invites the user to come back tomorrow to light it up.
func streakBadge(user *models.User) string {
	switch {
	case user == nil || user.StreakDays <= 0:
		return ""
	case user.StreakDays == 1:
		return "Зайди завтра — и загорится твой огонёк 🔥"
	default:
		return fmt.Sprintf("🔥 %d %s подряд!", user.StreakDays, dayWord(user.StreakDays))
	}
}

// dayWord returns the correct Russian plural of «день».
func dayWord(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 14:
		return "дней"
	}
	switch n % 10 {
	case 1:
		return "день"
	case 2, 3, 4:
		return "дня"
	default:
		return "дней"
	}
}
