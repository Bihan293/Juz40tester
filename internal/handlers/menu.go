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
// Предметы · Слабые темы / Свой тест · Статистика / Топ · Настройки
// (/ Подписка).
//
// It is sent ONLY with the main menu and is never removed or re-sent around
// a test: a ReplyKeyboardRemove (formerly sent when a test started) makes
// Telegram mobile clients replace the closed bot keyboard with the system
// text keyboard — the «keyboard pops up by itself when I open a test» bug —
// and re-sending the menu after every test popped the menu panel up over
// the result. Tests are answered with inline buttons only.
func mainMenuKeyboard() *bot.ReplyKeyboardMarkup {
	return mainMenuKeyboardFor(false)
}

// mainMenuKeyboardFor adds «⭐ Подписка» next to «⚙️ Настройки» when the
// subscriptions are on.
func mainMenuKeyboardFor(plans bool) *bot.ReplyKeyboardMarkup {
	if plans {
		return &bot.ReplyKeyboardMarkup{
			ResizeKeyboard: true,
			Keyboard: [][]bot.KeyboardButton{
				bot.ReplyRow(kbSubjects, kbWeak),
				bot.ReplyRow(kbCustom, kbProgress),
				bot.ReplyRow(kbTop, kbSettings),
				bot.ReplyRow(kbPlans),
			},
		}
	}
	return &bot.ReplyKeyboardMarkup{
		ResizeKeyboard: true,
		Keyboard: [][]bot.KeyboardButton{
			bot.ReplyRow(kbSubjects, kbWeak),
			bot.ReplyRow(kbCustom, kbProgress),
			bot.ReplyRow(kbTop, kbSettings),
		},
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
	if _, err := h.tg.SendMessage(ctx, chatID, text, h.menuKeyboard(chatID)); err != nil {
		logf("send main menu: %v", err)
	}
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
